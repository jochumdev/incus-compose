package dns

import (
	"context"
	"log/slog"
	"sync"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics/vars"
	"github.com/coredns/coredns/plugin/pkg/edns"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// adapter puts a CoreDNS plugin chain after a miekg/dns server, shaped after
// dnsserver.Server.ServeDNS at CoreDNS v1.14.6 - diff it by hand on a bump.
type adapter struct {
	chain plugin.Handler
}

func (a *adapter) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	// A query with no question section is SERVFAIL rather than a panic further
	// down. miekg's own mux checks this; we are the mux.
	if r == nil || len(r.Question) == 0 {
		reply(w, r, dns.RcodeServerFailure)

		return
	}

	// A plugin panic is this query's problem and not the process's.
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}

		slog.Error("recovered from a panic while serving",
			"qname", r.Question[0].Name, "panic", rec)

		// CoreDNS's own counter, normally incremented by core/dnsserver, which
		// serves nothing here.
		vars.Panic.Inc()

		reply(w, r, dns.RcodeServerFailure)
	}()

	// Anything but IN is refused. CoreDNS makes CHAOS configurable for
	// version.bind; we serve no such zone.
	if r.Question[0].Qclass != dns.ClassINET {
		reply(w, r, dns.RcodeRefused)

		return
	}

	// An EDNS version we do not speak is answered at once, with BADVERS.
	m, err := edns.Version(r)
	if err != nil {
		_ = w.WriteMsg(m)

		return
	}

	// The one that is not optional: without it a reply may exceed the buffer
	// the client advertised, and the client drops it.
	w = request.NewScrubWriter(r, w)

	rcode, err := a.chain.ServeDNS(context.Background(), w, r)
	if err != nil {
		slog.Error("serving", "qname", r.Question[0].Name, "err", err)
	}

	// A plugin that returned an rcode without writing gets a reply written for
	// it, which is what the last plugin in a chain relies on.
	if !plugin.ClientWrite(rcode) {
		reply(w, r, rcode)
	}
}

// reply answers with a bare rcode.
func reply(w dns.ResponseWriter, r *dns.Msg, rcode int) {
	m := new(dns.Msg)
	m.SetRcode(r, rcode)

	_ = w.WriteMsg(m)
}

// refuse ends the chain when there is no forwarder. An empty NOERROR would
// claim the name exists here with no records of this type.
//
// It returns the rcode and writes nothing, which is CoreDNS's contract:
// plugin.ClientWrite reports false for REFUSED, so ServeDNS above writes the
// reply on this plugin's behalf. Writing here as well put two responses on the
// wire for every query that fell through - half the syscalls on the path that
// carries most of a resolver's traffic, and duplicate ids at the client.
type refuse struct{}

func (refuse) Name() string { return "refuse" }

func (refuse) ServeDNS(_ context.Context, _ dns.ResponseWriter, _ *dns.Msg) (int, error) {
	return dns.RcodeRefused, nil
}

// wire hangs a query chain off the engine, back to front, the way
// core/dnsserver does: `stack = site.Plugin[i](stack)`. Transfers come first,
// since a transfer answers nothing the engine would and the engine answers
// nothing a transfer would.
func wire(x *xfr, view *ecs_view.ECSView, chain []plugin.Plugin) {
	stack := plugin.Handler(refuse{})

	for i := len(chain) - 1; i >= 0; i-- {
		stack = chain[i](stack)
	}

	view.Next = stack
	x.Next = view
}

// serveDNS answers on addr over UDP and TCP until ctx is canceled. Both always:
// a truncated UDP reply is only useful if the client can come back over TCP.
func serveDNS(ctx context.Context, addr string, handler dns.Handler) error {
	servers := []*dns.Server{
		{Addr: addr, Net: "udp", Handler: handler},
		{Addr: addr, Net: "tcp", Handler: handler},
	}

	// Buffered for both, so a failing server never blocks on a send nobody reads.
	errs := make(chan error, len(servers))

	var wg sync.WaitGroup

	for _, s := range servers {
		wg.Go(func() {
			err := s.ListenAndServe()
			if err != nil {
				errs <- err
			}
		})
	}

	defer wg.Wait()

	// Shutdown is what unblocks ListenAndServe, on the way out of either arm.
	defer func() {
		for _, s := range servers {
			_ = s.Shutdown()
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errs:
		return err
	}
}
