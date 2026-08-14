package test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// Timeouts. Bringing a stack up pulls or builds images, so it gets a lot of
// room; everything else is a single API call or a DNS query.
const (
	upTimeout      = 20 * time.Minute
	cleanupTimeout = 5 * time.Minute
	commandTimeout = 2 * time.Minute
	readyTimeout   = 3 * time.Minute
)

// dnsDomain is the TLD CoreDNS serves, exported to the fixtures as DNS_DOMAIN.
// Zones are <project>.<dnsDomain>.
const dnsDomain = "incus"

// corednsPort is where CoreDNS listens. resolv.conf has no port field, so an
// instance naming a resolver can only ever reach it on 53.
const corednsPort = "53"

// metricsPort is where the fixtures point COREDNS_HTTP, which serves
// /metrics, /health and /ready.
const metricsPort = "9153"

// requireTools skips the test when the machinery it drives is unavailable,
// rather than failing with something inscrutable.
func requireTools(t *testing.T) {
	t.Helper()

	for _, tool := range []string{"incus", "incus-compose"} {
		_, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not in PATH", tool)
		}
	}
}

// fixtureRoot is the compose stacks this suite boots, at the top of the
// checkout: they are the fleet, not this package's testdata.
func fixtureRoot(t *testing.T) string {
	t.Helper()

	return filepath.Join(repoRoot(t), "test", "fixtures")
}

// sanitize turns a name into something incus-compose's own sanitizing leaves
// alone, which is what keeps a zone predictable as <project>.incus.
func sanitize(name string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), "-")
}

// traceCommands echoes every command and everything it writes into the test log,
// off unless INCUS_COMPOSE_TEST_TRACE is set. The report helpers are the usual way.
var traceCommands = false

func init() {
	_, traceCommands = os.LookupEnv("INCUS_COMPOSE_TEST_TRACE")
}

// run executes a command with extra environment and returns what it wrote to
// stdout. What it wrote to stderr comes back on the error.
func run(ctx context.Context, t *testing.T, env []string, name string, args ...string) (string, error) {
	t.Helper()

	return runIn(ctx, t, "", env, name, args...)
}

