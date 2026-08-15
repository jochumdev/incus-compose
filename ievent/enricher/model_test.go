package enricher

import (
	"net/netip"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// No skip helper on any test here: the model talks to nothing. The Incus values
// come from testlib, which is honest for the question these ask - did the patch
// land - and dishonest for what a stopped instance's state actually looks like,
// which is why nothing here asserts that.

// filled builds a model with one project's networks and instances patched in.
func filled(t *testing.T, p *testlib.Project) *model {
	t.Helper()

	m := newModel()
	m.putWires(p.Networks)

	for i := range p.Instances {
		inst := p.Instances[i]
		m.putInstance(&inst, p.States[inst.Name])
	}

	return m
}

func TestPutInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// change mutates the baseline before it is patched in.
		change   func(*testlib.Project)
		running  bool
		wantNets []string
		wantIPv4 []string
	}{
		{
			name:     "a running instance lands under the network its NIC names",
			running:  true,
			wantNets: []string{"p/net0"},
			wantIPv4: []string{"10.0.0.10"},
		},
		{
			name: "a stopped instance keeps its labels and loses its addresses",
			change: func(p *testlib.Project) {
				p.Instances[0].StatusCode = incusapi.Stopped
				p.Instances[0].Status = "Stopped"
				p.States[testlib.InstanceName(0)] = nil
			},
			running:  false,
			wantNets: nil,
		},
		{
			name: "a running instance with no addresses yet is a fact, not a verdict",
			change: func(p *testlib.Project) {
				p.States[testlib.InstanceName(0)].Network = nil
			},
			running:  true,
			wantNets: nil,
		},
		{
			name: "a NIC on a network the model does not know is skipped",
			change: func(p *testlib.Project) {
				p.Instances[0].ExpandedDevices["eth0"]["network"] = "somewhere-else"
			},
			running:  true,
			wantNets: nil,
		},
		{
			// nictype+parent, with no network key at all, is how a NIC attaches to
			// an unmanaged host bridge - a real, valid device shape network alone
			// would silently drop every address on.
			name: "a NIC named by parent rather than network is still found",
			change: func(p *testlib.Project) {
				nic := p.Instances[0].ExpandedDevices["eth0"]
				delete(nic, "network")
				nic["nictype"] = "bridged"
				nic["parent"] = testlib.NetworkName(0)
			},
			running:  true,
			wantNets: []string{"p/net0"},
			wantIPv4: []string{"10.0.0.10"},
		},
		{
			name: "the loopback is never a network",
			change: func(p *testlib.Project) {
				p.Instances[0].ExpandedDevices["lo"] = map[string]string{"type": "nic", "network": "net0"}
			},
			running:  true,
			wantNets: []string{"p/net0"},
			wantIPv4: []string{"10.0.0.10"},
		},
		{
			name: "a local address is not served",
			change: func(p *testlib.Project) {
				st := p.States[testlib.InstanceName(0)]
				iface := st.Network["eth0"]
				iface.Addresses[0].Scope = "local"
				st.Network["eth0"] = iface
			},
			running:  true,
			wantNets: []string{"p/net0"},
			wantIPv4: nil,
		},
		{
			name: "devices fall back to the instance's own when nothing is expanded",
			change: func(p *testlib.Project) {
				p.Instances[0].Devices = p.Instances[0].ExpandedDevices
				p.Instances[0].ExpandedDevices = nil
			},
			running:  true,
			wantNets: []string{"p/net0"},
			wantIPv4: []string{"10.0.0.10"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := testlib.NewProject("p", 1, 1)
			if tc.change != nil {
				tc.change(p)
			}

			m := newModel()
			m.putWires(p.Networks)

			inst := p.Instances[0]
			got := m.putInstance(&inst, p.States[inst.Name])

			assert.Equal(t, tc.running, got.running)

			keys := make([]string, 0, len(got.nets))
			for k := range got.nets {
				keys = append(keys, k)
			}

			assert.ElementsMatch(t, tc.wantNets, keys, "networks it sits on")

			if len(tc.wantNets) == 1 {
				addrs := got.nets[tc.wantNets[0]].IPv4()

				out := make([]string, 0, len(addrs))
				for _, a := range addrs {
					out = append(out, a.String())
				}

				assert.ElementsMatch(t, tc.wantIPv4, out, "addresses on it")
			}
		})
	}
}

