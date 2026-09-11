package client

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	incusApi "github.com/lxc/incus/v7/shared/api"
)

// Subnets returns the network's address CIDRs, read from Incus.
func (r *Network) Subnets(ctx context.Context) ([]string, error) {
	err := r.get(ctx)
	if err != nil {
		return nil, err
	}

	net := r.State().IncusNetwork
	out := []string{}

	for _, key := range []string{"ipv4.address", "ipv6.address"} {
		value := net.Config[key]
		if value == "" || value == "none" {
			continue
		}

		cidr, _, _ := strings.Cut(value, ",")
		out = append(out, strings.TrimSpace(cidr))
	}

	return out, nil
}

// Type returns the network's Incus type, read once and cached, falling back to
// the requested type when the network does not exist yet.
func (r *Network) Type(ctx context.Context) string {
	net := r.State().IncusNetwork
	if net != nil && net.Type != "" {
		return net.Type
	}

	typ, err := r.client.globalClient.NetworkType(ctx, r.incusProject(), r.incusName)
	if err != nil {
		if r.Config.Type != "" {
			return r.Config.Type
		}

		return "bridge"
	}

	return typ
}

// aclNames splits a security.acls value into its names.
func aclNames(value string) []string {
	names := []string{}

	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}

	return names
}

// defaultACLName is the ACL carrying the docker-compose-parity posture.
func (r *Network) defaultACLName() string {
	return r.incusName + "-default"
}

// defaultACL is the posture every OVN network gets: instances on the same
// network reach each other, nothing else may initiate in, outbound is open.
func (r *Network) defaultACL() incusApi.NetworkACLsPost {
	return incusApi.NetworkACLsPost{
		NetworkACLPost: incusApi.NetworkACLPost{Name: r.defaultACLName()},
		NetworkACLPut: incusApi.NetworkACLPut{
			Ingress: []incusApi.NetworkACLRule{{
				Action: "allow",
				State:  "enabled",
				Source: "@internal",
			}},
		},
	}
}

