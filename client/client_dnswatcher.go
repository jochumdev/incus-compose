package client

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// dnsIPWaitTimeout bounds how long to wait for a freshly started instance to
// acquire its DHCP lease before recording its DNS address.
const dnsIPWaitTimeout = 10 * time.Second

// DNSmasqParse parses a raw.dnsmasq value into its three parts: address
// records as a service->[]IP map, cname records as a target->[]alias map,
// and any other lines (e.g. user-supplied raw.dnsmasq content) verbatim.
func DNSmasqParse(raw string) (map[string][]string, map[string][]string, string) {
	addresses := map[string][]string{}
	cnames := map[string][]string{}
	var extra strings.Builder

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "address=/") {
			rest := line[len("address=/"):]
			slash := strings.Index(rest, "/")
			if slash < 1 {
				continue
			}
			svc, ip := rest[:slash], rest[slash+1:]
			if ip != "" {
				addresses[svc] = append(addresses[svc], ip)
			}
		} else if strings.HasPrefix(line, "cname=") {
			rest := line[len("cname="):]
			parts := strings.Split(rest, ",")
			if len(parts) < 2 {
				continue
			}
			cnames[parts[len(parts)-1]] = parts[0 : len(parts)-1]
		} else {
			if len(line) > 1 {
				fmt.Fprintf(&extra, "%s\n", line)
			}
		}
	}
	return addresses, cnames, extra.String()
}

// dnsmasqRecords builds the raw.dnsmasq content: one "address" record per
// service IP, sorted by service name for deterministic output.
func dnsmasqRecords(serviceIPs map[string][]string) string {
	var b strings.Builder
	for _, service := range slices.Sorted(maps.Keys(serviceIPs)) {
		for _, ip := range serviceIPs[service] {
			fmt.Fprintf(&b, "address=/%s/%s\n", service, ip)
		}
	}
	return b.String()
}

// ErrDNSWatcher is used as wrapper to indicate an error happened in the DNSWatcher.
var ErrDNSWatcher = NewError("DNSWatcher")

// RegisterDNSWatcher wires service-name DNS records into the project's managed
// networks via the client lifecycle hooks. On each instance create/start/stop/
// delete it reads raw.dnsmasq from Incus, updates only the records for services
// seen in this run (identified via IncusName), preserves all other records, and
// writes back. Multiple projects can coexist in the same network without
// clobbering each other's records.
func (c *Client) RegisterDNSWatcher() error {
	networks := map[string]*Network{}
	instances := map[string]*Instance{}
	instanceIPs := map[string][]InterfaceIPs{}
	ownedSet := map[string]struct{}{} // dnsmasq keys this run owns
	lastRestart := time.Time{}
	mu := &sync.Mutex{}

	// This hook waits up to 5 seconds before starting an instance after the last restart of a dnsmasq.
	c.AddHookBefore(func(ctx context.Context, action Action, r Resource, args Options, err error) error {
		if action != ActionStart || r.Kind() != KindInstance {
			return err
		}

		// No need if there was no dnsmasq update.
		if lastRestart.IsZero() {
			return err
		}

		mu.Lock()
		elapsed := time.Since(lastRestart)
		mu.Unlock()

		if elapsed < time.Second {
			c.LogDebug("Waiting for DNSMasq to be ready", "resource", r, "time", time.Second-elapsed)
			time.Sleep(time.Second - elapsed)
		}

		return err
	})

	c.AddHookAfter(func(ctx context.Context, action Action, r Resource, _ Options, err error) error {
		if err != nil || !r.IsEnsured() {
			return err
		}

		mu.Lock()
		defer mu.Unlock()

		switch r.Kind() {
		case KindNetwork:
			net, ok := r.(*Network)
			if ok && action == ActionEnsure {
				networks[net.IncusName()] = net
				c.LogDebug("DNSWatcher network", "network", net.Name())
			}

		case KindInstance:
			inst, ok := r.(*Instance)
			if !ok {
				return ErrDNSWatcher.WithText("resource is not an *Instance")
			}

			// No need to do anything if service and incus name are the same or service name is empty.
			if inst.ServiceName() == inst.IncusName() || inst.ServiceName() == "" {
				return nil
			}

			svcKey := inst.ServiceName()
			ownedSet[svcKey] = struct{}{}

			changed := false
			switch action {
			case ActionEnsure:
				if !inst.Created() && inst.Running() {
					ips, ipErr := inst.WaitIPs(ctx, dnsIPWaitTimeout)
					if ipErr != nil {
						return ErrDNSWatcher.Wrap(ipErr)
					}

					instances[inst.IncusName()] = inst
					instanceIPs[inst.IncusName()] = ips

					changed = true
				}
			case ActionStart:
				ips, ipErr := inst.WaitIPs(ctx, dnsIPWaitTimeout)
				if ipErr != nil {
					return ErrDNSWatcher.Wrap(ipErr)
				}

				instances[inst.IncusName()] = inst
				instanceIPs[inst.IncusName()] = ips

				changed = true
			case ActionStop:
				delete(instances, inst.IncusName())
				delete(instanceIPs, inst.IncusName())

				changed = true
			default:
				// ActionDelete and ActionLog don't affect DNS registration.
			}

			if !changed {
				return nil
			}

			owned := make([]string, 0, len(ownedSet))
			for svc := range ownedSet {
				owned = append(owned, svc)
			}

			var errs error
			for _, network := range networks {
				servicesIPs := map[string][]string{}
				for instIncusName, iIPs := range instanceIPs {
					sName := instances[instIncusName].ServiceName()

					iPs := []string{}
					for _, ip := range iIPs {
						if ip.Network != network.IncusName() {
							continue
						}

						if len(ip.IPv4s) > 0 {
							iPs = append(iPs, ip.IPv4s...)
						}

						if len(ip.IPv6s) > 0 {
							iPs = append(iPs, ip.IPv6s...)
						}
					}

					if len(iPs) > 0 {
						// Aggregate by service name so every replica of a
						// service is registered under one DNS record.
						servicesIPs[sName] = append(servicesIPs[sName], iPs...)
					}
				}

				err = network.updateDNSAliases(ctx, owned, servicesIPs)
				if err != nil && strings.Contains(err.Error(), "ETag doesn't match") {
					// Try a second time.
					time.Sleep(100 * time.Millisecond)
					err = network.updateDNSAliases(ctx, owned, servicesIPs)
				}

				errs = errors.Join(errs, err)

				lastRestart = time.Now()
			}

			err = errors.Join(err, errs)
			if err != nil {
				return ErrDNSWatcher.Wrap(err)
			}

			return nil

		default:
			// Other kinds are not relevant to DNS watching.
		}

		return nil
	})

	return nil
}
