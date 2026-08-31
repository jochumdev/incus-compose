package enricher

import (
	"maps"
	"slices"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/internal/testlib"
)

// No skip helper on any test here: the state talks to nothing. The Incus values
// come from testlib, which is honest for the question these ask - did the patch
// land - but not for what a stopped instance's state actually looks like.

// storeNetworks patches a whole listing in, which is what a run does one
// message at a time.
func storeNetworks(m *state, networks []incusapi.Network) {
	for _, n := range networks {
		m.setNetwork(n)
	}
}

// filled builds a state with one project's networks and instances patched in.
func filled(t *testing.T, p *testlib.Project) *state {
	t.Helper()

	m := newState("")
	storeNetworks(m, p.Networks)

	for i := range p.Instances {
		inst := p.Instances[i]
		m.setInstance(&inst, p.States[inst.Name])
	}

	return m
}

// sits is where one instance sits, as project/network pairs.
func sits(i *iutil.Instance) []string {
	out := make([]string, 0, i.InterfaceCount())

	for iface := range i.Interfaces() {
		out = append(out, iutil.NetworkKey(iface.Project(), iface.Network()))
	}

	return out
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
			name: "a stopped instance keeps its config and loses its addresses",
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
			// nictype+parent, with no network key at all, is how a NIC attaches to
			// an unmanaged host bridge.
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

			m := newState("")
			storeNetworks(m, p.Networks)

			inst := p.Instances[0]
			got, _ := m.setInstance(&inst, p.States[inst.Name])
			require.NotNil(t, got)

			assert.Equal(t, tc.running, got.Running())
			assert.ElementsMatch(t, tc.wantNets, sits(got), "networks it sits on")

			if len(tc.wantNets) == 1 {
				on := slices.Collect(got.Interfaces())
				assert.ElementsMatch(t, tc.wantIPv4, on[0].IPv4(), "addresses on it")
			}
		})
	}
}

// TestPutInstanceInterfacesAreOrdered: the enricher decides whether an event is
// news by comparing one read against the last, so two reads of one unchanged
// instance have to compare equal whatever order the daemon listed its NICs in.
func TestPutInstanceInterfacesAreOrdered(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 3)

	// Three NICs, one per network, so map iteration has something to shuffle.
	inst := p.Instances[0]
	state := p.States[inst.Name]

	for n := 1; n < 3; n++ {
		device := testlib.NetworkName(n)

		inst.ExpandedDevices[device] = map[string]string{"type": "nic", "network": device}
		state.Network[device] = incusapi.InstanceStateNetwork{
			Type: "broadcast",
			Addresses: []incusapi.InstanceStateNetworkAddress{
				{Family: "inet", Address: testlib.Address(n, 0), Netmask: "24", Scope: "global"},
			},
		}
	}

	m := newState("")
	storeNetworks(m, p.Networks)

	first, _ := m.setInstance(&inst, state)
	require.Equal(t, []string{"p/net0", "p/net1", "p/net2"}, sits(first))

	for range 20 {
		again, _ := m.setInstance(&inst, state)

		assert.True(t, first.Equal(again), "the same read twice was taken as news")
	}
}

// TestPutInstanceConfig is the whole of what the enricher decides about
// configuration: which map to take. What any key in it means belongs to
// whoever asked - ic-dns reads its namespace, operator reads its own.
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
			name: "a volatile key is the daemon's bookkeeping, not the instance's",
			change: func(p *testlib.Project) {
				p.Instances[0].ExpandedConfig[ours] = "web"
				p.Instances[0].ExpandedConfig["volatile.eth0.hwaddr"] = "00:16:3e:00:00:01"
			},
			want: map[string]string{ours: "web"},
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

			m := newState("")
			storeNetworks(m, p.Networks)

			inst := p.Instances[0]

			got, _ := m.setInstance(&inst, p.States[inst.Name])

			assert.Equal(t, tc.want, maps.Collect(got.Config()))
		})
	}
}

func TestNewNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		change   func(*incusapi.Network)
		wantIPv4 string
		wantIPv6 string
	}{
		{
			name:     "a managed bridge carries the subnet its gateway names",
			wantIPv4: "10.0.0.1/24",
		},
		{
			name:   "an unmanaged network identifies no querier but still keys records",
			change: func(n *incusapi.Network) { n.Managed = false },
		},
		{
			name:   "none is a sentinel, not an address",
			change: func(n *incusapi.Network) { n.Config["ipv4.address"] = "none" },
		},
		{
			name:   "so is an auto the daemon has not filled in",
			change: func(n *incusapi.Network) { n.Config["ipv4.address"] = "auto" },
		},
		{
			name: "both families when both are configured",
			change: func(n *incusapi.Network) {
				n.Config["ipv6.address"] = "fd42:1::1/64"
			},
			wantIPv4: "10.0.0.1/24",
			wantIPv6: "fd42:1::1/64",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			n := testlib.NewNetwork("p", 0)
			if tc.change != nil {
				tc.change(&n)
			}

			got := newNetwork(n)

			assert.Equal(t, tc.wantIPv4, got.IPv4(), "the IPv4 subnet a querier is placed by")
			assert.Equal(t, tc.wantIPv6, got.IPv6(), "the IPv6 subnet a querier is placed by")
			assert.Equal(t, testlib.NetworkName(0), got.Name())
			assert.Equal(t, "p", got.Project(), "the project comes off the listing")
		})
	}
}

