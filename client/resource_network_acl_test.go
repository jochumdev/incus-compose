package client

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/shared"
)

func TestACLNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty", value: "", want: []string{}},
		{name: "one", value: "web", want: []string{"web"}},
		{name: "two", value: "web,db", want: []string{"web", "db"}},
		{name: "spaces and empties", value: " web , , db ", want: []string{"web", "db"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, aclNames(tt.value))
		})
	}
}

// TestNetworkACL pins that a network's ACL is created, attached to
// security.acls, and removed with the network.
func TestNetworkACL(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	c := newRandomTestClient(t, "net-acl-")

	acl := &incusApi.NetworkACLsPost{
		NetworkACLPost: incusApi.NetworkACLPost{Name: "web-acl"},
		NetworkACLPut: incusApi.NetworkACLPut{
			Ingress: []incusApi.NetworkACLRule{{
				Action:          "allow",
				State:           "enabled",
				Protocol:        "tcp",
				DestinationPort: "5432",
			}},
		},
	}

	res, err := c.Resource(KindNetwork, "acl-net", &NetworkConfig{ACL: acl})
	require.NoError(t, err)

	net, ok := res.(*Network)
	require.True(t, ok)
	require.NoError(t, RunAction(ctx, net, ActionEnsure, OptionCreate()))

	conn, err := c.Connection()
	require.NoError(t, err)

	got, _, err := conn.GetNetworkACL(ctx, net.incusProject(), "web-acl")
	require.NoError(t, err)
	require.Len(t, got.Ingress, 1)
	assert.Equal(t, "5432", got.Ingress[0].DestinationPort)

	nw, _, err := conn.GetNetwork(ctx, net.incusProject(), net.IncusName())
	require.NoError(t, err)
	assert.Contains(t, aclNames(nw.Config["security.acls"]), "web-acl", "the ACL must be attached")

	require.NoError(t, RunAction(ctx, net, ActionDelete))

	_, _, err = conn.GetNetworkACL(ctx, net.incusProject(), "web-acl")
	assert.True(t, incusApi.StatusErrorCheck(err, http.StatusNotFound), "the ACL must go with the network, got %v", err)
}

// TestNetworkDefaultACL pins the docker-compose-parity posture every OVN
// network gets: same-network reachable, nothing else in, outbound open.
func TestNetworkDefaultACL(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	gc, err := NewTestClient(testContext(t))
	require.NoError(t, err)

	ovn, err := gc.DetectOVN()
	require.NoError(t, err)

	if !ovn {
		t.Skip("no OVN on this server")
	}

	name := "net-default-acl-" + strings.ToLower(shared.RandString(12))

	c, err := gc.EnsureProject(name, EnsureProjectWithCreate(), EnsureProjectWithNetworkDriver("ovn"))
	require.NoError(t, err)
	require.NoError(t, c.Open())
	deleteProjectOnCleanup(t, gc, name)
	t.Cleanup(func() { _ = c.Done() })

	res, err := c.Resource(KindNetwork, "net", &NetworkConfig{Type: "ovn"})
	require.NoError(t, err)

	net, ok := res.(*Network)
	require.True(t, ok)
	require.NoError(t, RunAction(ctx, net, ActionEnsure, OptionCreate()))

	conn, err := c.Connection()
	require.NoError(t, err)

	acl, _, err := conn.GetNetworkACL(ctx, net.incusProject(), net.defaultACLName())
	require.NoError(t, err)
	require.Len(t, acl.Ingress, 1)
	assert.Equal(t, "@internal", acl.Ingress[0].Source)

	nw, _, err := conn.GetNetwork(ctx, net.incusProject(), net.IncusName())
	require.NoError(t, err)
	assert.Contains(t, aclNames(nw.Config["security.acls"]), net.defaultACLName())
	assert.Equal(t, "reject", nw.Config["security.acls.default.ingress.action"])
	assert.Equal(t, "allow", nw.Config["security.acls.default.egress.action"])
}

