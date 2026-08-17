package client

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// ----------------------------------------------------------------------------
// Unit Tests for sanitizeNetworkName
// ----------------------------------------------------------------------------

func TestNetworkNameForProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		projectName string
		prefix      string
		networkName string
		description string
	}{
		{
			name:        "short name no project",
			projectName: "",
			prefix:      "",
			networkName: "web",
			description: "Short names should pass through",
		},
		{
			name:        "short name with project",
			projectName: "test",
			prefix:      "",
			networkName: "web",
			description: "test-web should fit in 13 chars",
		},
		{
			name:        "long name gets hashed",
			projectName: "verylongproject",
			prefix:      "ic-",
			networkName: "verylongnetwork",
			description: "Long names should get ic-{hash10} format",
		},
		{
			name:        "underscore replacement",
			projectName: "my_project",
			prefix:      "",
			networkName: "my_net",
			description: "Underscores should become hyphens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := SanitizeNetworkName(tt.projectName, tt.prefix, tt.networkName)

			require.LessOrEqual(t, len(result), maxInterfaceNameLen, "Network name too long")

			if len(tt.projectName+"-"+tt.networkName) > maxInterfaceNameLen && tt.prefix != "" {
				require.True(t, strings.HasPrefix(result, tt.prefix))
				require.Len(t, result, len(tt.prefix)+networkNameHashLen)
			}

			require.NotContains(t, result, "_")

			result2 := SanitizeNetworkName(tt.projectName, tt.prefix, tt.networkName)
			require.Equal(t, result, result2, "Network name generation should be deterministic")
		})
	}

	p1n1 := SanitizeNetworkName("p1", "ic-", "n1")
	p2n1 := SanitizeNetworkName("p2", "ic-", "n1")
	require.NotEqual(t, p1n1, p2n1, "Same name different projects should give different names")
}

func TestNetworkCreateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		extensions map[string]string
		want       map[string]string
	}{
		{
			name: "empty",
		},
		{
			name:       "auto addresses are left to incus",
			extensions: map[string]string{"ipv4.address": "auto", "ipv6.address": "auto"},
			want:       map[string]string{"ipv4.address": "auto", "ipv6.address": "auto"},
		},
		{
			name:       "explicit IPv4 gets DHCP range",
			extensions: map[string]string{"ipv4.address": "10.200.0.1/24"},
			want:       map[string]string{"ipv4.address": "10.200.0.1/24", "ipv4.dhcp.ranges": "10.200.0.64-10.200.0.254"},
		},
		{
			name:       "explicit IPv6 gets DHCP range and stateful DHCP",
			extensions: map[string]string{"ipv6.address": "fd42:1::1/64"},
			want: map[string]string{
				"ipv6.address":       "fd42:1::1/64",
				"ipv6.dhcp.ranges":   "fd42:1::100-fd42:1::ffff",
				"ipv6.dhcp.stateful": "true",
			},
		},
		{
			name: "existing ranges are preserved",
			extensions: map[string]string{
				"ipv4.address":     "10.200.0.1/24",
				"ipv4.dhcp.ranges": "10.200.0.100-10.200.0.200",
			},
			want: map[string]string{
				"ipv4.address":     "10.200.0.1/24",
				"ipv4.dhcp.ranges": "10.200.0.100-10.200.0.200",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := networkCreateConfig(tt.extensions)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestDNSmasqRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		serviceIPs map[string][]string
		expected   string
	}{
		{
			name:       "empty",
			serviceIPs: map[string][]string{},
			expected:   "",
		},
		{
			name:       "single service single ip",
			serviceIPs: map[string][]string{"database": {"10.0.0.2"}},
			expected:   "address=/database/10.0.0.2\n",
		},
		{
			name:       "single service round-robin",
			serviceIPs: map[string][]string{"web": {"10.0.0.3", "10.0.0.4"}},
			expected:   "address=/web/10.0.0.3\naddress=/web/10.0.0.4\n",
		},
		{
			name:       "multiple services sorted",
			serviceIPs: map[string][]string{"web": {"10.0.0.3"}, "app": {"10.0.0.5"}},
			expected:   "address=/app/10.0.0.5\naddress=/web/10.0.0.3\n",
		},
		{
			name:       "dual stack",
			serviceIPs: map[string][]string{"db": {"10.0.0.2", "fd42::2"}},
			expected:   "address=/db/10.0.0.2\naddress=/db/fd42::2\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, dnsmasqRecords(tt.serviceIPs))
		})
	}
}

// ----------------------------------------------------------------------------
// candidateNames Unit Tests (offline, no Incus required)
// ----------------------------------------------------------------------------

