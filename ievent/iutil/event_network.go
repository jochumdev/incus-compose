package iutil

import "encoding/json"

// NetworkKey is the key of a network.
func NetworkKey(project, name string) string {
	return project + "/" + name
}

// Network is one network, and the subnets it serves. Read-only once built, so
// one read is shared by pointer with every event and every record that names it.
type Network struct {
	name    string
	project string
	managed bool

	ipv4 string
	ipv6 string
}

// NewNetwork builds one network from plain values, keeping this package free of
// the Incus module. The addresses are the configured subnets, gateway and all;
// what "none" or an unfilled "auto" means is decided before this is called.
func NewNetwork(name, project string, managed bool, ipv4, ipv6 string) *Network {
	return &Network{
		name:    name,
		project: project,
		managed: managed,
		ipv4:    ipv4,
		ipv6:    ipv6,
	}
}

// Name is what the network is called, bare, the way a NIC names it.
func (n *Network) Name() string {
	if n == nil {
		return ""
	}

	return n.name
}

// Project is the project that owns the network, which for a bridge is the
// default project unless a project has networks of its own.
func (n *Network) Project() string {
	if n == nil {
		return ""
	}

	return n.project
}

// Managed says Incus runs it, so the subnets below mean something.
func (n *Network) Managed() bool {
	if n == nil {
		return false
	}

	return n.managed
}

// IPv4 is the configured subnet, empty where there is none to serve.
func (n *Network) IPv4() string {
	if n == nil {
		return ""
	}

	return n.ipv4
}

// IPv6 is the configured subnet, empty where there is none to serve.
func (n *Network) IPv6() string {
	if n == nil {
		return ""
	}

	return n.ipv6
}

func (n *Network) equal(other *Network) bool {
	if n == nil || other == nil {
		return n == other
	}

	return n.name == other.name &&
		n.project == other.project &&
		n.managed == other.managed &&
		n.ipv4 == other.ipv4 &&
		n.ipv6 == other.ipv6
}

type networkJSON struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Managed bool   `json:"managed"`
	IPv4    string `json:"ipv4,omitempty"`
	IPv6    string `json:"ipv6,omitempty"`
}

// MarshalJSON writes the wire form, since the fields it is built from are unexported.
func (n Network) MarshalJSON() ([]byte, error) {
	return json.Marshal(networkJSON{
		Name:    n.name,
		Project: n.project,
		Managed: n.managed,
		IPv4:    n.ipv4,
		IPv6:    n.ipv6,
	})
}

// UnmarshalJSON reads the wire form back.
func (n *Network) UnmarshalJSON(b []byte) error {
	var v networkJSON

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	*n = Network{name: v.Name, project: v.Project, managed: v.Managed, ipv4: v.IPv4, ipv6: v.IPv6}

	return nil
}

// Network is the network read, or nil where nothing read one. Ask
// Enriched(EnrichedNetwork) to tell that from nothing having been read.
func (e *Event) Network() *Network {
	return e.network
}

// WithNetwork derives an event carrying one network read, for a network event
// where nothing about an instance changed.
func (e *Event) WithNetwork(net *Network) *Event {
	next := *e
	next.network = net
	next.enriched |= EnrichedNetwork

	return &next
}
