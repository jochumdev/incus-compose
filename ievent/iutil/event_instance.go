package iutil

import (
	"encoding/json"
	"iter"
	"maps"
	"slices"
)

// InstanceInterface is one NIC of an instance: the network it sits on, and what
// it holds there. Read-only once built, so the instance holding it can be shared
// by pointer rather than copied per event.
type InstanceInterface struct {
	project string
	network string
	managed bool

	ipv4 []string
	ipv6 []string
}

// NewInstanceInterface builds one NIC. It takes ownership of both slices - do
// not reuse them; sorting is the caller's.
func NewInstanceInterface(project, network string, managed bool, ipv4, ipv6 []string) InstanceInterface {
	return InstanceInterface{
		project: project,
		network: network,
		managed: managed,
		ipv4:    ipv4,
		ipv6:    ipv6,
	}
}

// Project is the project that owns the network, which for a bridge may not be
// the one the instance is in.
func (n InstanceInterface) Project() string { return n.project }

// Network is the name of the network this NIC sits on.
func (n InstanceInterface) Network() string { return n.network }

// Managed says Incus runs the network, so it has a subnet of its own.
func (n InstanceInterface) Managed() bool { return n.managed }

// IPv4 is what this NIC holds, cloned: one or two addresses, so a copy costs
// less than the ways a shared slice goes wrong.
func (n InstanceInterface) IPv4() []string { return slices.Clone(n.ipv4) }

// IPv6 is what this NIC holds, cloned.
func (n InstanceInterface) IPv6() []string { return slices.Clone(n.ipv6) }

func (n InstanceInterface) equal(other InstanceInterface) bool {
	if n.project != other.project ||
		n.network != other.network ||
		n.managed != other.managed ||
		!slices.Equal(n.ipv4, other.ipv4) ||
		!slices.Equal(n.ipv6, other.ipv6) {
		return false
	}

	return true
}

// instanceInterfaceJSON is the wire form. Its own type rather than tags on the
// fields, so the file format is one visible thing and renaming a field is not a
// format change.
type instanceInterfaceJSON struct {
	Project string   `json:"project"`
	Network string   `json:"network"`
	Managed bool     `json:"managed"`
	IPv4    []string `json:"ipv4,omitempty"`
	IPv6    []string `json:"ipv6,omitempty"`
}

// MarshalJSON writes the wire form, since the fields it is built from are unexported.
func (n InstanceInterface) MarshalJSON() ([]byte, error) {
	return json.Marshal(instanceInterfaceJSON{
		Project: n.project,
		Network: n.network,
		Managed: n.managed,
		IPv4:    n.ipv4,
		IPv6:    n.ipv6,
	})
}

// UnmarshalJSON reads the wire form back through the constructor.
func (n *InstanceInterface) UnmarshalJSON(b []byte) error {
	var v instanceInterfaceJSON

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	*n = NewInstanceInterface(v.Project, v.Network, v.Managed, v.IPv4, v.IPv6)

	return nil
}

// Instance is what one Instance read amounted to. Facts, not verdicts: running
// with no addresses is reported as exactly that, and what it means is the
// business of whoever asked.
//
// Read-only once built, which is what lets one read be shared by pointer with
// every event derived from it, and with the state that holds it.
type Instance struct {
	running bool
	config  map[string]string

	interfaces []InstanceInterface

	// networks is every network the interfaces name, by NetworkKey. Carried with
	// the instance rather than looked up later: an interface says where the
	// instance sits, and the network says what that place is - the subnet a
	// querier is placed by, which nothing downstream can work out on its own.
	networks map[string]*Network
}

// NewInstance builds one instance read. It takes ownership of both maps and the
// slice - do not reuse them.
func NewInstance(
	running bool,
	config map[string]string,
	interfaces []InstanceInterface,
	networks map[string]*Network,
) *Instance {
	return &Instance{
		running:    running,
		config:     config,
		interfaces: interfaces,
		networks:   networks,
	}
}

// Running says the instance was running when it was read. Nothing read answers
// false, the same as the instance being down; ask Enriched to tell them apart.
func (i *Instance) Running() bool {
	if i == nil {
		return false
	}

	return i.running
}

