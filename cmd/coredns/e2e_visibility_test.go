package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// visibility is the matrix documented in the fixture: the two APIs share the
// gateway network and see each other, the two databases share nothing.
var visibility = []struct {
	from    string
	sees    []string
	blinded []string
}{
	{
		from:    "gateway",
		sees:    []string{"gateway", "users-api", "products-api"},
		blinded: []string{"users-db", "products-db"},
	},
	{
		from:    "users-api",
		sees:    []string{"gateway", "users-api", "products-api", "users-db"},
		blinded: []string{"products-db"},
	},
	{
		from:    "users-db",
		sees:    []string{"users-api", "users-db"},
		blinded: []string{"gateway", "products-api", "products-db"},
	},
	{
		from:    "products-api",
		sees:    []string{"gateway", "users-api", "products-api", "products-db"},
		blinded: []string{"users-db"},
	},
	{
		from:    "products-db",
		sees:    []string{"products-api", "products-db"},
		blinded: []string{"gateway", "users-api", "users-db"},
	},
}

// TestVisibility stands the fixture up once and runs the whole battery against
// it. Subtests are parallel; the stack's cleanup runs only after they finish.
func TestVisibility(t *testing.T) {
	testlib.SkipE2E(t)

	su := newE2ESuite(t)
	s := su.add("shop", "shop")

	su.up()

	readyCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	s.waitReady(readyCtx, "gateway")
	cancel()

	t.Run("viaResolver", func(t *testing.T) {
		t.Parallel()
		s.runMatrix(t, viaResolver)
	})

	t.Run("direct", func(t *testing.T) {
		t.Parallel()
		s.runMatrix(t, direct)
	})

	t.Run("iutilNetworkAddress", s.testiutilNetworkAddress)
	t.Run("identifiesQuerierBySourceAddress", s.testIdentifiesQuerierBySourceAddress)
	t.Run("failsClosedForUnknownQuerier", s.testFailsClosedForUnknownQuerier)
	t.Run("forwardsExternalNames", s.testForwardsExternalNames)
	t.Run("apex", s.testApex)
	t.Run("ipv6", s.testIPv6)
	t.Run("ptr", s.testPTR)
	t.Run("respondsToInstanceDelete", s.testRespondsToInstanceDelete)
}

// queryMode is how a service asks a question: through its own resolver, or
// straight at CoreDNS.
type queryMode int

const (
	// viaResolver uses the instance's configured resolver, which is CoreDNS
	// itself, so the querier is identified by source address.
	viaResolver queryMode = iota
	// direct queries CoreDNS with an explicit client subnet, which exercises the
	// other identification channel from the same instance.
	direct
)

// ask runs one query from a service and returns the answer section.
func (s *stack) ask(ctx context.Context, t *testing.T, mode queryMode, from, qname, qtype string) (string, error) {
	t.Helper()

	args := []string{"kdig", "+json", "+timeout=3", "+retry=0"}

	if mode == direct {
		args = append(args, "@"+s.dnsAddr(), "-p", corednsPort,
			"+subnet="+s.clientSubnetOf(from)+"/32")
	}

	args = append(args, qname, qtype)

	return s.exec(ctx, from, args...)
}

// runMatrix drives the whole visibility table in one mode.
func (s *stack) runMatrix(t *testing.T, mode queryMode) {
	t.Helper()

	for _, row := range visibility {
		t.Run(row.from, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
			defer cancel()

			// An NXDOMAIN says nothing about why on the wire; a rise in
			// unidentified_clients tells an unplaceable querier from a blind one.
			t.Cleanup(func() {
				if t.Failed() {
					s.reportCounters(t, "gateway", "after "+row.from)
				}
			})

			for _, target := range row.sees {
				out, err := s.ask(ctx, t, mode, row.from, target+"."+s.zone(), "A")
				if !assert.NoErrorf(t, err, "%s -> %s", row.from, target) {
					continue
				}

				if !assert.Equalf(t, "NOERROR", rcode(out),
					"%s must resolve %s:\n%s", row.from, target, out) {
					s.reportQuerier(t, row.from, target)

					continue
				}

				if !assert.Positivef(t, answerCount(out),
					"%s must resolve %s:\n%s", row.from, target, out) {
					continue
				}

				// It must be an address the target actually holds.
				assert.Truef(t, containsAny(out, s.addressesOf(target)),
					"%s -> %s returned an address %s does not hold:\n%s",
					row.from, target, target, out)
			}

			for _, target := range row.blinded {
				out, err := s.ask(ctx, t, mode, row.from, target+"."+s.zone(), "A")
				if !assert.NoErrorf(t, err, "%s -> %s", row.from, target) {
					continue
				}

				assert.Equalf(t, "NXDOMAIN", rcode(out),
					"%s must not resolve %s:\n%s", row.from, target, out)
			}
		})
	}
}