// TestPutInstanceConfig is the whole of what the enricher decides about
// configuration: which map to take. What any key in it means belongs to
// whoever asked - coredns reads its namespace, operator reads its own.
func TestPutInstanceConfig(t *testing.T) {
	t.Parallel()

	const ours = testlib.LabelPrefix + "service"

	tests := []struct {
		name   string
		change func(*testlib.Project)
		want   map[string]string
	}{
		{
			name: "no configuration is an empty map, not a missing one",
			want: map[string]string{},
		},
		{
			name: "keys arrive whole, namespace and all",
			change: func(p *testlib.Project) {
				p.Instances[0].ExpandedConfig[ours] = "web"
			},
			want: map[string]string{ours: "web"},
		},
		{
			name: "nothing is filtered out, whoever wrote it",
			change: func(p *testlib.Project) {
				p.Instances[0].ExpandedConfig[ours] = "web"
				p.Instances[0].ExpandedConfig["user.label.operator.check"] = "http"
				p.Instances[0].ExpandedConfig["boot.autostart"] = "true"
			},
			want: map[string]string{
				ours:                        "web",
				"user.label.operator.check": "http",
				"boot.autostart":            "true",
			},
		},
		{
			name: "a blank value is a value; turning one off is the consumer's rule",
			change: func(p *testlib.Project) {
				p.Instances[0].ExpandedConfig[ours] = "   "
			},
			want: map[string]string{ours: "   "},
		},
		{
			name: "the expanded configuration wins, so profile keys are seen",
			change: func(p *testlib.Project) {
				p.Instances[0].Config[ours] = "local"
				p.Instances[0].ExpandedConfig[ours] = "profile"
			},
			want: map[string]string{ours: "profile"},
		},
		{
			name: "the instance's own answers when nothing was expanded",
			change: func(p *testlib.Project) {
				p.Instances[0].Config[ours] = "local"
				p.Instances[0].ExpandedConfig = nil
			},
			want: map[string]string{ours: "local"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := testlib.NewProject("p", 1, 1)
			if tc.change != nil {
				tc.change(p)
			}

			m := newModel()
			m.putWires(p.Networks)

			inst := p.Instances[0]

			assert.Equal(t, tc.want, m.putInstance(&inst, p.States[inst.Name]).config)
		})
	}
}

func TestNewWire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*incusapi.Network)
		want   []netip.Prefix
	}{
		{
			name: "a managed bridge carries the subnet its gateway names",
			want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		},
		{
			name:   "an unmanaged wire identifies no querier but still keys records",
			change: func(n *incusapi.Network) { n.Managed = false },
			want:   nil,
		},
		{
			name:   "none is a sentinel, not an address",
			change: func(n *incusapi.Network) { n.Config["ipv4.address"] = "none" },
			want:   nil,
		},
		{
			name:   "so is auto",
			change: func(n *incusapi.Network) { n.Config["ipv4.address"] = "auto" },
			want:   nil,
		},
		{
			name:   "something unparseable is skipped rather than fatal",
			change: func(n *incusapi.Network) { n.Config["ipv4.address"] = "not-an-address" },
			want:   nil,
		},
		{
			name: "both families when both are configured",
			change: func(n *incusapi.Network) {
				n.Config["ipv6.address"] = "fd42:1::1/64"
			},
			want: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/24"),
				netip.MustParsePrefix("fd42:1::/64"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			n := testlib.NewNetwork("p", 0)
			if tc.change != nil {
				tc.change(&n)
			}

			assert.Equal(t, tc.want, newWire(n).prefixes, "the subnets a querier is placed by")
		})
	}
}

// TestDropInstance is rule 3: a delete deletes, and costs no read.
func TestDropInstance(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 2, 1)
	m := filled(t, p)

	require.NotNil(t, m.instance("p", testlib.InstanceName(0)))

	m.dropInstance("p", testlib.InstanceName(0))

	assert.Nil(t, m.instance("p", testlib.InstanceName(0)), "gone")
	assert.NotNil(t, m.instance("p", testlib.InstanceName(1)), "and only that one")
}