func TestCandidateNames_WithOverride(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "myproject")
	r, err := c.Resource(KindNetwork, "shared", &NetworkConfig{
		External:     true,
		OverrideName: "my-production-net",
	})
	require.NoError(t, err)
	net, ok := r.(*Network)
	require.True(t, ok)

	candidates := net.candidateNames()
	require.Len(t, candidates, 4)
	require.Equal(t, "my-production-net", candidates[0], "override raw first")
	require.True(t, strings.HasPrefix(candidates[1], "ic-"), "override sanitized second")
	require.Equal(t, "shared", candidates[2], "compose name raw third")
	require.True(t, strings.HasPrefix(candidates[3], "ic-"), "compose name sanitized fourth")
}

func TestCandidateNames_WithoutOverride(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "myproject")
	r, err := c.Resource(KindNetwork, "shared", &NetworkConfig{External: true})
	require.NoError(t, err)
	net, ok := r.(*Network)
	require.True(t, ok)

	candidates := net.candidateNames()
	require.Len(t, candidates, 2)
	require.Equal(t, "shared", candidates[0], "compose name raw first")
	require.True(t, strings.HasPrefix(candidates[1], "ic-"), "compose name sanitized second")
}

func TestCandidateNames_DeduplicatesShortName(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "")
	r, err := c.Resource(KindNetwork, "mynet", &NetworkConfig{External: true})
	require.NoError(t, err)
	net, ok := r.(*Network)
	require.True(t, ok)

	candidates := net.candidateNames()
	require.Len(t, candidates, 1, "raw and sanitized are the same — should deduplicate")
	require.Equal(t, "mynet", candidates[0])
}

// ----------------------------------------------------------------------------
// Local-only Tests (no Incus required)
// ----------------------------------------------------------------------------

func TestNetworkResource_ReturnsSameInstance(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "network-test")

	r1, err := c.Resource(KindNetwork, "test-same", &NetworkConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindNetwork, "test-same", &NetworkConfig{})
	require.NoError(t, err)

	require.Same(t, r1, r2)
}

func TestNetworkResource_DifferentNamesAreDifferent(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "network-test")

	r1, err := c.Resource(KindNetwork, "network-a", &NetworkConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindNetwork, "network-b", &NetworkConfig{})
	require.NoError(t, err)

	require.NotSame(t, r1, r2)
}

func TestNetworkIncusName_ShortName(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "network-test")

	r, err := c.Resource(KindNetwork, "web", &NetworkConfig{})
	require.NoError(t, err)

	network, ok := r.(*Network)
	require.True(t, ok)
	require.LessOrEqual(t, len(network.IncusName()), maxInterfaceNameLen)
}

func TestNetworkIncusName_LongNameHashed(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "network-test")

	r, err := c.Resource(KindNetwork, "very-long-network-name", &NetworkConfig{})
	require.NoError(t, err)

	network, ok := r.(*Network)
	require.True(t, ok)
	require.LessOrEqual(t, len(network.IncusName()), maxInterfaceNameLen)
	require.True(t, strings.HasPrefix(network.IncusName(), "ic-"))
}

func TestNetworkIncusName_Deterministic(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "network-test")

	r1, err := c.Resource(KindNetwork, "det-test", &NetworkConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindNetwork, "det-test", &NetworkConfig{})
	require.NoError(t, err)

	network1, ok := r1.(*Network)
	require.True(t, ok)
	network2, ok := r2.(*Network)
	require.True(t, ok)
	require.Equal(t, network1.IncusName(), network2.IncusName())
}

func TestNetworkExternal_InitialIncusNameIsRaw(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "network-test")

	r, err := c.Resource(KindNetwork, "incusbr0", &NetworkConfig{External: true})
	require.NoError(t, err)

	network, ok := r.(*Network)
	require.True(t, ok)
	require.Equal(t, "incusbr0", network.IncusName())
}

// ----------------------------------------------------------------------------
// Ensure Tests
// ----------------------------------------------------------------------------

func TestNetworkEnsure(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name     string
		network  string
		config   *NetworkConfig
		opts     []Option
		wantErr  bool
		validate func(*testing.T, Resource)
	}{
		{
			name:    "with create",
			network: "test-net",
			config:  &NetworkConfig{},
			opts:    []Option{OptionCreate()},
		},
		{
			name:    "without create fails",
			network: "non-existent",
			config:  &NetworkConfig{},
			wantErr: true,
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				require.False(t, r.IsEnsured())
			},
		},
		{
			name:    "default type is bridge",
			network: "test-default-type",
			config:  &NetworkConfig{},
			opts:    []Option{OptionCreate()},
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				network, ok := r.(*Network)
				require.True(t, ok)
				require.NotNil(t, network.State().IncusNetwork)
				require.Equal(t, "bridge", network.State().IncusNetwork.Type)
			},
		},
		{
			name:    "custom type",
			network: "test-custom-type",
			config:  &NetworkConfig{Type: "bridge"},
			opts:    []Option{OptionCreate()},
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				network, ok := r.(*Network)
				require.True(t, ok)
				require.Equal(t, "bridge", network.State().IncusNetwork.Type)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "network-ensure-")

			r, err := c.Resource(KindNetwork, tt.network, tt.config)
			require.NoError(t, err)

			err = RunAction(ctx, r, ActionEnsure, tt.opts...)
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrNotFound)
			} else {
				require.NoError(t, err)
				require.True(t, r.IsEnsured())
			}
			if tt.validate != nil {
				tt.validate(t, r)
			}
		})
	}
}