// TestDropInstance is rule 3: a delete deletes, and costs no read.
func TestDropInstance(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 2, 1)
	m := filled(t, p)

	require.NotNil(t, m.instance("p", testlib.InstanceName(0)))

	m.deleteInstance("p", testlib.InstanceName(0))

	assert.Nil(t, m.instance("p", testlib.InstanceName(0)), "gone")
	assert.NotNil(t, m.instance("p", testlib.InstanceName(1)), "and only that one")
}

// TestDropInstanceIsPerProject: two projects may hold the same instance name,
// and a delete in one says nothing about the other. The project is half the
// key, and this is what says so.
func TestDropInstanceIsPerProject(t *testing.T) {
	t.Parallel()

	m := newState("")

	for _, project := range []string{"one", "two"} {
		p := testlib.NewProject(project, 1, 1)

		storeNetworks(m, append(p.Networks, testlib.NewNetwork("one", 0)))

		inst := p.Instances[0]
		m.setInstance(&inst, p.States[inst.Name])
	}

	name := testlib.InstanceName(0)
	require.NotNil(t, m.instance("one", name))
	require.NotNil(t, m.instance("two", name))

	m.deleteInstance("one", name)

	assert.Nil(t, m.instance("one", name), "gone from the project the delete named")
	assert.NotNil(t, m.instance("two", name), "and still there in the other")
}

// TestDropInstanceIsIdempotent: a delete for something already gone is not an
// error. Two events for one deletion is the normal case, not the odd one.
func TestDropInstanceIsIdempotent(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 1, 1))

	m.deleteInstance("p", testlib.InstanceName(0))
	m.deleteInstance("p", testlib.InstanceName(0))
	m.deleteInstance("p", "never-existed")
	m.deleteInstance("never-existed", testlib.InstanceName(0))

	assert.Empty(t, slices.Collect(m.projectInstances("p")))
}

func TestDropProjectTakesItsInstances(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("keep", 2, 1))

	other := testlib.NewProject("go", 3, 1)
	storeNetworks(m, append(other.Networks, testlib.NewNetwork("keep", 0)))

	for i := range other.Instances {
		inst := other.Instances[i]
		m.setInstance(&inst, other.States[inst.Name])
	}

	m.setProject("go", map[string]string{})
	m.deleteProject("go")

	assert.Empty(t, slices.Collect(m.projectInstances("go")), "the project's instances go with it")
	assert.Nil(t, m.projectConfig("go"))
	assert.Len(t, slices.Collect(m.projectInstances("keep")), 2, "and nobody else's do")
}

// TestInstancesIn is what a profile change fans out over, without reading the
// project to find out who is in it.
func TestInstancesIn(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 3, 1))

	assert.ElementsMatch(t, []subject{
		{project: "p", instance: testlib.InstanceName(0)},
		{project: "p", instance: testlib.InstanceName(1)},
		{project: "p", instance: testlib.InstanceName(2)},
	}, slices.Collect(m.projectInstances("p")))

	assert.Empty(t, slices.Collect(m.projectInstances("nobody")))
}

// TestProjectInstancesDoesNotInventTheProject: a read that created what it was
// asked about would make a project nothing has read look like one that is
// simply empty, which is the difference withProject turns on.
func TestProjectInstancesDoesNotInventTheProject(t *testing.T) {
	t.Parallel()

	m := newState("")

	assert.Empty(t, slices.Collect(m.projectInstances("nobody")))
	assert.Empty(t, slices.Collect(m.networkInstances("nobody", "net0")))
	assert.Nil(t, m.instance("nobody", "web"))

	assert.Nil(t, m.projectConfig("nobody"), "asking about a project read it in")
}

// TestNetworkInstances is what a network change fans out over. Both halves of
// the key are compared: two projects with features.networks may own the same
// bare name, and a change to one says nothing about the other.
func TestNetworkInstances(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 2, 1))

	assert.ElementsMatch(t, []subject{
		{project: "p", instance: testlib.InstanceName(0)},
		{project: "p", instance: testlib.InstanceName(1)},
	}, slices.Collect(m.networkInstances("p", testlib.NetworkName(0))))

	assert.Empty(t, slices.Collect(m.networkInstances("other", testlib.NetworkName(0))),
		"the same bare name in another project fanned out over this one")
	assert.Empty(t, slices.Collect(m.networkInstances("p", "net9")))
}