// runIn is run with a working directory. A test binary runs in its own package
// directory, which is not where a person would be standing.
func runIn(ctx context.Context, t *testing.T, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()

	var out, stderr bytes.Buffer

	trace := io.Discard
	if traceCommands {
		trace = t.Output()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = io.MultiWriter(&out, trace)
	cmd.Stderr = io.MultiWriter(&stderr, trace)

	if traceCommands {
		// The extra environment goes on the line too, so it can be pasted into a
		// shell - most of what a fixture interpolates arrives that way.
		line := name + " " + strings.Join(args, " ")
		if len(env) > 0 {
			line = strings.Join(env, " ") + " " + line
		}

		fmt.Fprintf(t.Output(), "$ %s\n", line)
	}

	err := cmd.Run()

	// Carried on the error rather than logged, so a command that failed says why
	// wherever its error is reported, and one that worked says nothing at all.
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return out.String(), err
}

// composeConfig is one rendered `incus-compose config --format=json`.
//
// A fixture says what it wants once and the suite reads it. Every accessor
// tolerates a missing path, so a changed fixture fails on an assertion.
type composeConfig struct {
	raw map[string]any
}

// node walks the nested maps a decoded JSON document is made of, returning nil
// as soon as the path leaves the tree.
func (c composeConfig) node(path ...string) any {
	var value any = c.raw

	for _, key := range path {
		node, ok := value.(map[string]any)
		if !ok {
			return nil
		}

		value = node[key]
	}

	return value
}

// services is every service the fixture declares.
func (c composeConfig) services() map[string]any {
	out, _ := c.node("services").(map[string]any)

	return out
}

// nameserver is the resolver this fixture's services name, and whether they
// agree. incus-compose maps compose's dns: onto the instance's resolv.conf.
func (c composeConfig) nameserver() (string, bool, error) {
	want := ""

	for name, svc := range c.services() {
		node, _ := svc.(map[string]any)

		list, _ := node["dns"].([]any)
		if len(list) == 0 {
			continue
		}

		addr, _ := list[0].(string)
		if addr == "" {
			continue
		}

		if want == "" {
			want = addr

			continue
		}

		if want != addr {
			return "", false, fmt.Errorf("service %q resolves through %s, others through %s", name, addr, want)
		}
	}

	return want, want != "", nil
}

// pins reports whether some service holds addr on one of its networks, which is
// what makes a stack the one serving DNS rather than one naming it.
func (c composeConfig) pins(addr string) bool {
	for _, svc := range c.services() {
		node, _ := svc.(map[string]any)

		nets, _ := node["networks"].(map[string]any)
		for _, n := range nets {
			attach, _ := n.(map[string]any)

			got, _ := attach["ipv4_address"].(string)

			only, _, _ := strings.Cut(got, "/")
			if only == addr {
				return true
			}
		}
	}

	return false
}

// stack is one incus-compose project: one compose fixture, deployed.
type stack struct {
	suite *e2eSuite

	// label names the stack within its suite, and is what makes its Incus
	// project name unique.
	label string

	// src is the fixture in the repository; dir is the copy the run works in.
	src string
	dir string

	project string
	cfg     composeConfig

	// addrs maps a service to the addresses it holds, resolved once after the
	// stack is up.
	addrs map[string]map[string]string
}

// zone is the DNS zone this stack's project resolves to.
func (s *stack) zone() string { return s.project + "." + dnsDomain + "." }

// searchDomain is the zone without its trailing dot, which is what an instance
// needs in resolv.conf to resolve short names.
func (s *stack) searchDomain() string { return strings.TrimSuffix(s.zone(), ".") }

// dnsAddr is where this stack's instances send their queries. It belongs to the
// suite, since one server answers for every stack in it.
func (s *stack) dnsAddr() string { return s.suite.corednsAddr }

// compose runs incus-compose against this stack's fixture and project.
func (s *stack) compose(ctx context.Context, args ...string) (string, error) {
	s.suite.t.Helper()

	// --os-env is what lets a fixture read DNS_DOMAIN and COREFILE from the
	// environment set below.
	full := append([]string{
		"--debug",
		"--os-env",
		"-p", s.project,
		"-P", s.dir,
	}, args...)

	env := append([]string{
		"DNS_DOMAIN=" + dnsDomain,
		"DNS_SEARCH=" + s.searchDomain(),
	}, s.suite.env...)

	// From inside the project directory, which is where incus-compose is run by
	// hand. A test binary would otherwise run it from e2e/.
	return runIn(ctx, s.suite.t, s.dir, env, "incus-compose", full...)
}

// mustCompose fails the test if the command does not succeed.
func (s *stack) mustCompose(ctx context.Context, args ...string) string {
	s.suite.t.Helper()

	out, err := s.compose(ctx, args...)
	require.NoErrorf(s.suite.t, err, "incus-compose %s", strings.Join(args, " "))

	return out
}

// exec runs a command inside a service's instance.
func (s *stack) exec(ctx context.Context, service string, args ...string) (string, error) {
	s.suite.t.Helper()

	return s.compose(ctx, append([]string{"exec", "-T", service, "--"}, args...)...)
}

// dotenv reads the fixture's .env, the one place a stack's addresses are
// written. incus-compose interpolates compose.yaml from it natively.
func (s *stack) dotenv() map[string]string {
	t := s.suite.t
	t.Helper()

	out := map[string]string{}

	raw, err := os.ReadFile(filepath.Join(s.dir, ".env"))
	if os.IsNotExist(err) {
		return out
	}

	require.NoErrorf(t, err, "reading %s/.env", s.dir)

	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		// A fixture meant to be sourced by a shell writes `export K="v"`.
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		value = strings.Trim(strings.TrimSpace(value), `"'`)

		out[key] = value
	}

	return out
}

// render reads the fixture's config. It talks to no daemon, so it runs before
// anything is deployed and is what the suite locates CoreDNS from.
func (s *stack) render() {
	t := s.suite.t
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := s.compose(ctx, "config", "--format=json")
	require.NoErrorf(t, err, "rendering %s", s.dir)

	var raw map[string]any

	err = json.Unmarshal([]byte(out), &raw)
	require.NoErrorf(t, err, "parsing the config of %s", s.dir)

	s.cfg = composeConfig{raw: raw}
	require.NotEmptyf(t, s.cfg.services(), "%s declares no services", s.dir)
}

// e2eSuite is one end-to-end run.
type e2eSuite struct {
	t *testing.T

	// name is the project prefix, from the test name.
	name string

	// stacks is deploy order; teardown runs in reverse, so a stack referencing
	// another's network goes first.
	stacks []*stack

	// dns is the stack holding CoreDNS, and corednsAddr the address every other
	// stack's instances name.
	dns         *stack
	corednsAddr string

	// env is extra environment every stack's incus-compose call carries, which
	// is how a fixture is told about something made outside it.
	env []string

	// diagnose runs only when the test failed, and only while the stacks are
	// still standing.
	diagnose []func()
}

