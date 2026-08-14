package ecs_view

import (
	"context"
	"net/netip"
	"sync/atomic"

	"github.com/coredns/coredns/plugin"
	"github.com/miekg/dns"
)

// Source is a plugin that feeds views to ecs_view.
type Source interface {
	// SetSink is called once at startup, before the source produces anything.
	SetSink(Sink)
}

// Sink is where a source publishes.
type Sink interface {
	// Replace swaps everything served for snap, deleting by absence. The snapshot
	// must be finished and unreferenced: from here on every reader shares it.
	Replace(snap *Snapshot)
	// SetHealthy says whether the source's data is fresh. While it is not,
	// answers are clamped so stale records expire fast.
	SetHealthy(healthy bool)
}

// ECSView serves records filtered per querier: a read-only view onto what a
// source publishes, deriving and accumulating nothing.
type ECSView struct {
	Next plugin.Handler

	// Server labels this engine's metrics. A field rather than the context key
	// CoreDNS uses, which core/dnsserver fills and nothing here runs.
	Server string

	// EchoSubnet turns on the RFC 7871 reply option, set once before anything
	// serves. Off by default: it costs the query path an allocation, and only an
	// ECS-aware cache in front reads it.
	EchoSubnet bool

	// Metrics turns on the counters and gauges.
	Metrics bool

	// AllowTransfer is who may ask for a zone transfer, set once before
	// anything serves. Empty allows nobody: a transfer hands over a whole zone
	// unfiltered, and it is the one thing here that lets a peer enumerate.
	//
	// The zone's own opt-in is the other half, and both have to say yes.
	AllowTransfer []netip.Prefix

	// current is the published snapshot.
	current atomic.Pointer[Snapshot]

	// healthy is false while the source says its data is stale, which clamps the
	// TTL.
	healthy atomic.Bool

	// published says a source has handed over at least one snapshot, which an
	// empty fleet does and a cold start has not.
	published atomic.Bool
}

// New returns an ECSView already holding an empty snapshot, so the query path
// never has to check for nil.
func New() *ECSView {
	v := &ECSView{}
	v.current.Store(EmptySnapshot())

	return v
}

// Name implements the plugin.Handler interface.
func (v *ECSView) Name() string { return Name }

// ServeDNS implements plugin.Handler. A name outside every live zone falls
// through to the next plugin.
func (v *ECSView) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	handled, code, err := v.Answer(ctx, w, r)
	if handled {
		return code, err
	}

	return plugin.NextOrFailure(v.Name(), v.Next, ctx, w, r)
}

// Replace implements Sink: a pointer swap, unchanged.
func (v *ECSView) Replace(snap *Snapshot) {
	v.current.Store(snap)
	v.published.Store(true)

	if v.Metrics {
		zonesGauge.Set(float64(len(snap.ByZone)))
		addressesGauge.Set(float64(len(snap.ByAddr)))
	}
}

// SetHealthy implements Sink.
func (v *ECSView) SetHealthy(healthy bool) {
	v.healthy.Store(healthy)
}

// Ready turns true once a snapshot has been published and the source reports
// itself healthy.
func (v *ECSView) Ready() bool {
	return v.published.Load() && v.healthy.Load()
}