func TestNetworkPatches(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 2)
	m := filled(t, p)

	t.Run("storeNetwork patches one in place", func(t *testing.T) {
		n := testlib.NewNetwork("p", 0)
		n.Config["ipv4.address"] = "10.9.0.1/24"
		m.setNetwork(n)

		got, project, known := m.network("p", testlib.NetworkName(0))
		require.True(t, known)

		assert.Equal(t, "p", project)
		assert.Equal(t, "10.9.0.1/24", got.IPv4())
	})

	t.Run("deleteNetwork removes one and leaves the rest", func(t *testing.T) {
		m.deleteNetwork("p", testlib.NetworkName(0))

		_, _, known := m.network("p", testlib.NetworkName(0))
		assert.False(t, known)

		_, _, known = m.network("p", testlib.NetworkName(1))
		assert.True(t, known)
	})

	t.Run("setNetwork puts back one deleteNetwork took", func(t *testing.T) {
		m.setNetwork(testlib.NewNetwork("p", 0))

		_, _, known := m.network("p", testlib.NetworkName(0))
		assert.True(t, known)
	})
}

// TestWhatAnInstanceHoldsDoesNotMove: the network outlives the event and is
// patched in place, so what an instance already carries may not move under it.
func TestWhatAnInstanceHoldsDoesNotMove(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)
	m := filled(t, p)

	held := m.instance("p", testlib.InstanceName(0))
	require.Equal(t, []string{"p/net0"}, sits(held))

	n := testlib.NewNetwork("p", 0)
	n.Name = "renamed"
	m.setNetwork(n)

	assert.Equal(t, []string{"p/net0"}, sits(held), "what was handed over moved")
}

// TestBridgeResolvesInTheDefaultProject: a NIC names its network bare, and a
// project without features.networks resolves it against the default project.
func TestBridgeResolvesInTheDefaultProject(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)

	// The bridge belongs to the default project; the instance merely uses it.
	m := newState("")
	storeNetworks(m, []incusapi.Network{testlib.NewNetwork(iutil.DefaultProject, 0)})

	inst := p.Instances[0]
	got, _ := m.setInstance(&inst, p.States[inst.Name])
	require.NotNil(t, got)

	assert.Equal(t, []string{iutil.DefaultProject + "/" + testlib.NetworkName(0)}, sits(got),
		"keyed by the project that owns the bridge, not the one looking at it")
}

// TestTheOwnProjectWinsOverTheDefault: two networks share a bare name, and the
// instance's own project is asked first - that is what features.networks means.
func TestTheOwnProjectWinsOverTheDefault(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)

	m := newState("")
	storeNetworks(m, []incusapi.Network{
		testlib.NewNetwork(iutil.DefaultProject, 0),
		testlib.NewNetwork("p", 0),
	})

	inst := p.Instances[0]
	got, _ := m.setInstance(&inst, p.States[inst.Name])
	require.NotNil(t, got)

	assert.Equal(t, []string{"p/" + testlib.NetworkName(0)}, sits(got))
}

// TestAnIncompleteReadIsNoRead pins the rule a half-read instance cost a whole
// e2e run to find: a NIC naming a network nothing has read cannot be placed, so
// nothing is stored rather than a subset of where the instance actually sits.
func TestAnIncompleteReadIsNoRead(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)
	inst := p.Instances[0]
	state := p.States[inst.Name]

	m := newState("")

	// The network its NIC names has not been read yet.
	held, missing := m.setInstance(&inst, state)
	require.Nil(t, held, "a read that could not place a NIC was taken as one")
	assert.Nil(t, m.instance("p", inst.Name), "and it was stored anyway")

	assert.Equal(t, testlib.NetworkName(0), missing,
		"the network that could not be placed is what has to be read")

	m.setNetwork(p.Networks[0])

	got, _ := m.setInstance(&inst, state)
	require.NotNil(t, got, "the same read was refused once the network was known")
	assert.Equal(t, []string{"p/net0"}, sits(got))
}

// TestMissingIsWhatTheListingLeftOut: a listing is the whole of one scope, so
// every name held that it does not have has gone.
func TestMissingIsWhatTheListingLeftOut(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 3, 1))

	assert.Equal(t, []subject{
		{project: "p", instance: testlib.InstanceName(1)},
		{project: "p", instance: testlib.InstanceName(2)},
	}, m.missingInstances("p", []string{testlib.InstanceName(0)}))

	assert.Nil(t, m.missingInstances("p", []string{
		testlib.InstanceName(0), testlib.InstanceName(1), testlib.InstanceName(2),
	}), "a listing that has everything pruned something")

	assert.Nil(t, m.missingInstances("nobody", nil), "a project nothing holds answered with names")
}

