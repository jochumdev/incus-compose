package dns

import (
	"net/netip"

	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// render turns one name's addresses on one network into records, shared by every
// view that can see this network. v4 holds only IPv4, v6 only IPv6, pre-sorted.
func render(name string, v4, v6 []netip.Addr, ttl uint32) ecs_view.Records {
	out := make(ecs_view.Records, 2)

	if len(v4) > 0 {
		rrs := make([]dns.A, len(v4))
		buf := make([]byte, 0, 4*len(v4))
		list := make([]dns.RR, 0, len(v4))

		for i, addr := range v4 {
			b := addr.As4()
			at := len(buf)
			buf = append(buf, b[:]...)

			rrs[i] = dns.A{
				Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
				A:   buf[at : at+4 : at+4],
			}
			list = append(list, &rrs[i])
		}

		out[dns.TypeA] = list
	}

	if len(v6) > 0 {
		rrs := make([]dns.AAAA, len(v6))
		buf := make([]byte, 0, 16*len(v6))
		list := make([]dns.RR, 0, len(v6))

		for i, addr := range v6 {
			b := addr.As16()
			at := len(buf)
			buf = append(buf, b[:]...)

			rrs[i] = dns.AAAA{
				Hdr:  dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl},
				AAAA: buf[at : at+16 : at+16],
			}
			list = append(list, &rrs[i])
		}

		out[dns.TypeAAAA] = list
	}

	return out
}

// renderPTR turns the names one address answers to into records, sharing storage
// as render does; sorting the targets is the caller's.
func renderPTR(name string, targets []string, ttl uint32) ecs_view.Records {
	if len(targets) == 0 {
		return nil
	}

	rrs := make([]dns.PTR, len(targets))
	list := make([]dns.RR, 0, len(targets))

	for i, target := range targets {
		rrs[i] = dns.PTR{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
			Ptr: target,
		}
		list = append(list, &rrs[i])
	}

	return ecs_view.Records{dns.TypePTR: list}
}