// export makes a value visible to every stack's compose file, which is how a
// fixture is told something only the suite knows.
func (s *e2eSuite) export(key, value string) {
	s.env = append(s.env, key+"="+value)
}

// onFailure registers something to report when the run fails. A plain t.Cleanup
// would run either after teardown or, when up aborts, never at all.
func (s *e2eSuite) onFailure(fn func()) {
	s.diagnose = append(s.diagnose, fn)
}

// distribute hands one stack's .env to every stack in the suite, so the address
// the DNS stack pins is written once rather than copied into three fixtures.
//
// It travels as process environment, which --os-env carries into the
// interpolation and which beats a fixture's own .env.
func (s *e2eSuite) distribute(from *stack) {
	s.t.Helper()

	keys := make([]string, 0, 4)

	env := from.dotenv()
	for key := range env {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		// The process environment wins, as compose-spec has it: a .env holds
		// defaults, and `COREDNS_DEBUG=true just test-e2e` has to beat them.
		_, set := os.LookupEnv(key)
		if set {
			continue
		}

		s.export(key, env[key])
	}
}

// newE2ESuite starts a suite named after the test. Nothing is deployed until up.
func newE2ESuite(t *testing.T) *e2eSuite {
	t.Helper()

	requireTools(t)

	s := &e2eSuite{t: t, name: sanitize(t.Name())}

	// The coredns image is built from the repository root, which a compose file
	// cannot name as a context relative to itself.
	s.export("PROJECT_DIR", repoRoot(t))

	// Per run, and never a path two runs share: a leftover would read as this
	// run's state - a spent certificate, or a cold store for another fleet.
	s.export("COREDNS_DATA_DIR", t.TempDir())

	// INCUS_REMOTE already reaches the CLI through the process environment;
	// forwarding it explicitly puts it in reach of compose interpolation too.
	remote := os.Getenv("INCUS_REMOTE")
	if remote != "" {
		s.export("INCUS_REMOTE", remote)
	}

	return s
}

// repoRoot is the absolute path of this checkout, derived from this file's own
// location so it does not depend on the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot determine the path of this source file")

	abs, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	require.NoError(t, err, "resolving the repository root")

	return abs
}

// add registers a compose fixture, with dir relative to test/fixtures/.
func (s *e2eSuite) add(label, dir string) *stack {
	s.t.Helper()

	st := &stack{
		suite:   s,
		label:   label,
		src:     filepath.Join(fixtureRoot(s.t), dir),
		dir:     filepath.Join(fixtureRoot(s.t), dir),
		project: s.name + "-" + sanitize(label),
	}

	_, err := os.Stat(filepath.Join(st.dir, "compose.yaml"))
	require.NoErrorf(s.t, err, "no compose.yaml under test/fixtures/%s", dir)

	s.stacks = append(s.stacks, st)

	return st
}

// locateDNS finds the address every instance resolves through, and the stack
// that pins it - a cross-fixture invariant nobody edits both sides of.
func (s *e2eSuite) locateDNS() {
	s.t.Helper()

	for _, st := range s.stacks {
		if st.cfg.raw == nil {
			st.render()
		}
	}

	want := ""

	for _, st := range s.stacks {
		addr, ok, err := st.cfg.nameserver()
		require.NoErrorf(s.t, err, "stack %q", st.label)

		if !ok {
			continue
		}

		if want == "" {
			want = addr

			continue
		}

		require.Equalf(s.t, want, addr, "stack %q resolves elsewhere", st.label)
	}

	require.NotEmpty(s.t, want, "no stack names a resolver through dns:")

	for _, st := range s.stacks {
		if st.cfg.pins(want) {
			s.dns = st

			break
		}
	}

	require.NotNilf(s.t, s.dns, "no stack pins %s, so nothing is listening on it", want)

	s.corednsAddr = want
}

