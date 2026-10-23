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

	"github.com/lxc/incus-compose/shared"
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
func (r *Network) ensureACLs(ctx context.Context, resolved map[string]string) error {
	if !r.client.Global().HasExtension(shared.Incus75Extension) {
		return nil
	}

	net := r.State().IncusNetwork
	if net == nil {
		return ErrNotEnsured
	}

	if !net.Managed {
		return nil
	}

	acls := []incusApi.NetworkACLsPost{}
	if net.Type == "ovn" {
		acls = append(acls, r.defaultACL())
	}

	if r.Config.ACL != nil {
		if r.Config.ACL.Name == "" {
			return fmt.Errorf("network %q: ACL has no name", r.Name())
		}

		acl := *r.Config.ACL
		acl.Name = SanitizeIncusName(acl.Name, -1)
		if len(resolved) > 0 {
			acl.Ingress = slices.Clone(r.Config.ACL.Ingress)
			for i := range acl.Ingress {
				if sub, ok := resolved[acl.Ingress[i].Source]; ok {
					acl.Ingress[i].Source = sub
				}
			}

			acl.Egress = slices.Clone(r.Config.ACL.Egress)
			for i := range acl.Egress {
				if sub, ok := resolved[acl.Egress[i].Destination]; ok {
					acl.Egress[i].Destination = sub
				}
			}
		}
		acls = append(acls, acl)
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

		for _, key := range []string{"security.acls.default.ingress.action", "security.acls.default.egress.action"} {
			if val := r.Config.Extensions[key]; val != "" && put.Config[key] != val {
				put.Config[key] = val
				changed = true
			}
		}

		// The posture is a default, not a policy: a value the user set (through
		// x-incus, x-incus-compose.acl.default, or by hand) is left alone.
		if put.Config["security.acls.default.ingress.action"] == "" {
			put.Config["security.acls.default.ingress.action"] = "reject"
			changed = true
		}

		if put.Config["security.acls.default.egress.action"] == "" {
			put.Config["security.acls.default.egress.action"] = "allow"
			changed = true
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
		names = append(names, SanitizeIncusName(r.Config.ACL.Name, -1))
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
			delete(put.Config, "security.acls.default.ingress.action")
			delete(put.Config, "security.acls.default.egress.action")
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
	name = SanitizeIncusName(name, -1)

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

	name = SanitizeIncusName(name, -1)

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
func (r *Network) ensurePeers(ctx context.Context, extra ...incusApi.NetworkPeersPost) error {
	peers := append(slices.Clone(r.Config.Peers), extra...)
	if r.Type(ctx) != "ovn" || len(peers) == 0 {
		return nil
	}

	net := r.State().IncusNetwork
	if net != nil && !net.Managed {
		return nil
	}

	project := r.incusProject()
	var errs error

	for _, peer := range peers {
		err := r.ensurePeerOn(ctx, project, r.incusName, peer)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return errs
}

func (r *Network) ensurePeerOn(ctx context.Context, hostProject, hostNetwork string, peer incusApi.NetworkPeersPost) error {
	conn, err := r.client.GlobalConnection()
	if err != nil {
		return err
	}

	peer.Name = SanitizeIncusName(peer.Name, -1)

	err = retry.New(
		retry.Context(ctx),
		retry.Attempts(10),
		retry.Delay(100*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(isConcurrencyConflict),
	).Do(func() error {
		existing, getErr := conn.GetNetworkPeers(ctx, hostProject, hostNetwork)
		if getErr != nil {
			if incusApi.StatusErrorCheck(getErr, http.StatusNotFound) || strings.Contains(getErr.Error(), "does not support") {
				return nil
			}

			return getErr
		}

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

		if haveName != nil && haveTarget != nil && haveName.Name == haveTarget.Name && haveName.Status != incusApi.NetworkStatusErrored {
			return nil
		}

		if haveTarget != nil && (haveName == nil || haveName.Name != haveTarget.Name) {
			delErr := conn.DeleteNetworkPeer(ctx, hostProject, hostNetwork, haveTarget.Name)
			if delErr != nil && !incusApi.StatusErrorCheck(delErr, http.StatusNotFound) {
				return fmt.Errorf("removing conflicting network peer %q: %w", haveTarget.Name, delErr)
			}
		}

		if haveName != nil {
			// A peer of the same name pointing elsewhere (or in Errored state after
			// its target was recreated) is stale; replace it.
			delErr := conn.DeleteNetworkPeer(ctx, hostProject, hostNetwork, peer.Name)
			if delErr != nil && !incusApi.StatusErrorCheck(delErr, http.StatusNotFound) {
				return fmt.Errorf("replacing network peer %q: %w", peer.Name, delErr)
			}
		}

		createErr := conn.CreateNetworkPeer(ctx, hostProject, hostNetwork, peer)
		if createErr != nil && (incusApi.StatusErrorCheck(createErr, http.StatusConflict) || strings.Contains(createErr.Error(), "already exists")) {
			p, _, pErr := conn.GetNetworkPeer(ctx, hostProject, hostNetwork, peer.Name)
			if pErr == nil && p.TargetProject == peer.TargetProject && p.TargetNetwork == peer.TargetNetwork && p.Status != incusApi.NetworkStatusErrored {
				return nil
			}
		}

		return createErr
	})
	if err != nil {
		return fmt.Errorf("creating network peer %q: %w", peer.Name, err)
	}

	return nil
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

	recipName := SanitizePeerName(project, r.incusName)
	var errs error

	for _, peer := range existing {
		targetProject := peer.TargetProject
		targetNetwork := peer.TargetNetwork

		err := conn.DeleteNetworkPeer(ctx, project, r.incusName, peer.Name)
		if err != nil && !incusApi.StatusErrorCheck(err, http.StatusNotFound) && !strings.Contains(err.Error(), "does not support") {
			errs = errors.Join(errs, fmt.Errorf("deleting network peer %q: %w", peer.Name, err))
		}

		if targetProject != "" && targetNetwork != "" {
			recipErr := conn.DeleteNetworkPeer(ctx, targetProject, targetNetwork, recipName)
			if recipErr != nil &&
				!incusApi.StatusErrorCheck(recipErr, http.StatusNotFound) &&
				!strings.Contains(recipErr.Error(), "does not support") &&
				!strings.Contains(recipErr.Error(), "in use") {
				errs = errors.Join(errs, fmt.Errorf("deleting reciprocal network peer %q on %s/%s: %w", recipName, targetProject, targetNetwork, recipErr))
			}
		}
	}

	return errs
}
