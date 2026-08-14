package test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// TestCrossProject stands up three compose files - CoreDNS in one project, two
// workload projects in others - and asserts the visibility rule across them.
//
// Only this catches a bridge two projects reference failing to collapse to one
// network key, which reads on the wire as correct isolation.
//
//	coredns     dns
//	alpha-api   dns, alpha-net
//	alpha-db         alpha-net
//	beta-api    dns,             beta-net
//	beta-db                      beta-net
//
// No t.Parallel(): the fixtures pin subnets, so suites deploy one at a time.
func TestCrossProject(t *testing.T) {
	testlib.SkipE2E(t)

	su := newE2ESuite(t)

	// The dns stack owns the bridge alpha and beta attach to, so it is added
	// first: deploy order is the order stacks are added, teardown its reverse.
	dns := su.add("dns", "multi/dns")

	// The dns fixture's .env is this run's addressing; alpha and beta inherit it
	// rather than keeping copies.
	su.distribute(dns)
	su.export("DNS_PROJECT", dns.project)

	// Launch order: dns first, because it owns the bridge the other two attach
	// to, then alpha and beta. Teardown runs this in reverse.
	alpha := su.add("alpha", "multi/alpha")
	beta := su.add("beta", "multi/beta")

	// The shared bridge only works if every project reports it under one
	// owner/name pair, so dump that rather than interpreting an NXDOMAIN.
	su.onFailure(func() {
		for _, st := range []*stack{dns, alpha, beta} {
			st.reportNetworks(t)
			st.reportInstances(t)
		}

		// Last, because it is the longest and the most useful: with
		// COREDNS_DEBUG=true it carries client address and view per query.
		dns.reportServerLog(t, "coredns")
	})

	su.up()

	// Either project proves the server is up; both zones come from one snapshot.
	alpha.waitReady("alpha-api")
	beta.waitReady("beta-api")

	t.Run("sharedBridgeCrossesProjects", func(t *testing.T) {
		crossProjectVisibility(t, alpha, beta)
	})

	t.Run("separateZonesPerProject", func(t *testing.T) {
		separateZones(t, alpha, beta)
	})
}

// crossProjectVisibility is the matrix. Each row asks from one project and names
// a target in the other, so every assertion crosses a project boundary.
func crossProjectVisibility(t *testing.T, alpha, beta *stack) {
	tests := []struct {
		name     string
		from     *stack
		asker    string
		target   *stack
		resolves string
		want     string
	}{
		{
			name: "alpha-api sees beta-api over the shared bridge",
			from: alpha, asker: "alpha-api",
			target: beta, resolves: "beta-api", want: "NOERROR",
		},
		{
			name: "beta-api sees alpha-api over the shared bridge",
			from: beta, asker: "beta-api",
			target: alpha, resolves: "alpha-api", want: "NOERROR",
		},
		{
			// beta-db is on beta-net alone, which alpha touches nowhere.
			name: "alpha-api cannot see beta-db",
			from: alpha, asker: "alpha-api",
			target: beta, resolves: "beta-db", want: "NXDOMAIN",
		},
		{
			// alpha-db is not on the shared bridge, so the bridge buys it nothing.
			name: "alpha-db cannot see beta-api",
			from: alpha, asker: "alpha-db",
			target: beta, resolves: "beta-api", want: "NXDOMAIN",
		},
		{
			name: "beta-db cannot see alpha-api",
			from: beta, asker: "beta-db",
			target: alpha, resolves: "alpha-api", want: "NXDOMAIN",
		},
		{
			// Within one project, the private bridge still works.
			name: "alpha-api sees alpha-db over alpha-net",
			from: alpha, asker: "alpha-api",
			target: alpha, resolves: "alpha-db", want: "NOERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
			defer cancel()

			qname := tc.resolves + "." + tc.target.zone()

			out, err := tc.from.ask(ctx, t, viaResolver, tc.asker, qname, "A")
			require.NoErrorf(t, err, "%s -> %s", tc.asker, qname)

			if !assert.Equalf(t, tc.want, rcode(out), "%s -> %s:\n%s", tc.asker, qname, out) {
				probeTarget(t, tc.from, tc.asker, tc.target, tc.resolves)
			}
		})
	}
}