// ConfigValue is one configuration key, which is what a consumer looking for
// its own namespace wants: no copy of the rest.
func (i *Instance) ConfigValue(key string) (string, bool) {
	if i == nil {
		return "", false
	}

	v, ok := i.config[key]

	return v, ok
}

// Config is every configuration key, expanded, with the daemon's own volatile
// bookkeeping already dropped. An iterator rather than the map, so reading it
// costs nothing and writing it is not on offer.
func (i *Instance) Config() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		if i == nil {
			return
		}

		for k, v := range i.config {
			if !yield(k, v) {
				return
			}
		}
	}
}

// Interfaces is where the instance sits, one entry per NIC.
func (i *Instance) Interfaces() iter.Seq[InstanceInterface] {
	return func(yield func(InstanceInterface) bool) {
		if i == nil {
			return
		}

		for _, iface := range i.interfaces {
			if !yield(iface) {
				return
			}
		}
	}
}

// Network is one network the instance sits on, by NetworkKey, or nil. What an
// interface names is what this answers.
func (i *Instance) Network(key string) *Network {
	if i == nil {
		return nil
	}

	return i.networks[key]
}

// Networks is every network the instance sits on, by NetworkKey. One entry per
// network rather than per NIC, so two NICs on one wire share it.
func (i *Instance) Networks() iter.Seq2[string, *Network] {
	return func(yield func(string, *Network) bool) {
		if i == nil {
			return
		}

		for key, net := range i.networks {
			if !yield(key, net) {
				return
			}
		}
	}
}

// InterfaceCount is how many NICs were placed, for a caller that only needs to
// know whether there were any.
func (i *Instance) InterfaceCount() int {
	if i == nil {
		return 0
	}

	return len(i.interfaces)
}

// Equal reports whether two instances say the same thing, where they sit included.
func (i *Instance) Equal(other *Instance) bool {
	if i == nil || other == nil {
		return i == other
	}

	if i.running != other.running ||
		!maps.Equal(i.config, other.config) ||
		!slices.EqualFunc(i.interfaces, other.interfaces, (InstanceInterface).equal) ||
		!maps.EqualFunc(i.networks, other.networks, (*Network).equal) {
		return false
	}

	return true
}

// EqualNoNets is Equal apart from where the instance sits.
func (i *Instance) EqualNoNets(other *Instance) bool {
	if i == nil || other == nil {
		return i == other
	}

	if i.running != other.running ||
		!maps.Equal(i.config, other.config) {
		return false
	}

	return true
}

type instanceJSON struct {
	Running    bool                `json:"running"`
	Config     map[string]string   `json:"config,omitempty"`
	Interfaces []InstanceInterface `json:"interfaces,omitempty"`
	Networks   map[string]*Network `json:"networks,omitempty"`
}

// MarshalJSON writes the wire form, since the fields it is built from are unexported.
func (i Instance) MarshalJSON() ([]byte, error) {
	return json.Marshal(instanceJSON{
		Running:    i.running,
		Config:     i.config,
		Interfaces: i.interfaces,
		Networks:   i.networks,
	})
}

// UnmarshalJSON reads the wire form back.
func (i *Instance) UnmarshalJSON(b []byte) error {
	var v instanceJSON

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	*i = Instance{
		running:    v.Running,
		config:     v.Config,
		interfaces: v.Interfaces,
		networks:   v.Networks,
	}

	return nil
}

// WithInstance derives an event carrying what one instance read found. The
// instance is shared rather than copied, which it can be because nothing may
// write to it once it is built.
func (e *Event) WithInstance(inst *Instance, completeInterfaces bool) *Event {
	next := *e
	next.enriched |= EnrichedInstance

	if completeInterfaces {
		next.enriched |= EnrichedInstanceWithInterfaces
	}

	next.instance = inst

	return &next
}

// Instance is what the read found, or nil where nothing read it. Ask
// Enriched(EnrichedInstance) to tell that from an instance that is simply down.
func (e *Event) Instance() *Instance {
	return e.instance
}