// TestPruneNetworksAndProjects covers the two a run drops where they stand,
// having nobody to announce them to.
func TestPruneNetworksAndProjects(t *testing.T) {
	t.Parallel()

	m := filled(t, testlib.NewProject("p", 1, 2))
	m.setProject("p", map[string]string{})
	m.setProject("gone", map[string]string{})

	t.Run("a network the listing left out", func(t *testing.T) {
		m.pruneNetworks("p", []string{testlib.NetworkName(0)})

		_, _, known := m.network("p", testlib.NetworkName(0))
		assert.True(t, known, "a listed name was pruned")

		_, _, known = m.network("p", testlib.NetworkName(1))
		assert.False(t, known, "a name the listing no longer has stayed")
	})

	t.Run("a project the listing left out, and everything in it", func(t *testing.T) {
		m.pruneProjects([]string{"p"})

		assert.NotNil(t, m.projectConfig("p"), "a listed project was pruned")
		assert.Nil(t, m.projectConfig("gone"))
		assert.Len(t, slices.Collect(m.projectInstances("p")), 1, "and nobody else's instances went")
	})
}

// TestCloneDoesNotMoveUnderItsReader is what a writer goroutine rests on: what
// it was handed is the fleet as it stood, whatever the fold does next.
func TestCloneDoesNotMoveUnderItsReader(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 2, 1)
	m := filled(t, p)

	// Held by the caller as well as by the state, which is how setProject is
	// given one: straight off the Incus client.
	config := map[string]string{iutil.FeaturesNetworks: "true"}
	m.setProject("p", config)

	held := m.clone()

	t.Run("a project that sets something", func(t *testing.T) {
		assert.Equal(t, config, held.projectConfig("p"))
	})

	t.Run("an instance the fold replaces after the copy", func(t *testing.T) {
		was := held.instance("p", testlib.InstanceName(0))
		require.NotNil(t, was)

		moved := p.Instances[0]
		moved.ExpandedConfig["user.label.moved"] = "yes"
		m.setInstance(&moved, p.States[moved.Name])

		assert.True(t, was.Equal(held.instance("p", testlib.InstanceName(0))),
			"a read that landed after the copy reached it")

		_, seen := m.instance("p", testlib.InstanceName(0)).ConfigValue("user.label.moved")
		assert.True(t, seen, "and the live state did not take it")
	})

	t.Run("an instance the fold deletes after the copy", func(t *testing.T) {
		m.deleteInstance("p", testlib.InstanceName(1))

		assert.NotNil(t, held.instance("p", testlib.InstanceName(1)), "a delete reached the copy")
		assert.Nil(t, m.instance("p", testlib.InstanceName(1)))
	})

	t.Run("a network the fold deletes after the copy", func(t *testing.T) {
		m.deleteNetwork("p", testlib.NetworkName(0))

		_, _, known := held.network("p", testlib.NetworkName(0))
		assert.True(t, known, "a delete reached the copy")

		_, _, known = m.network("p", testlib.NetworkName(0))
		assert.False(t, known)
	})

	t.Run("a whole project the fold drops after the copy", func(t *testing.T) {
		m.deleteProject("p")

		assert.Len(t, slices.Collect(held.projectInstances("p")), 2, "a prune reached the copy")
		assert.Empty(t, slices.Collect(m.projectInstances("p")))
	})

	t.Run("what a project sets, written through the map it was given", func(t *testing.T) {
		config[iutil.FeaturesNetworks] = "false"

		assert.Equal(t, "true", held.projectConfig("p")[iutil.FeaturesNetworks],
			"the copy shares the map it was handed")
	})
}

// TestASubnetThatMovedIsNews: the networks travel with the instance, so a
// bridge that was renumbered makes every read on it say something new. Without
// them the two reads compare equal - same name, same addresses - and the change
// reaches nobody, because the network's own fan-out is what re-reads them.
func TestASubnetThatMovedIsNews(t *testing.T) {
	t.Parallel()

	p := testlib.NewProject("p", 1, 1)
	m := filled(t, p)

	inst := p.Instances[0]

	was, _ := m.setInstance(&inst, p.States[inst.Name])
	require.NotNil(t, was)

	moved := testlib.NewNetwork("p", 0)
	moved.Config["ipv4.address"] = "10.9.0.1/24"
	m.setNetwork(moved)

	now, _ := m.setInstance(&inst, p.States[inst.Name])
	require.NotNil(t, now)

	assert.False(t, was.Equal(now), "a subnet that moved was taken for the same read twice")

	held := now.Network(iutil.NetworkKey("p", testlib.NetworkName(0)))
	require.NotNil(t, held, "the network the NIC names did not travel with the instance")
	assert.Equal(t, "10.9.0.1/24", held.IPv4())
}