// up brings every stack up in order and returns once each has its addresses.
func (s *e2eSuite) up() {
	s.t.Helper()

	s.locateDNS()
	s.createToken()

	// Teardown must not use t.Context(): it is canceled just before cleanups
	// run, which would kill `down` instantly and leave the project deployed.
	//
	// Reverse order, so a stack referencing another's network goes first, and
	// --volumes, or the next run inherits an already-spent trust token.
	s.t.Cleanup(func() {
		// A failed run leaves everything standing, because the state is the
		// evidence. INCUS_COMPOSE_TEST_KEEP forces the same for a passing one.
		_, keep := os.LookupEnv("INCUS_COMPOSE_TEST_KEEP")
		if keep || s.t.Failed() {
			for _, st := range s.stacks {
				s.t.Logf("keeping project %s, clean with: incus-compose -p %s -P %s down --volumes --project",
					st.project, st.project, st.src)
			}

			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()

		for i := len(s.stacks) - 1; i >= 0; i-- {
			st := s.stacks[i]

			_, err := st.compose(ctx, "down", "--volumes", "--project")
			assert.NoErrorf(s.t, err, "tearing down %s", st.project)
		}
	})

	// Registered after the teardown so it runs before it: a diagnostic is worth
	// nothing once the projects it would inspect are gone.
	s.t.Cleanup(func() {
		if !s.t.Failed() {
			return
		}

		for _, fn := range s.diagnose {
			fn()
		}
	})

	// Clear anything a previously killed run left behind, whose seeded volume
	// would otherwise supply a spent trust token to this one.
	preCtx, preCancel := context.WithTimeout(s.t.Context(), cleanupTimeout)

	for _, st := range s.stacks {
		_, _ = st.compose(preCtx, "down", "--project")
	}

	preCancel()

	upCtx, cancel := context.WithTimeout(s.t.Context(), upTimeout)
	defer cancel()

	for _, st := range s.stacks {
		st.mustCompose(upCtx, "up", "--detach", "--build", "--pull=always")
		st.resolveAddrs()
	}
}

// createToken issues one unrestricted trust token for the suite and revokes the
// certificate afterwards. One CoreDNS answers for every project, so one token.
func (s *e2eSuite) createToken() {
	s.t.Helper()

	ctx, cancel := context.WithTimeout(s.t.Context(), commandTimeout)
	defer cancel()

	out, err := run(ctx, s.t, nil, "incus", "config", "trust", "add", s.name)
	require.NoError(s.t, err, "minting a trust token")

	// Exported rather than written: the fixture declares it as a compose secret
	// from the environment, so it never reaches the checkout.
	s.export("INCUS_TOKEN", parseToken(s.t, out))

	s.t.Cleanup(func() {
		// Revoked on the same terms the stacks are torn down on: a kept stack
		// whose certificate has gone logs the revocation and nothing else.
		_, keep := os.LookupEnv("INCUS_COMPOSE_TEST_KEEP")
		if keep || s.t.Failed() {
			s.t.Logf("keeping certificate %s, revoke with: incus config trust remove %s", s.name, s.name)

			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()

		// The certificate is unrestricted, so leaving it behind matters.
		out, err := run(ctx, s.t, nil, "incus", "config", "trust", "list", "--format", "csv")
		if !assert.NoError(s.t, err, "listing trust store") {
			return
		}

		for _, line := range strings.Split(out, "\n") {
			fields := strings.Split(line, ",")
			if len(fields) < 4 || fields[0] != s.name {
				continue
			}

			_, err := run(ctx, s.t, nil, "incus", "config", "trust", "remove", fields[3])
			assert.NoErrorf(s.t, err, "revoking certificate %s", fields[3])
		}
	})
}

// parseToken pulls the base64 token out of `incus config trust add` output, so
// a format change fails here rather than as a connection error later.
func parseToken(t *testing.T, out string) string {
	t.Helper()

	for _, field := range strings.Fields(out) {
		if !strings.HasPrefix(field, "ey") {
			continue
		}

		padded := field + strings.Repeat("=", (4-len(field)%4)%4)

		raw, err := base64.StdEncoding.DecodeString(padded)
		if err != nil {
			continue
		}

		var probe struct {
			ClientName string `json:"client_name"`
		}

		err = json.Unmarshal(raw, &probe)
		if err != nil || probe.ClientName == "" {
			continue
		}

		return field
	}

	require.FailNowf(t, "no trust token found", "output:\n%s", out)

	return ""
}

// instanceJSON is the slice of `incus list --format json` this test needs.
type instanceJSON struct {
	Name  string `json:"name"`
	State struct {
		Network map[string]struct {
			Type      string `json:"type"`
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state"`
}

// resolveAddrs records every global address each service holds. JSON rather than
// csv, whose multi-line records drop all but a multi-homed instance's first.
func (s *stack) resolveAddrs() {
	t := s.suite.t
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()

	out, err := run(ctx, t, nil, "incus", "list", "--project", s.project, "--format", "json")
	require.NoError(t, err, "listing instances")

	var instances []instanceJSON

	err = json.Unmarshal([]byte(out), &instances)
	require.NoError(t, err, "parsing instance list")

	s.addrs = map[string]map[string]string{}

	for _, inst := range instances {
		service := serviceOf(inst.Name)
		if s.addrs[service] == nil {
			s.addrs[service] = map[string]string{}
		}

		for device, iface := range inst.State.Network {
			if device == "lo" || iface.Type == "loopback" {
				continue
			}

			for _, addr := range iface.Addresses {
				if addr.Scope != "global" {
					continue
				}

				s.addrs[service][addr.Address] = addr.Address
			}
		}
	}
}

// serviceOf strips the replica index incus-compose appends. These service names
// contain dashes, so only a trailing all-digit segment counts as an index.
func serviceOf(instance string) string {
	idx := strings.LastIndex(instance, "-")
	if idx < 0 {
		return instance
	}

	suffix := instance[idx+1:]
	if suffix == "" {
		return instance
	}

	for _, r := range suffix {
		if r < '0' || r > '9' {
			return instance
		}
	}

	return instance[:idx]
}

// addressesOf returns every address a service holds, sorted so failure messages
// are stable.
func (s *stack) addressesOf(service string) []string {
	t := s.suite.t
	t.Helper()

	out := make([]string, 0, len(s.addrs[service]))
	for addr := range s.addrs[service] {
		out = append(out, addr)
	}

	require.NotEmptyf(t, out, "no addresses recorded for service %q", service)

	sort.Strings(out)

	return out
}

// clientSubnetOf returns the IPv4 address to present as the querier's client
// subnet. IPv6 paired with /32 truncates into something matching no instance.
func (s *stack) clientSubnetOf(service string) string {
	t := s.suite.t
	t.Helper()

	for _, addr := range s.addressesOf(service) {
		if !strings.Contains(addr, ":") {
			return addr
		}
	}

	require.FailNowf(t, "no IPv4 address recorded", "service %q", service)

	return ""
}

// waitReady blocks until the plugin answers for this stack's zone, so a battery
// does not race the first sync. from names a service to ask from.
func (s *stack) waitReady(from string) {
	t := s.suite.t
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), readyTimeout)
	defer cancel()

	for {
		out, err := s.exec(ctx, from, "kdig", "@"+s.dnsAddr(), "-p", corednsPort,
			"+timeout=2", "+retry=0", s.zone(), "SOA")
		if err == nil && rcode(out) == "NOERROR" && answerCount(out) > 0 {
			return
		}

		select {
		case <-ctx.Done():
			require.FailNowf(t, "plugin never answered for the zone",
				"%s: %v\n%s", s.zone(), ctx.Err(), out)
		case <-time.After(2 * time.Second):
		}
	}
}

// counters reads the plugins' own metrics from inside an instance, because the
// host does not always have a path to the dns bridge.
func (s *stack) counters(ctx context.Context, from string) (map[string]float64, error) {
	url := "http://" + net.JoinHostPort(s.dnsAddr(), metricsPort) + "/metrics"

	body, err := s.exec(ctx, from, "curl", "-sS", "--max-time", "5", url)
	if err != nil {
		return nil, err
	}

	out := map[string]float64{}

	// Both prefixes: answering counters belong to the ecs_view engine, and
	// whatever the dns plugin publishes about serving is its own.
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "coredns_dns_") && !strings.HasPrefix(line, "coredns_ecs_view_") {
			continue
		}

		name, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}

		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}

		out[name] = f
	}

	return out, nil
}