func TestNetworkEnsure_Idempotent(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "network-idempotent-")

	r, err := c.Resource(KindNetwork, "test-idempotent", &NetworkConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())
}

func TestNetworkEnsure_WithoutCreate_ThenWithCreate(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "network-retry-")

	r, err := c.Resource(KindNetwork, "test-retry", &NetworkConfig{})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure)
	require.Error(t, err)
	require.False(t, r.IsEnsured())

	err = RunAction(ctx, r, ActionEnsure, OptionCreate())
	require.NoError(t, err)
	require.True(t, r.IsEnsured())
}

func TestNetworkEnsure_ExistsOnNewClient(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "network-persist-")

	r, err := c.Resource(KindNetwork, "test-persist", &NetworkConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	newClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	r2, err := newClient.Resource(KindNetwork, "test-persist", &NetworkConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r2, ActionEnsure))
	require.True(t, r2.IsEnsured())
}

func TestNetworkProjectDeletesNetwork(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "network-projdel-")

	r, err := c.Resource(KindNetwork, "test-project-net", &NetworkConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	require.NoError(t, c.globalClient.DeleteProject(c.project, true))

	newC, err := createProjectClient(c.globalClient, c.project)
	require.NoError(t, err)

	r, err = newC.Resource(KindNetwork, "test-project-net", &NetworkConfig{})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure)
	require.Error(t, err, "network should be gone after project deletion")
	require.False(t, r.IsEnsured())
}

// ----------------------------------------------------------------------------
// Delete Tests
// ----------------------------------------------------------------------------

func TestNetworkDelete(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name    string
		ensure  bool
		wantErr bool
	}{
		{
			name:   "after ensure",
			ensure: true,
		},
		{
			name:    "not ensured error",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "network-delete-")

			r, err := c.Resource(KindNetwork, "test-delete", &NetworkConfig{})
			require.NoError(t, err)

			if tt.ensure {
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, r.IsEnsured())
			}

			err = RunAction(ctx, r, ActionDelete, OptionForce())
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.False(t, r.IsEnsured())
		})
	}
}

// ----------------------------------------------------------------------------
// Hook Tests
// ----------------------------------------------------------------------------

func TestNetworkHooks(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name string
		run  func(*testing.T, *Client)
	}{
		{
			name: "before is called",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				called := false
				c.AddHookBefore(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
					if action == ActionEnsure && r.Kind() == KindNetwork {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindNetwork, "test-before-hook", &NetworkConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, called, "before hook should have been called")
			},
		},
		{
			name: "after is called",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				called := false
				c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
					if action == ActionEnsure && r.Kind() == KindNetwork {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindNetwork, "test-after-hook", &NetworkConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, called, "after hook should have been called")
			},
		},
		{
			name: "after receives error",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				var receivedErr error
				c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
					if action == ActionEnsure && r.Kind() == KindNetwork {
						receivedErr = err
					}
					return err
				})
				r, err := c.Resource(KindNetwork, "non-existent", &NetworkConfig{})
				require.NoError(t, err)
				_ = RunAction(ctx, r, ActionEnsure)
				require.NotNil(t, receivedErr, "after hook should receive the error")
			},
		},
		{
			name: "before can abort",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				c.AddHookBefore(func(_ context.Context, _ Action, r Resource, _ Options, err error) error {
					if r.Name() == "abort-me" {
						return ErrAborted
					}
					return err
				})
				r, err := c.Resource(KindNetwork, "abort-me", &NetworkConfig{})
				require.NoError(t, err)
				err = RunAction(ctx, r, ActionEnsure, OptionCreate())
				require.ErrorIs(t, err, ErrAborted)
				require.False(t, r.IsEnsured())
			},
		},
		{
			name: "after can modify error",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				c.AddHookAfter(func(_ context.Context, _ Action, _ Resource, _ Options, err error) error {
					if err != nil {
						return ErrAborted
					}
					return nil
				})
				r, err := c.Resource(KindNetwork, "non-existent", &NetworkConfig{})
				require.NoError(t, err)
				err = RunAction(ctx, r, ActionEnsure)
				require.ErrorIs(t, err, ErrAborted)
			},
		},
		{
			name: "delete action",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				var lastAction Action
				c.AddHookBefore(func(_ context.Context, a Action, _ Resource, _ Options, err error) error {
					lastAction = a
					return err
				})
				r, err := c.Resource(KindNetwork, "test-delete-hook", &NetworkConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.Equal(t, ActionEnsure, lastAction)
				require.NoError(t, RunAction(ctx, r, ActionDelete))
				require.Equal(t, ActionDelete, lastAction)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "network-hook-")
			tt.run(t, c)
		})
	}
}

