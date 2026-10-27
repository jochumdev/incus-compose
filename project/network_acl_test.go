package project

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

func TestParsePortSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		specs   []string
		want    []portGroup
		wantErr string
	}{
		{
			name:  "empty",
			specs: nil,
			want:  []portGroup{{protocol: "", ports: ""}},
		},
		{
			name:  "single port defaults to tcp",
			specs: []string{"5432"},
			want:  []portGroup{{protocol: "tcp", ports: "5432"}},
		},
		{
			name:  "multiple protocols and ranges grouped in order",
			specs: []string{"80", "53/udp", "443/tcp", "8000-8080/tcp", "*/icmp4"},
			want: []portGroup{
				{protocol: "tcp", ports: "80,443,8000-8080"},
				{protocol: "udp", ports: "53"},
				{protocol: "icmp4", ports: ""},
			},
		},
		{
			name:  "wildcard tcp",
			specs: []string{"80/tcp", "*/tcp"},
			want:  []portGroup{{protocol: "tcp", ports: ""}},
		},
		{
			name:    "invalid protocol",
			specs:   []string{"80/sctp"},
			wantErr: "unsupported protocol",
		},
		{
			name:    "icmp with port number",
			specs:   []string{"8/icmp4"},
			wantErr: "only supports wildcard port",
		},
		{
			name:    "invalid port range",
			specs:   []string{"9000-8000/tcp"},
			wantErr: "port range",
		},
		{
			name:    "port zero",
			specs:   []string{"0/tcp"},
			wantErr: "between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePortSpecs(tt.specs)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsIPLiteral(t *testing.T) {
	t.Parallel()

	assert.True(t, isIPLiteral("203.0.113.0/24"))
	assert.True(t, isIPLiteral("192.168.1.10"))
	assert.True(t, isIPLiteral("10.0.0.1-10.0.0.10"))
	assert.True(t, isIPLiteral("fd42::/64"))
	assert.True(t, isIPLiteral("fd42::1-fd42::10"))
	assert.True(t, isIPLiteral("10.0.0.0/24, 10.0.1.0/24"))

	assert.False(t, isIPLiteral("frontend"))
	assert.False(t, isIPLiteral("billing-app:public"))
	assert.False(t, isIPLiteral("@internal"))
	assert.False(t, isIPLiteral("10.0.0.10-10.0.0.1"))
}

func TestNetworkACLCompilation(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)

	baseClient, err := gc.EnsureProject("default")
	require.NoError(t, err)

	testlib.SkipNoExtension(t, shared.Incus75Extension, "Network ACLs require incus 7.5")

	t.Run("bridge network acl with same-project cross-project and egress rules", func(t *testing.T) {
		t.Parallel()

		c := baseClient.Clone()

		p := &types.Project{
			Name: "myproj",
			Networks: types.Networks{
				"frontend": {Driver: "bridge"},
				"backend": {
					Driver: "bridge",
					Extensions: types.Extensions{
						"x-incus-compose": map[string]any{
							"acl": map[string]any{
								"ingress": []any{
									map[string]any{
										"from":  "frontend",
										"ports": []any{"5432"},
									},
									map[string]any{
										"from":        "default:public",
										"ports":       []any{"443/tcp"},
										"description": "billing webhook callback",
									},
								},
								"egress": []any{
									map[string]any{
										"to":    "any",
										"ports": []any{"53/udp", "53/tcp", "443/tcp"},
									},
								},
							},
						},
					},
				},
			},
		}

		service := types.ServiceConfig{
			Name: "db",
			Networks: map[string]*types.ServiceNetworkConfig{
				"backend": {},
			},
		}

		_, resources, err := instanceNetworkDevices(c, p, service, "db-1")
		require.NoError(t, err)
		require.Len(t, resources, 1)

		rNet, err := c.Resource(client.KindNetwork, "backend", nil)
		require.NoError(t, err)
		backendNet, ok := rNet.(*client.Network)
		require.True(t, ok)
		require.NotNil(t, backendNet.Config.ACL)

		assert.Empty(t, backendNet.Config.Extensions["security.acls.default.egress.action"])
		assert.Contains(t, backendNet.Config.ACLNetworks, "frontend")
		assert.Contains(t, backendNet.Config.ACLNetworks, "default:public")

		assert.Equal(t, []incusApi.NetworkACLRule{
			{
				Action:          "allow",
				State:           "enabled",
				Source:          "frontend",
				Protocol:        "tcp",
				DestinationPort: "5432",
				Description:     "incus-client: default:ingress from frontend (tcp/5432)",
			},
			{
				Action:          "allow",
				State:           "enabled",
				Source:          "default:public",
				Protocol:        "tcp",
				DestinationPort: "443",
				Description:     "billing webhook callback",
			},
		}, backendNet.Config.ACL.Ingress)

		assert.Equal(t, []incusApi.NetworkACLRule{
			{
				Action:          "allow",
				State:           "enabled",
				Destination:     "",
				Protocol:        "udp",
				DestinationPort: "53",
				Description:     "incus-client: default:egress to any (udp/53)",
			},
			{
				Action:          "allow",
				State:           "enabled",
				Destination:     "",
				Protocol:        "tcp",
				DestinationPort: "53,443",
				Description:     "incus-client: default:egress to any (tcp/53,443)",
			},
		}, backendNet.Config.ACL.Egress)
	})

	t.Run("ovn network acl supports default actions state and selectors", func(t *testing.T) {
		t.Parallel()

		c := baseClient.Clone()

		p := &types.Project{
			Name: "ovnproj",
			Networks: types.Networks{
				"backend": {
					Driver: "ovn",
					Extensions: types.Extensions{
						"x-incus-compose": map[string]any{
							"parent": "incusbr0",
							"acl": map[string]any{
								"default": map[string]any{
									"ingress": "drop",
									"egress":  "reject",
								},
								"ingress": []any{
									map[string]any{
										"from":  "@internal",
										"ports": []any{"*/icmp4"},
									},
									map[string]any{
										"from":   "@external",
										"ports":  []any{"8080"},
										"state":  "logged",
										"action": "drop",
									},
								},
							},
						},
					},
				},
			},
		}

		service := types.ServiceConfig{
			Name: "db",
			Networks: map[string]*types.ServiceNetworkConfig{
				"backend": {},
			},
		}

		_, _, err := instanceNetworkDevices(c, p, service, "db-1")
		require.NoError(t, err)

		rNet, err := c.Resource(client.KindNetwork, "backend", nil)
		require.NoError(t, err)
		backendNet, ok := rNet.(*client.Network)
		require.True(t, ok)
		require.NotNil(t, backendNet.Config.ACL)

		assert.Equal(t, "incusbr0", backendNet.Config.Extensions["network"])
		assert.Equal(t, "drop", backendNet.Config.Extensions["security.acls.default.ingress.action"])
		assert.Equal(t, "reject", backendNet.Config.Extensions["security.acls.default.egress.action"])
		require.Len(t, backendNet.Config.ACL.Ingress, 2)
		assert.Equal(t, "@internal", backendNet.Config.ACL.Ingress[0].Source)
		assert.Equal(t, "icmp4", backendNet.Config.ACL.Ingress[0].Protocol)
		assert.Equal(t, "", backendNet.Config.ACL.Ingress[0].DestinationPort)
		assert.Equal(t, "incus-client: default:ingress from @internal (icmp4)", backendNet.Config.ACL.Ingress[0].Description)
		assert.Equal(t, "drop", backendNet.Config.ACL.Ingress[1].Action)
		assert.Equal(t, "logged", backendNet.Config.ACL.Ingress[1].State)
	})

	t.Run("bridge rejects ovn-only acl knobs", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name    string
			acl     map[string]any
			wantErr string
		}{
			{
				name: "default block on bridge",
				acl: map[string]any{
					"default": map[string]any{"ingress": "drop"},
				},
				wantErr: "acl.default is only supported on OVN networks",
			},
			{
				name: "non-allow action on bridge",
				acl: map[string]any{
					"ingress": []any{map[string]any{"from": "10.0.0.0/24", "action": "drop"}},
				},
				wantErr: "only supported on OVN networks",
			},
			{
				name: "logged state on bridge",
				acl: map[string]any{
					"ingress": []any{map[string]any{"from": "10.0.0.0/24", "state": "logged"}},
				},
				wantErr: "only supported on OVN networks",
			},
			{
				name: "internal selector on bridge",
				acl: map[string]any{
					"ingress": []any{map[string]any{"from": "@internal"}},
				},
				wantErr: "acl selector \"@internal\" is only supported on OVN networks",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				c := baseClient.Clone()

				netName := "net-" + tc.name
				p := &types.Project{
					Name: "errproj",
					Networks: types.Networks{
						netName: {
							Driver: "bridge",
							Extensions: types.Extensions{
								"x-incus-compose": map[string]any{"acl": tc.acl},
							},
						},
					},
				}
				service := types.ServiceConfig{
					Name:     "svc",
					Networks: map[string]*types.ServiceNetworkConfig{netName: {}},
				}

				_, _, err := instanceNetworkDevices(c, p, service, "svc-1")
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			})
		}
	})
}