// reportCounters dumps the plugins' counters, which tell an unattributable
// query from one that resolved to nothing. Both answer NXDOMAIN.
func (s *stack) reportCounters(t *testing.T, from, when string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), commandTimeout)
	defer cancel()

	got, err := s.counters(ctx, from)
	if err != nil {
		t.Logf("%s: reading metrics failed: %s", when, err)

		return
	}

	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}

	sort.Strings(names)

	t.Logf("%s: plugin counters", when)

	for _, name := range names {
		t.Logf("  %s = %g", name, got[name])
	}
}

// reportQuerier says what the querier looks like from the outside, and whether
// asking directly answered differently.
//
// The two paths differ only in which address identifies the querier, so direct
// succeeding where the resolver path failed is the whole story.
func (s *stack) reportQuerier(t *testing.T, from, target string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), commandTimeout)
	defer cancel()

	t.Logf("querier %s holds %v, direct mode presents %s",
		from, s.addressesOf(from), s.clientSubnetOf(from))

	out, err := s.ask(ctx, t, direct, from, target+"."+s.zone(), "A")
	if err != nil {
		t.Logf("querier %s: the direct comparison failed: %s", from, err)

		return
	}

	t.Logf("querier %s -> %s asked directly: %s", from, target, rcode(out))
}