// ----------------------------------------------------------------------------
// External Network Tests
// ----------------------------------------------------------------------------

func TestNetworkExternal_EnsureFailsIfNotExists(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "network-ext-")

	r, err := c.Resource(KindNetwork, "non-existent-external", &NetworkConfig{External: true})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure, OptionCreate())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotFound)
	require.False(t, r.IsEnsured())
}

func TestNetworkExternal_DeleteIsNoOp(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "network-extdel-")

	r, err := c.Resource(KindNetwork, "test-ext-del", &NetworkConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	network, ok := r.(*Network)
	require.True(t, ok)
	incusName := network.IncusName()

	newDeleteClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	extR, err := newDeleteClient.Resource(KindNetwork, incusName, &NetworkConfig{External: true})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, extR, ActionEnsure))
	require.True(t, extR.IsEnsured())

	require.NoError(t, RunAction(ctx, extR, ActionDelete))

	newClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	checkR, err := newClient.Resource(KindNetwork, "test-ext-del", &NetworkConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, checkR, ActionEnsure))
	require.True(t, checkR.IsEnsured())
}

// ----------------------------------------------------------------------------
// DHCP Range Tests
// ----------------------------------------------------------------------------

func TestCalcIPv4DHCPRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cidr    string
		want    string
		wantErr bool
	}{
		{
			name: "/24 bridge address",
			cidr: "10.100.0.1/24",
			want: "10.100.0.64-10.100.0.254",
		},
		{
			name: "/24 normalized network address",
			cidr: "10.100.0.0/24",
			want: "10.100.0.64-10.100.0.254",
		},
		{
			name: "/16",
			cidr: "172.16.0.1/16",
			want: "172.16.64.0-172.16.255.254",
		},
		{
			name: "/28 (small subnet)",
			cidr: "192.168.1.1/28",
			want: "192.168.1.4-192.168.1.14",
		},
		{
			name:    "invalid CIDR",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
		{
			name:    "/31 too small",
			cidr:    "10.0.0.0/31",
			wantErr: true,
		},
		{
			name:    "/32 too small",
			cidr:    "10.0.0.1/32",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := calcIPv4DHCPRange(tt.cidr)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestCalcIPv6DHCPRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cidr    string
		want    string
		wantErr bool
	}{
		{
			name: "/64 bridge address",
			cidr: "fd42:abc::1/64",
			want: "fd42:abc::100-fd42:abc::ffff",
		},
		{
			name: "/64 normalized network address",
			cidr: "fd42:abc::/64",
			want: "fd42:abc::100-fd42:abc::ffff",
		},
		{
			name: "different prefix",
			cidr: "fd00:1234:5678::1/64",
			want: "fd00:1234:5678::100-fd00:1234:5678::ffff",
		},
		{
			name:    "invalid CIDR",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := calcIPv6DHCPRange(tt.cidr)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNetworkEnsure_ConcurrentCreate(t *testing.T) {
	testlib.SkipLocal(t)
	ctx := t.Context()

	// OverrideName skips the per-project prefix, so every worker resolves to
	// the same global network and actually races.
	name := "icnet" + strings.ToLower(RandString(6))

	const workers = 6
	nets := make([]Resource, workers)
	for i := range nets {
		c := newRandomTestClient(t, "network-race-")
		r, err := c.Resource(KindNetwork, name, &NetworkConfig{OverrideName: name})
		require.NoError(t, err)
		nets[i] = r
	}

	require.Equal(t, nets[0].IncusName(), nets[workers-1].IncusName())

	t.Cleanup(func() {
		_ = RunAction(context.WithoutCancel(ctx), nets[0], ActionDelete)
	})

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})

	for i, r := range nets {
		wg.Add(1)
		go func() {
			defer wg.Done()

			<-start

			errs[i] = RunAction(ctx, r, ActionEnsure, OptionCreate())
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, i)
		require.True(t, nets[i].IsEnsured(), i)
	}
}