// ensureACLs creates or updates the network's ACLs and attaches them. An OVN
// network always gets the default posture; Config.ACL adds the caller's rules.
func (r *Network) ensureACLs(ctx context.Context) error {
	net := r.State().IncusNetwork
	if net == nil {
		return ErrNotEnsured
	}

	acls := []incusApi.NetworkACLsPost{}
	if net.Type == "ovn" {
		acls = append(acls, r.defaultACL())
	}

	if r.Config.ACL != nil {
		if r.Config.ACL.Name == "" {
			return fmt.Errorf("network %q: ACL has no name", r.Name())
		}

		acls = append(acls, *r.Config.ACL)
	}

	if len(acls) == 0 {
		return nil
	}

	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	project := r.incusProject()

	for _, acl := range acls {
		err = retry.New(
			retry.Context(ctx),
			retry.Attempts(10),
			retry.Delay(100*time.Millisecond),
			retry.DelayType(retry.FixedDelay),
			retry.LastErrorOnly(true),
			retry.RetryIf(isConcurrencyConflict),
		).Do(func() error {
			_, etag, getErr := conn.GetNetworkACL(ctx, project, acl.Name)
			if getErr != nil {
				if !incusApi.StatusErrorCheck(getErr, http.StatusNotFound) {
					return fmt.Errorf("getting network ACL %q: %w", acl.Name, getErr)
				}

				createErr := conn.CreateNetworkACL(ctx, project, acl)
				if createErr != nil && (incusApi.StatusErrorCheck(createErr, http.StatusConflict) || strings.Contains(createErr.Error(), "already exists")) {
					_, etag, getErr = conn.GetNetworkACL(ctx, project, acl.Name)
					if getErr != nil {
						return getErr
					}

					return conn.UpdateNetworkACL(ctx, project, acl.Name, acl.NetworkACLPut, etag)
				}

				return createErr
			}

			return conn.UpdateNetworkACL(ctx, project, acl.Name, acl.NetworkACLPut, etag)
		})
		if err != nil {
			return fmt.Errorf("ensuring network ACL %q: %w", acl.Name, err)
		}
	}

	err = retry.New(
		retry.Context(ctx),
		retry.Attempts(10),
		retry.Delay(100*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(isConcurrencyConflict),
	).Do(func() error {
		getErr := r.get(ctx)
		if getErr != nil {
			return getErr
		}

		currentNet := r.State().IncusNetwork
		if currentNet == nil {
			return ErrNotEnsured
		}

		names := aclNames(currentNet.Config["security.acls"])
		changed := false

		for _, acl := range acls {
			if !slices.Contains(names, acl.Name) {
				names = append(names, acl.Name)
				changed = true
			}
		}

		put := currentNet.Writable()
		put.Config = maps.Clone(currentNet.Config)
		if put.Config == nil {
			put.Config = map[string]string{}
		}

		// The posture is a default, not a policy: a value the user set (through
		// x-incus or by hand) is left alone.
		if currentNet.Type == "ovn" {
			if put.Config["security.acls.default.ingress.action"] == "" {
				put.Config["security.acls.default.ingress.action"] = "reject"
				changed = true
			}

			if put.Config["security.acls.default.egress.action"] == "" {
				put.Config["security.acls.default.egress.action"] = "allow"
				changed = true
			}
		}

		if !changed {
			return nil
		}

		put.Config["security.acls"] = strings.Join(names, ",")

		updateErr := conn.UpdateNetwork(ctx, project, r.incusName, put, r.State().ETag)
		if updateErr != nil {
			return updateErr
		}

		getErr = r.get(ctx)
		if getErr != nil {
			return getErr
		}

		if r.State().IncusNetwork != nil {
			freshNames := aclNames(r.State().IncusNetwork.Config["security.acls"])
			for _, acl := range acls {
				if !slices.Contains(freshNames, acl.Name) {
					return incusApi.StatusErrorf(http.StatusPreconditionFailed, "ACL %q not in network %q after update", acl.Name, r.Name())
				}
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("attaching ACLs to network %q: %w", r.Name(), err)
	}

	return nil
}

// deleteACLs detaches the network's ACLs and deletes them.
func (r *Network) deleteACLs(ctx context.Context) error {
	net := r.State().IncusNetwork
	if net == nil {
		return nil
	}

	names := []string{}
	if net.Type == "ovn" {
		names = append(names, r.defaultACLName())
	}

	if r.Config.ACL != nil && r.Config.ACL.Name != "" {
		names = append(names, r.Config.ACL.Name)
	}

	if len(names) == 0 {
		return nil
	}

	err := r.detachACLs(ctx, names)
	if err != nil {
		return err
	}

	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	project := r.incusProject()
	var errs error

	for _, name := range names {
		err := conn.DeleteNetworkACL(ctx, project, name)
		if err != nil && !incusApi.StatusErrorCheck(err, http.StatusNotFound) {
			errs = errors.Join(errs, fmt.Errorf("deleting network ACL %q: %w", name, err))
		}
	}

	return errs
}

// detachACLs removes names from the network's security.acls.
func (r *Network) detachACLs(ctx context.Context, names []string) error {
	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	err = retry.New(
		retry.Context(ctx),
		retry.Attempts(10),
		retry.Delay(100*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(isConcurrencyConflict),
	).Do(func() error {
		getErr := r.get(ctx)
		if getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				return nil
			}

			return getErr
		}

		currentNet := r.State().IncusNetwork
		if currentNet == nil {
			return nil
		}

		current := aclNames(currentNet.Config["security.acls"])
		rest := slices.DeleteFunc(slices.Clone(current), func(n string) bool { return slices.Contains(names, n) })
		if len(rest) == len(current) {
			return nil
		}

		put := currentNet.Writable()
		put.Config = maps.Clone(currentNet.Config)
		if put.Config == nil {
			put.Config = map[string]string{}
		}

		if len(rest) == 0 {
			delete(put.Config, "security.acls")
		} else {
			put.Config["security.acls"] = strings.Join(rest, ", ")
		}

		updateErr := conn.UpdateNetwork(ctx, r.incusProject(), r.incusName, put, r.State().ETag)
		if updateErr != nil {
			return updateErr
		}

		checkErr := r.get(ctx)
		if checkErr != nil {
			if errors.Is(checkErr, ErrNotFound) {
				return nil
			}

			return checkErr
		}

		if r.State().IncusNetwork != nil {
			freshNames := aclNames(r.State().IncusNetwork.Config["security.acls"])
			for _, name := range names {
				if slices.Contains(freshNames, name) {
					return incusApi.StatusErrorf(http.StatusPreconditionFailed, "network %q still contains ACL %q after detach", r.incusName, name)
				}
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("detaching network ACLs: %w", err)
	}

	return nil
}

// RemoveACL deletes the named ACL. If the ACL is still in use by this
// network, detachACLs is called to remove it before deletion.
func (r *Network) RemoveACL(ctx context.Context, name string) error {
	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	err = r.detachACLs(ctx, []string{name})
	if err != nil {
		return err
	}

	err = retry.New(
		retry.Context(ctx),
		retry.Attempts(10),
		retry.Delay(100*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(func(err error) bool {
			return strings.Contains(err.Error(), "in use") || isConcurrencyConflict(err)
		}),
	).Do(func() error {
		delErr := conn.DeleteNetworkACL(ctx, r.incusProject(), name)
		if delErr != nil && !incusApi.StatusErrorCheck(delErr, http.StatusNotFound) {
			if strings.Contains(delErr.Error(), "in use") {
				_ = r.detachACLs(ctx, []string{name})
			}

			return delErr
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("deleting network ACL %q: %w", name, err)
	}

	return nil
}

// RemovePeer removes one peer from the network, leaving the rest.
func (r *Network) RemovePeer(ctx context.Context, name string) error {
	if r.Type(ctx) != "ovn" {
		return nil
	}

	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	err = conn.DeleteNetworkPeer(ctx, r.incusProject(), r.incusName, name)
	if err != nil && (incusApi.StatusErrorCheck(err, http.StatusNotFound) || strings.Contains(err.Error(), "does not support")) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting network peer %q: %w", name, err)
	}

	return nil
}

// ensurePeers creates each configured peer relationship on the network. A peer
// whose other side does not exist yet stays pending until it does.
func (r *Network) ensurePeers(ctx context.Context) error {
	if r.Type(ctx) != "ovn" || len(r.Config.Peers) == 0 {
		return nil
	}

	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	project := r.incusProject()

	existing, err := conn.GetNetworkPeers(ctx, project, r.incusName)
	if err != nil {
		if strings.Contains(err.Error(), "does not support") {
			return nil
		}

		return err
	}

	var errs error

	for _, peer := range r.Config.Peers {
		var haveName *incusApi.NetworkPeer
		var haveTarget *incusApi.NetworkPeer

		for i := range existing {
			if existing[i].Name == peer.Name {
				haveName = &existing[i]
			}
			if existing[i].TargetProject == peer.TargetProject && existing[i].TargetNetwork == peer.TargetNetwork {
				haveTarget = &existing[i]
			}
		}

		if haveName != nil && haveTarget != nil && haveName.Name == haveTarget.Name {
			continue
		}

		if haveTarget != nil && (haveName == nil || haveName.Name != haveTarget.Name) {
			err := conn.DeleteNetworkPeer(ctx, project, r.incusName, haveTarget.Name)
			if err != nil && !incusApi.StatusErrorCheck(err, http.StatusNotFound) {
				errs = errors.Join(errs, fmt.Errorf("removing conflicting network peer %q: %w", haveTarget.Name, err))

				continue
			}
			existing = slices.DeleteFunc(existing, func(p incusApi.NetworkPeer) bool {
				return p.Name == haveTarget.Name
			})
		}

		if haveName != nil {
			// A peer of the same name pointing elsewhere is stale (its target
			// network was recreated); replace it.
			err := conn.DeleteNetworkPeer(ctx, project, r.incusName, peer.Name)
			if err != nil && !incusApi.StatusErrorCheck(err, http.StatusNotFound) {
				errs = errors.Join(errs, fmt.Errorf("replacing network peer %q: %w", peer.Name, err))

				continue
			}
			existing = slices.DeleteFunc(existing, func(p incusApi.NetworkPeer) bool {
				return p.Name == peer.Name
			})
		}

		err = conn.CreateNetworkPeer(ctx, project, r.incusName, peer)
		if err != nil && (incusApi.StatusErrorCheck(err, http.StatusConflict) || strings.Contains(err.Error(), "already exists")) {
			p, _, getErr := conn.GetNetworkPeer(ctx, project, r.incusName, peer.Name)
			if getErr == nil && p.TargetProject == peer.TargetProject && p.TargetNetwork == peer.TargetNetwork {
				err = nil
			}
		}
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("creating network peer %q: %w", peer.Name, err))
		} else {
			existing = append(existing, incusApi.NetworkPeer{
				Name:          peer.Name,
				TargetProject: peer.TargetProject,
				TargetNetwork: peer.TargetNetwork,
			})
		}
	}

	return errs
}

// deletePeers removes each configured peer relationship from the network.
func (r *Network) deletePeers(ctx context.Context) error {
	if r.Type(ctx) != "ovn" {
		return nil
	}

	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	project := r.incusProject()

	existing, err := conn.GetNetworkPeers(ctx, project, r.incusName)
	if err != nil {
		if incusApi.StatusErrorCheck(err, http.StatusNotFound) || strings.Contains(err.Error(), "does not support") {
			return nil
		}

		return err
	}

	var errs error

	for _, peer := range existing {
		err := conn.DeleteNetworkPeer(ctx, project, r.incusName, peer.Name)
		if err != nil && !incusApi.StatusErrorCheck(err, http.StatusNotFound) && !strings.Contains(err.Error(), "does not support") {
			errs = errors.Join(errs, fmt.Errorf("deleting network peer %q: %w", peer.Name, err))
		}
	}

	return errs
}