// testiutilNetworkAddress is the assertion a naive implementation fails:
// users-db is only on users-net, so it gets users-api's address there.
func (s *stack) testiutilNetworkAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := s.ask(ctx, t, viaResolver, "users-db", "users-api."+s.zone(), "A")
	require.NoError(t, err, "users-db -> users-api")

	// gateway shares only the gateway network, so the two answers must differ.
	fromGateway, err := s.ask(ctx, t, viaResolver, "gateway", "users-api."+s.zone(), "A")
	require.NoError(t, err, "gateway -> users-api")

	dbAnswer := answerAddress(t, out)
	gwAnswer := answerAddress(t, fromGateway)

	assert.NotEqual(t, gwAnswer, dbAnswer,
		"users-db and gateway must each be answered on the network they share")
}

// testIdentifiesQuerierBySourceAddress covers the direct-resolver path: with no
// client subnet, the source address identifies the querier and buys no more.
func (s *stack) testIdentifiesQuerierBySourceAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := s.exec(ctx, "gateway", "kdig", "+json", "@"+s.dnsAddr(), "-p", corednsPort,
		"+timeout=3", "+retry=0", "+nosubnet", "gateway."+s.zone(), "A")
	require.NoError(t, err, "querying without a client subnet")

	require.Equalf(t, "NOERROR", rcode(out),
		"a query with no client subnet must be attributed by source address:\n%s", out)
	assert.Positivef(t, answerCount(out), "no answer for the querier itself:\n%s", out)

	// gateway shares no network with users-db, so this stays invisible.
	out, err = s.exec(ctx, "gateway", "kdig", "+json", "@"+s.dnsAddr(), "-p", corednsPort,
		"+timeout=3", "+retry=0", "+nosubnet", "users-db."+s.zone(), "A")
	require.NoError(t, err, "querying without a client subnet")

	assert.Equalf(t, "NXDOMAIN", rcode(out),
		"the source address must not widen what the querier may see:\n%s", out)
}

// externalDomains prove the forward path over the real internet, spread across
// operators and TLDs so no single outage takes out a majority.
var externalDomains = []string{
	"google.com",
	"cloudflare.com",
	"github.com",
	"wikipedia.org",
	"debian.org",
	"kernel.org",
	"linuxcontainers.org",
	"amazon.com",
	"microsoft.com",
	"iana.org",
}

// testForwardsExternalNames checks that a name outside every Incus zone reaches
// the forwarder, which is the only way anything but .incus resolves in a stack.
//
// Half the list passes: one failure says something about that domain, while a
// broken forward path takes out all ten.
func (s *stack) testForwardsExternalNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	resolved := make([]string, 0, len(externalDomains))

	for _, name := range externalDomains {
		// Addressed to CoreDNS rather than through resolv.conf: Incus appends
		// the bridge resolver after ours, and the stub would retry there.
		out, err := s.exec(ctx, "gateway", "kdig", "+json", "@"+s.dnsAddr(), "-p", corednsPort,
			"+timeout=3", "+retry=0", name+".", "A")
		if err != nil {
			t.Logf("%s: %s", name, err)

			continue
		}

		if rcode(out) != "NOERROR" || answerCount(out) == 0 {
			t.Logf("%s: %s with %d answers", name, rcode(out), answerCount(out))

			continue
		}

		resolved = append(resolved, name)
	}

	want := len(externalDomains) / 2

	t.Logf("resolved %d of %d external names: %v", len(resolved), len(externalDomains), resolved)

	assert.GreaterOrEqualf(t, len(resolved), want,
		"only %d of %d external names resolved, want at least %d - the forward path looks broken",
		len(resolved), len(externalDomains), want)
}

// testFailsClosedForUnknownQuerier checks that a querier on no known network is
// told nothing. A client subnet is present, so the source address cannot rescue it.
func (s *stack) testFailsClosedForUnknownQuerier(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := s.exec(ctx, "gateway", "kdig", "+json", "@"+s.dnsAddr(), "-p", corednsPort,
		"+timeout=3", "+retry=0", "+subnet=192.0.2.1/32", "gateway."+s.zone(), "A")
	require.NoError(t, err, "querying as an unknown client")

	assert.Equalf(t, "NXDOMAIN", rcode(out),
		"a querier on no known network must be NXDOMAIN:\n%s", out)
}

func (s *stack) testApex(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := s.ask(ctx, t, direct, "gateway", s.zone(), "SOA")
	require.NoError(t, err, "apex SOA")

	assert.Equalf(t, "NOERROR", rcode(out), "apex must answer SOA:\n%s", out)
	assert.Positivef(t, answerCount(out), "apex must answer SOA:\n%s", out)
}

