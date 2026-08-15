// Incus API values for tests that must not talk to a daemon. A value built here
// encodes a guess at what incusd returns, so a test that turns on the daemon's
// real behavior proves nothing against it and belongs in a tier that has one.

package testlib

import (
	"fmt"

	incusapi "github.com/lxc/incus/v7/shared/api"
)

// LabelPrefix is the namespace an instance or a project configures a synthetic test from.
const LabelPrefix = "testlib."

// Project is one project's worth of Incus values, as three reads would return them.
type Project struct {
	Project   incusapi.Project
	Networks  []incusapi.Network
	Instances []incusapi.Instance

	// States is what GetInstanceState answers, keyed by instance name.
	States map[string]*incusapi.InstanceState
}

// NewProject builds a project with the given number of instances and networks.
// Everything derives from the index: net<n> owns 10.<n>.0.0/24, and inst<i> sits
// on network i%networks holding 10.<n>.0.<10+i>.
func NewProject(name string, instances, networks int) *Project {
	p := &Project{
		Project: incusapi.Project{
			Name:       name,
			ProjectPut: incusapi.ProjectPut{Config: map[string]string{}},
		},
		States: map[string]*incusapi.InstanceState{},
	}

	for n := range networks {
		p.Networks = append(p.Networks, NewNetwork(name, n))
	}

	for i := range instances {
		on := 0
		if networks > 0 {
			on = i % networks
		}

		inst := NewInstance(name, i, on)
		p.Instances = append(p.Instances, inst)
		p.States[inst.Name] = NewInstanceState(i, on)
	}

	return p
}

// NewNetwork builds the nth managed bridge of a project.
func NewNetwork(project string, n int) incusapi.Network {
	return incusapi.Network{
		Name:    NetworkName(n),
		Project: project,
		Managed: true,
		Type:    "bridge",
		NetworkPut: incusapi.NetworkPut{
			Config: map[string]string{
				"ipv4.address": fmt.Sprintf("10.%d.0.1/24", n),
				"ipv6.address": "none",
			},
		},
	}
}

// NewInstance builds the ith running instance of a project, on network on. Its
// NIC is an expanded device, as a profile-supplied one is, so Devices is empty.
func NewInstance(project string, i, on int) incusapi.Instance {
	name := InstanceName(i)

	nic := map[string]string{"type": "nic", "network": NetworkName(on)}

	return incusapi.Instance{
		Name:       name,
		Project:    project,
		Status:     "Running",
		StatusCode: incusapi.Running,
		InstancePut: incusapi.InstancePut{
			Config:  map[string]string{},
			Devices: map[string]map[string]string{},
		},
		ExpandedConfig:  map[string]string{},
		ExpandedDevices: map[string]map[string]string{"eth0": nic},
	}
}

// NewInstanceState builds the state of the ith instance on network on: one
// global address on eth0, plus loopback.
func NewInstanceState(i, on int) *incusapi.InstanceState {
	return &incusapi.InstanceState{
		Status:     "Running",
		StatusCode: incusapi.Running,
		Network: map[string]incusapi.InstanceStateNetwork{
			"lo": {
				Type: "loopback",
				Addresses: []incusapi.InstanceStateNetworkAddress{
					{Family: "inet", Address: "127.0.0.1", Scope: "local"},
				},
			},
			"eth0": {
				Type: "broadcast",
				Addresses: []incusapi.InstanceStateNetworkAddress{
					{Family: "inet", Address: Address(on, i), Netmask: "24", Scope: "global"},
				},
			},
		},
	}
}

// InstanceName is what the ith instance is called.
func InstanceName(i int) string { return fmt.Sprintf("inst%d", i) }

// NetworkName is what the nth network is called.
func NetworkName(n int) string { return fmt.Sprintf("net%d", n) }

// Address is the address the ith instance holds on the nth network.
func Address(n, i int) string { return fmt.Sprintf("10.%d.0.%d", n, 10+i) }

// Label writes one of our keys, prefix and all, onto a config map.
func Label(config map[string]string, key, value string) {
	config[LabelPrefix+key] = value
}
