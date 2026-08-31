// Package kubebench benchmarks CoreDNS's own kubernetes plugin at the same
// shape as the ecs_view one, as a yardstick. It drives ServeDNS with a hand-fed
// API cache, so nothing but the query path is measured.
package kubebench

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/coredns/coredns/plugin/kubernetes"
	"github.com/coredns/coredns/plugin/kubernetes/object"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	api "k8s.io/api/core/v1"
)

// apiConn is a pre-populated stand-in for the plugin's Kubernetes API cache.
// The plugin reads it through indexes, so the benchmark builds those indexes
// once and the query path does map lookups exactly as it does in a cluster.
type apiConn struct {
	svc        map[string][]*object.Service
	eps        map[string][]*object.Endpoints
	namespaces map[string]*object.Namespace
}

func (a *apiConn) SvcIndex(s string) []*object.Service    { return a.svc[s] }
func (a *apiConn) EpIndex(s string) []*object.Endpoints   { return a.eps[s] }
func (a *apiConn) HasSynced() bool                        { return true }
func (a *apiConn) Run()                                   {}
func (a *apiConn) Stop() error                            { return nil }
func (a *apiConn) Modified(kubernetes.ModifiedMode) int64 { return 3 }

func (a *apiConn) ServiceList() []*object.Service {
	out := make([]*object.Service, 0, len(a.svc))
	for _, list := range a.svc {
		out = append(out, list...)
	}

	return out
}

func (a *apiConn) EndpointsList() []*object.Endpoints {
	out := make([]*object.Endpoints, 0, len(a.eps))
	for _, list := range a.eps {
		out = append(out, list...)
	}

	return out
}

func (a *apiConn) GetNamespaceByName(name string) (*object.Namespace, error) {
	ns, ok := a.namespaces[name]
	if !ok {
		return nil, errors.New("namespace not found")
	}

	return ns, nil
}

// The rest of the API surface the plugin expects. None of it is on the path a
// service A query takes.
func (a *apiConn) ServiceImportList() []*object.ServiceImport       { return nil }
func (a *apiConn) SvcImportIndex(string) []*object.ServiceImport    { return nil }
func (a *apiConn) SvcIndexReverse(string) []*object.Service         { return nil }
func (a *apiConn) SvcExtIndexReverse(string) []*object.Service      { return nil }
func (a *apiConn) EpIndexReverse(string) []*object.Endpoints        { return nil }
func (a *apiConn) McEpIndex(string) []*object.MultiClusterEndpoints { return nil }
func (a *apiConn) PodIndex(string) []*object.Pod                    { return nil }
func (a *apiConn) GetNodeByName(context.Context, string) (*api.Node, error) {
	return nil, errors.New("no nodes")
}

// benchPlugin mirrors the ecs_view shape: zones zones, one namespace per zone, and
// in each a headless service named web that fans out to replicas endpoints.
func benchPlugin(zones, replicas int) *kubernetes.Kubernetes {
	names := make([]string, 0, zones)
	conn := &apiConn{
		svc:        make(map[string][]*object.Service, zones),
		eps:        make(map[string][]*object.Endpoints, zones),
		namespaces: make(map[string]*object.Namespace, zones),
	}

	for z := range zones {
		names = append(names, fmt.Sprintf("cluster%03d.local.", z))

		ns := fmt.Sprintf("ns%03d", z)
		idx := object.ServiceKey("web", ns)

		conn.namespaces[ns] = &object.Namespace{Name: ns}

		// Headless, so the answer is every endpoint rather than one cluster IP.
		// That is what makes replicas mean the same thing on both sides.
		conn.svc[idx] = []*object.Service{{
			Name:       "web",
			Namespace:  ns,
			Index:      idx,
			ClusterIPs: []string{api.ClusterIPNone},
			Ports:      []api.ServicePort{{Name: "http", Protocol: api.ProtocolTCP, Port: 80}},
		}}

		addrs := make([]object.EndpointAddress, 0, replicas)
		for n := range replicas {
			addrs = append(addrs, object.EndpointAddress{IP: fmt.Sprintf("10.%d.%d.%d", z/256, z%256, n+1)})
		}

		conn.eps[idx] = []*object.Endpoints{{
			Name:      "web",
			Namespace: ns,
			Index:     idx,
			Subsets: []object.EndpointSubset{{
				Addresses: addrs,
				Ports:     []object.EndpointPort{{Port: 80, Name: "http", Protocol: "tcp"}},
			}},
		}}
	}

	k := kubernetes.New(names)
	k.APIConn = conn

	return k
}

// BenchmarkServeDNS measures the same query the ecs_view benchmark measures: one
// service name that fans out to every replica behind it.
func BenchmarkServeDNS(b *testing.B) {
	for _, c := range cases {
		k := benchPlugin(c.zones, c.replicas)
		ctx := context.Background()
		w := dnstest.NewRecorder(&test.ResponseWriter{})

		qname := fmt.Sprintf("web.ns%03d.svc.cluster%03d.local.", c.zones-1, c.zones-1)

		req := new(dns.Msg)
		req.SetQuestion(qname, dns.TypeA)

		b.Run(fmt.Sprintf("zones=%d_replicas=%d", c.zones, c.replicas), func(b *testing.B) {
			// A run that measures NXDOMAIN measures nothing, so the shape is
			// checked once before it is timed.
			_, err := k.ServeDNS(ctx, w, req)
			if err != nil {
				b.Fatal(err)
			}

			if len(w.Msg.Answer) != c.replicas {
				b.Fatalf("%s answered %d records, want %d", qname, len(w.Msg.Answer), c.replicas)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for range b.N {
				_, _ = k.ServeDNS(ctx, w, req)
			}
		})
	}
}

// cases is the shared shape both plugins are measured at, see ../benchmark.md.
var cases = []struct{ zones, replicas int }{
	{1, 1}, {1, 100}, {50, 1}, {50, 20}, {500, 1}, {500, 20},
}