// TestNetworkPeer pins that a mutual peer relationship is created on both
// networks and removed with them. OVN only: bridge has no peers.
func TestNetworkPeer(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	gc, err := NewTestClient(testContext(t))
	require.NoError(t, err)

	ovn, err := gc.DetectOVN()
	require.NoError(t, err)

	if !ovn {
		t.Skip("no OVN on this server")
	}

	name := "net-peer-" + strings.ToLower(shared.RandString(12))

	c, err := gc.EnsureProject(name, EnsureProjectWithCreate(), EnsureProjectWithNetworkDriver("ovn"))
	require.NoError(t, err)
	require.NoError(t, c.Open())
	deleteProjectOnCleanup(t, gc, name)
	t.Cleanup(func() { _ = c.Done() })

	aRes, err := c.Resource(KindNetwork, "net-a", &NetworkConfig{Type: "ovn"})
	require.NoError(t, err)
	bRes, err := c.Resource(KindNetwork, "net-b", &NetworkConfig{Type: "ovn"})
	require.NoError(t, err)

	netA, ok := aRes.(*Network)
	require.True(t, ok)
	netB, ok := bRes.(*Network)
	require.True(t, ok)

	netA.Config.Peers = []incusApi.NetworkPeersPost{{
		Name:          "to-b",
		TargetProject: c.IncusProject(),
		TargetNetwork: netB.IncusName(),
	}}
	netB.Config.Peers = []incusApi.NetworkPeersPost{{
		Name:          "to-a",
		TargetProject: c.IncusProject(),
		TargetNetwork: netA.IncusName(),
	}}

	require.NoError(t, RunAction(ctx, netA, ActionEnsure, OptionCreate()))
	require.NoError(t, RunAction(ctx, netB, ActionEnsure, OptionCreate()))

	conn, err := c.Connection()
	require.NoError(t, err)

	peer, _, err := conn.GetNetworkPeer(ctx, netA.incusProject(), netA.IncusName(), "to-b")
	require.NoError(t, err)
	assert.Equal(t, incusApi.NetworkStatusCreated, peer.Status, "both sides must exist for the relationship to be created")

	require.NoError(t, RunAction(ctx, netB, ActionDelete))
	require.NoError(t, RunAction(ctx, netA, ActionDelete))

	_, _, err = conn.GetNetworkPeer(ctx, netA.incusProject(), netA.IncusName(), "to-b")
	assert.True(t, incusApi.StatusErrorCheck(err, http.StatusNotFound), "the peer must go with the network, got %v", err)
}

func TestIsConcurrencyConflict(t *testing.T) {
	t.Parallel()

	assert.False(t, isConcurrencyConflict(nil))
	assert.False(t, isConcurrencyConflict(errors.New("something else")))
	assert.True(t, isConcurrencyConflict(errors.New("referential integrity violation: table logical_switch")))
	assert.True(t, isConcurrencyConflict(errors.New("constraint violation: transaction causes multiple rows")))
	assert.True(t, isConcurrencyConflict(incusApi.StatusErrorf(http.StatusPreconditionFailed, "precondition failed")))
	assert.True(t, isConcurrencyConflict(incusApi.StatusErrorf(http.StatusConflict, "conflict")))
}

func TestNetworkACL_ConcurrentAttachAndDetach(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	gc, err := NewTestClient(testContext(t))
	require.NoError(t, err)

	ovn, err := gc.DetectOVN()
	require.NoError(t, err)

	if !ovn {
		t.Skip("no OVN on this server")
	}

	projectName := "net-acl-race-" + strings.ToLower(shared.RandString(12))

	c, err := gc.EnsureProject(projectName, EnsureProjectWithCreate(), EnsureProjectWithNetworkDriver("ovn"))
	require.NoError(t, err)
	require.NoError(t, c.Open())
	deleteProjectOnCleanup(t, gc, projectName)
	t.Cleanup(func() { _ = c.Done() })

	name := "net-race"

	baseRes, err := c.Resource(KindNetwork, name, &NetworkConfig{Type: "ovn"})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, baseRes, ActionEnsure, OptionCreate()))

	const workers = 4
	nets := make([]*Network, workers)
	for i := range nets {
		acl := &incusApi.NetworkACLsPost{
			NetworkACLPost: incusApi.NetworkACLPost{Name: fmt.Sprintf("worker-%d-acl", i)},
			NetworkACLPut: incusApi.NetworkACLPut{
				Ingress: []incusApi.NetworkACLRule{{
					Action: "allow",
					State:  "enabled",
					Source: "@internal",
				}},
			},
		}

		workerClient := c.Clone()
		r, err := workerClient.Resource(KindNetwork, name, &NetworkConfig{
			Type: "ovn",
			ACL:  acl,
		})
		require.NoError(t, err)
		net, ok := r.(*Network)
		require.True(t, ok)
		nets[i] = net
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})

	for i, net := range nets {
		wg.Add(1)
		go func(idx int, n *Network) {
			defer wg.Done()
			<-start
			errs[idx] = RunAction(ctx, n, ActionEnsure, OptionCreate())
		}(i, net)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "worker %d ensure failed", i)
	}

	conn, err := c.Connection()
	require.NoError(t, err)

	nw, _, err := conn.GetNetwork(ctx, nets[0].incusProject(), nets[0].IncusName())
	require.NoError(t, err)

	currentACLs := aclNames(nw.Config["security.acls"])
	for i := range nets {
		assert.Contains(t, currentACLs, fmt.Sprintf("worker-%d-acl", i))
	}

	startDetach := make(chan struct{})
	for i, net := range nets {
		wg.Add(1)
		go func(idx int, n *Network) {
			defer wg.Done()
			<-startDetach
			errs[idx] = n.RemoveACL(ctx, fmt.Sprintf("worker-%d-acl", idx))
		}(i, net)
	}

	close(startDetach)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "worker %d detach failed", i)
	}

	nw, _, err = conn.GetNetwork(ctx, nets[0].incusProject(), nets[0].IncusName())
	require.NoError(t, err)

	remainingACLs := aclNames(nw.Config["security.acls"])
	for i := range nets {
		assert.NotContains(t, remainingACLs, fmt.Sprintf("worker-%d-acl", i))
	}
}