// probeTarget separates the three failures that all read as NXDOMAIN: a missing
// service label, a target with no records at all, and a split network key.
func probeTarget(t *testing.T, from *stack, asker string, target *stack, service string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), commandTimeout)
	defer cancel()

	// An instance answers to its own name whatever the service label says, and
	// incus-compose appends a replica index to it.
	instance := service + "-1"
	local := target.label + "-api"

	probes := []struct {
		what  string
		st    *stack
		asker string
		qname string
	}{
		{"service name, from its own project", target, local, service + "." + target.zone()},
		{"instance name, from its own project", target, local, instance + "." + target.zone()},
		{"instance name, across projects", from, asker, instance + "." + target.zone()},
	}

	for _, p := range probes {
		out, err := p.st.ask(ctx, t, viaResolver, p.asker, p.qname, "A")
		if err != nil {
			t.Logf("  %s: %s -> query failed: %s", p.what, p.qname, err)

			continue
		}

		t.Logf("  %s: %s asked from %s -> %s (%d answers)",
			p.what, p.qname, p.asker, rcode(out), answerCount(out))
	}
}

// separateZones checks the two projects keep their own zones, and that an answer
// crossing the bridge holds the bridge address rather than the private one.
//
// The address is the sharper half: the wrong one still answers NOERROR.
func separateZones(t *testing.T, alpha, beta *stack) {
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	require.NotEqual(t, alpha.zone(), beta.zone(), "each project serves its own zone")

	// Both zones answer at the apex, from one server.
	for _, st := range []*stack{alpha, beta} {
		out, err := st.exec(ctx, st.label+"-api", "kdig", "@"+st.dnsAddr(), "-p", corednsPort,
			"+timeout=3", "+retry=0", st.zone(), "SOA")
		require.NoErrorf(t, err, "apex of %s", st.zone())
		assert.Equalf(t, "NOERROR", rcode(out), "apex of %s:\n%s", st.zone(), out)
	}

	// beta-api holds an address on the shared bridge and one on beta-net. alpha
	// shares only the bridge, so that is the one it must be given.
	out, err := alpha.ask(ctx, t, viaResolver, "alpha-api", "beta-api."+beta.zone(), "A")
	require.NoError(t, err, "alpha-api -> beta-api")
	require.Equalf(t, "NOERROR", rcode(out), "alpha-api must resolve beta-api:\n%s", out)

	got := answerAddress(t, out)
	require.NotEmpty(t, got, "no address in the answer")

	assert.Truef(t, sharedSubnet(got),
		"alpha-api was handed %s, which is not on the shared bridge - it cannot route there:\n%s",
		got, out)
}

// sharedSubnet reports whether addr is on the dns stack's bridge. Hardcoded, so
// a fixture that moves it fails here rather than passing on the wrong network.
func sharedSubnet(addr string) bool {
	const prefix = "10.233.134."

	return len(addr) > len(prefix) && addr[:len(prefix)] == prefix
}

// TestLocateDNSMulti renders the three multi fixtures and checks they agree on
// the resolver, deploying nothing and talking to no daemon.
func TestLocateDNSMulti(t *testing.T) {
	testlib.SkipE2E(t)

	su := newE2ESuite(t)

	dns := su.add("dns", "multi/dns")

	su.distribute(dns)
	su.export("DNS_PROJECT", dns.project)

	su.add("alpha", "multi/alpha")
	su.add("beta", "multi/beta")

	su.locateDNS()

	assert.Equal(t, "10.233.134.11", su.corednsAddr,
		"alpha and beta must both resolve through the dns fixture's pinned address")

	require.NotNil(t, su.dns, "some stack must pin it")
	assert.Equal(t, "dns", su.dns.label,
		"the stack holding coredns is the one that pins the address, not one that names it")
}