// testPTR checks the reverse direction obeys the forward rule, reversing each
// querier's own address so no knowledge of which bridge holds what is needed.
//
// The name comes from dns.ReverseAddr rather than the plugin's own helper, so
// the wire is checked against something that did not build it.
func (s *stack) testPTR(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	own, err := dns.ReverseAddr(s.clientSubnetOf("users-db"))
	require.NoError(t, err, "reverse name of users-db")

	out, err := s.ask(ctx, t, viaResolver, "users-db", own, "PTR")
	require.NoError(t, err, "reverse of the querier's own address")

	assert.Equalf(t, "NOERROR", rcode(out), "an instance must reverse its own address:\n%s", out)
	assert.Equalf(t, "users-db-1."+s.zone(), answerPTR(t, out),
		"the reverse must name the instance:\n%s", out)

	// products-db shares no network with users-db, so its address reverses to
	// nothing here - exactly as its name resolves to nothing.
	blind, err := dns.ReverseAddr(s.clientSubnetOf("products-db"))
	require.NoError(t, err, "reverse name of products-db")

	out, err = s.ask(ctx, t, viaResolver, "users-db", blind, "PTR")
	require.NoError(t, err, "reverse of an unreachable address")

	assert.Equalf(t, "NXDOMAIN", rcode(out),
		"reversed an address on a network the querier shares nothing with:\n%s", out)
}

func (s *stack) testIPv6(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := s.ask(ctx, t, viaResolver, "gateway", "users-api."+s.zone(), "AAAA")
	require.NoError(t, err, "AAAA query")

	assert.Equalf(t, "NOERROR", rcode(out), "expected an AAAA record on a dual-stack fixture:\n%s", out)
	assert.Positivef(t, answerCount(out), "expected an AAAA record on a dual-stack fixture:\n%s", out)
}

// testRespondsToInstanceDelete checks the store follows the Incus event stream
// rather than a timer.
//
// It restarts products-db before returning rather than from t.Cleanup, which
// would run after the parallel subtests have already resumed.
func (s *stack) testRespondsToInstanceDelete(t *testing.T) {
	deleteCtx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	_, err := s.ask(deleteCtx, t, viaResolver, "products-api", "delete-me."+s.zone(), "A")
	require.NoError(t, err)

	out, err := s.compose(deleteCtx, "incus", "delete", "--force", "delete-me")
	require.NoError(t, err, "delete delete-me: %s", out)

	require.Eventually(t, func() bool {
		out, err := s.ask(deleteCtx, t, viaResolver, "products-api", "delete-me."+s.zone(), "A")

		return err == nil && rcode(out) == "NXDOMAIN"
	}, 60*time.Second, 2*time.Second, "delete-me still resolved after being deleted")
}

// kdigRR is the subset of an RFC 8427 answer record kdig's +json output fills
// in for the record types this file queries. The other rdataX fields are
// omitted; a record of a type not asked for here is never inspected.
type kdigRR struct {
	RdataA    string `json:"rdataA"`
	RdataAAAA string `json:"rdataAAAA"`
	RdataPTR  string `json:"rdataPTR"`
}

// kdigMessage is the subset of kdig's +json output (RFC 8427) this file reads.
type kdigMessage struct {
	RCODE     int      `json:"RCODE"`
	ANCOUNT   int      `json:"ANCOUNT"`
	AnswerRRs []kdigRR `json:"answerRRs"`
}

// parseKdig decodes one kdig +json response.
func parseKdig(out string) (kdigMessage, error) {
	var msg kdigMessage

	err := json.Unmarshal([]byte(out), &msg)

	return msg, err
}

// rcode extracts the response code from a kdig +json response. The top-level
// RCODE is unambiguous - unlike text output, it does not share a name with the
// EDNS pseudosection's ext-rcode.
func rcode(out string) string {
	msg, err := parseKdig(out)
	if err != nil {
		return ""
	}

	return dns.RcodeToString[msg.RCODE]
}

// answerCount reports how many records the answer section holds.
func answerCount(out string) int {
	msg, err := parseKdig(out)
	if err != nil {
		return 0
	}

	return msg.ANCOUNT
}

// answerAddress returns the first A/AAAA address in a kdig answer section.
func answerAddress(t *testing.T, out string) string {
	t.Helper()

	msg, err := parseKdig(out)
	require.NoErrorf(t, err, "parsing kdig +json output:\n%s", out)

	for _, rr := range msg.AnswerRRs {
		if rr.RdataA != "" {
			return rr.RdataA
		}

		if rr.RdataAAAA != "" {
			return rr.RdataAAAA
		}
	}

	require.FailNowf(t, "no address in answer", "%s", out)

	return ""
}

// answerPTR returns the first PTR target in a kdig answer section.
func answerPTR(t *testing.T, out string) string {
	t.Helper()

	msg, err := parseKdig(out)
	require.NoErrorf(t, err, "parsing kdig +json output:\n%s", out)

	for _, rr := range msg.AnswerRRs {
		if rr.RdataPTR != "" {
			return rr.RdataPTR
		}
	}

	require.FailNowf(t, "no PTR record in answer", "%s", out)

	return ""
}

// containsAny reports whether the answer section holds any of the given
// addresses.
func containsAny(out string, addrs []string) bool {
	msg, err := parseKdig(out)
	if err != nil {
		return false
	}

	for _, rr := range msg.AnswerRRs {
		for _, addr := range addrs {
			if rr.RdataA == addr || rr.RdataAAAA == addr {
				return true
			}
		}
	}

	return false
}