// TestLocateDNS covers the config read on its own: `config` renders a fixture
// and talks to no daemon, so this stands nothing up.
func TestLocateDNS(t *testing.T) {
	testlib.SkipE2E(t)

	su := newE2ESuite(t)
	su.add("shop", "shop")
	su.locateDNS()

	addr, err := netip.ParseAddr(su.corednsAddr)
	require.NoError(t, err, "the fixtures must name a literal address")
	assert.True(t, addr.Is4(), "instances resolve through an IPv4 address")

	require.NotNil(t, su.dns, "some stack must pin that address")
	assert.Equal(t, "shop", su.dns.label, "the shop fixture still carries coredns")

	port, err := strconv.Atoi(corednsPort)
	require.NoError(t, err)
	assert.Positive(t, port)
}

// networkJSON is the slice of `incus network list --format json` that decides
// visibility: the plugin keys a network by the project that owns it, so two
// projects referencing one bridge only intersect if both report the same pair.
type networkJSON struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Managed bool   `json:"managed"`
	Type    string `json:"type"`
}

// reportNetworks dumps the networks this stack's project can see, as owner/name
// pairs. Two stacks sharing a bridge must agree on one pair.
func (s *stack) reportNetworks(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), commandTimeout)
	defer cancel()

	out, err := run(ctx, t, nil, "incus", "network", "list", "--project", s.project, "--format", "json")
	if err != nil {
		t.Logf("%s: listing networks failed: %s", s.project, err)

		return
	}

	var nets []networkJSON

	err = json.Unmarshal([]byte(out), &nets)
	if err != nil {
		t.Logf("%s: parsing networks failed: %s", s.project, err)

		return
	}

	t.Logf("%s sees these networks, as the plugin keys them:", s.project)

	for _, n := range nets {
		if !n.Managed {
			continue
		}

		t.Logf("  %s/%s (type %s)", n.Project, n.Name, n.Type)
	}
}

// reportServerLog dumps the DNS server's console, which nothing else in this
// suite collects. COREDNS_DEBUG=true adds a line per query.
func (s *stack) reportServerLog(t *testing.T, service string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), commandTimeout)
	defer cancel()

	out, err := run(ctx, t, nil, "incus", "console", "--show-log",
		service+"-1", "--project", s.project)
	if err != nil {
		t.Logf("%s: reading the %s console failed: %s", s.project, service, err)

		return
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	// The tail is where the queries are, not the banner and connection retries.
	const tail = 120
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	t.Logf("%s: last %d lines of the %s console:", s.project, len(lines), service)

	for _, line := range lines {
		t.Logf("  %s", line)
	}
}

// reportInstances dumps every instance with its zone, its addresses, and the
// Incus network behind each NIC - the table a visibility question turns on.
func (s *stack) reportInstances(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), commandTimeout)
	defer cancel()

	out, err := run(ctx, t, nil, "incus", "list", "--project", s.project, "--format", "json")
	if err != nil {
		t.Logf("%s: listing instances failed: %s", s.project, err)

		return
	}

	var instances []struct {
		Name    string                       `json:"name"`
		Devices map[string]map[string]string `json:"expanded_devices"`
		State   struct {
			Network map[string]struct {
				Type      string `json:"type"`
				Addresses []struct {
					Family  string `json:"family"`
					Address string `json:"address"`
					Scope   string `json:"scope"`
				} `json:"addresses"`
			} `json:"network"`
		} `json:"state"`
	}

	err = json.Unmarshal([]byte(out), &instances)
	if err != nil {
		t.Logf("%s: parsing instances failed: %s", s.project, err)

		return
	}

	t.Logf("%s (zone %s):", s.project, s.zone())

	for _, inst := range instances {
		for dev, iface := range inst.State.Network {
			if dev == "lo" || iface.Type == "loopback" {
				continue
			}

			network := inst.Devices[dev]["network"]

			for _, addr := range iface.Addresses {
				if addr.Scope != "global" || addr.Family != "inet" {
					continue
				}

				t.Logf("  %-14s %-5s %-16s network=%s", inst.Name, dev, addr.Address, network)
			}
		}
	}
}