// TestDropInstanceIsPerProject: two projects may hold the same instance name,
// and a delete in one says nothing about the other. The project is half the
// key, and this is what says so.
func TestDropInstanceIsPerProject(t *testing.T) {
	t.Parallel()

	m := newModel()

	for _, project := range []string{"one", "two"} {
		p := testlib.NewProject(project, 1, 1)

		m.putWires(append(p.Networks, testlib.NewNetwork("one", 0)))

		inst := p.Instances[0]
		m.putInstance(&inst, p.States[inst.Name])
	}

	name := testlib.InstanceName(0)
	require.NotNil(t, m.instance("one", name))
	require.NotNil(t, m.instance("two", name))

	m.dropInstance("one", name)

	assert.Nil(t, m.instance("one", name), "gone from the project the delete named")
	assert.NotNil(t, m.instance("two", name), "and still there in the other")
}

// TestDropInstanceIsIdempotent: a delete for something already gone is not an
// error. Two events for one deletion is the normal case, not the odd one.
func TestDropInstanceIsIdempotent(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 1, 1))

	m.dropInstance("p", testlib.InstanceName(0))
	m.dropInstance("p", testlib.InstanceName(0))
	m.dropInstance("p", "never-existed")

	assert.Empty(t, m.instances)
}

func TestDropProjectTakesItsInstances(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("keep", 2, 1))

	other := testlib.NewProject("go", 3, 1)
	m.putWires(append(other.Networks, testlib.NewNetwork("keep", 0)))

	for i := range other.Instances {
		inst := other.Instances[i]
		m.putInstance(&inst, other.States[inst.Name])
	}

	m.putProject("go", map[string]string{})
	m.dropProject("go")

	assert.Empty(t, m.instancesIn("go"), "the project's instances go with it")
	assert.NotContains(t, m.projects, "go")
	assert.Len(t, m.instancesIn("keep"), 2, "and nobody else's do")
}

// TestInstancesIn is what a profile change fans out over, without reading the
// project to find out who is in it.
func TestInstancesIn(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 3, 1))

	assert.ElementsMatch(t, []subject{
		{project: "p", name: testlib.InstanceName(0)},
		{project: "p", name: testlib.InstanceName(1)},
		{project: "p", name: testlib.InstanceName(2)},
	}, m.instancesIn("p"))

	assert.Empty(t, m.instancesIn("nobody"))
}

func TestWirePatches(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 2)
	m := filled(t, p)

	t.Run("putWire patches one in place", func(t *testing.T) {
		n := testlib.NewNetwork("p", 0)
		n.Config["ipv4.address"] = "10.9.0.1/24"
		m.putWire(n)

		assert.Equal(t,
			[]netip.Prefix{netip.MustParsePrefix("10.9.0.0/24")},
			m.wires["p/net0"].prefixes)
	})

	t.Run("dropWire removes one and leaves the rest", func(t *testing.T) {
		m.dropWire("p", "net0")

		assert.NotContains(t, m.wires, "p/net0")
		assert.Contains(t, m.wires, "p/net1")
	})

	t.Run("putWires replaces the lot, which is how a network goes away", func(t *testing.T) {
		m.putWires([]incusapi.Network{testlib.NewNetwork("p", 0)})

		assert.Contains(t, m.wires, "p/net0")
		assert.NotContains(t, m.wires, "p/net1")
	})
}

// TestPrefixesAreClonedIntoTheEvent: the wire outlives the event and is patched
// in place, so what an event already carries may not move under it.
func TestPrefixesAreClonedIntoTheEvent(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)
	m := filled(t, p)

	held := m.instance("p", testlib.InstanceName(0)).nets["p/net0"].Prefixes()
	require.Len(t, held, 1)

	n := testlib.NewNetwork("p", 0)
	n.Config["ipv4.address"] = "10.9.0.1/24"
	m.putWire(n)

	assert.Equal(t, "10.0.0.0/24", held[0].String(), "what was handed over does not move")
}

// TestBridgeResolvesInTheDefaultProject: a NIC names its network bare, and a
// project without features.networks resolves it against the default project.
func TestBridgeResolvesInTheDefaultProject(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)

	// The bridge belongs to the default project; the instance merely uses it.
	m := newModel()
	m.putWires([]incusapi.Network{testlib.NewNetwork(defaultProject, 0)})

	inst := p.Instances[0]
	got := m.putInstance(&inst, p.States[inst.Name])

	require.Contains(t, got.nets, key(defaultProject, testlib.NetworkName(0)),
		"keyed by the project that owns the bridge, not the one looking at it")
}
