package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// newToken mints the trust token incus-compose hands the sidecar. Incus takes
// the name and the scope from here, so projects is what bounds the daemon;
// empty mints an unrestricted token.
func newToken(t *testing.T, c *client.Client, projects ...string) string {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	// A token operation never finishes, only its first update carries the token.
	tokenCtx, release := context.WithCancel(t.Context())
	defer release()

	updates, err := conn.CreateCertificateToken(tokenCtx, incusApi.CertificatesPost{
		CertificatePut: incusApi.CertificatePut{
			Name:       "ic-healthd-" + c.IncusProject(),
			Type:       "client",
			Restricted: len(projects) > 0,
			Projects:   projects,
		},
		Token: true,
	})
	require.NoError(t, err)

	opAPI, ok := <-updates
	require.True(t, ok, "the certificate token operation reported nothing")

	addToken, err := opAPI.ToCertificateAddToken()
	require.NoError(t, err)

	return addToken.String()
}

// markProject sets a project config key, the way incus-compose stamps
// HealthEnabledKey on every project it creates.
func markProject(t *testing.T, c *client.Client, key, value string) {
	t.Helper()

	gConn, err := c.GlobalConnection()
	require.NoError(t, err)

	project, etag, err := gConn.GetProject(t.Context(), c.IncusProject())
	require.NoError(t, err)

	writable := project.Writable()
	if writable.Config == nil {
		writable.Config = incusApi.ConfigMap{}
	}

	writable.Config[key] = value

	require.NoError(t, gConn.UpdateProject(t.Context(), c.IncusProject(), writable, etag))
}

// revokeCert removes what registration left in the trust store.
func revokeCert(t *testing.T, c *client.Client) {
	t.Helper()

	gConn, err := c.GlobalConnection()
	if err != nil {
		return
	}

	// t.Context is already canceled by the time cleanup runs.
	ctx := context.Background()

	certs, err := gConn.GetCertificates(ctx)
	if err != nil {
		return
	}

	want := "ic-healthd-" + c.IncusProject()
	for _, cert := range certs {
		if cert.Name == want {
			_ = gConn.DeleteCertificate(ctx, cert.Fingerprint)
		}
	}
}

// healthdConfig is what incus-compose injects into the sidecar. projects both
// scopes the token and fills the project list; no projects means an
// unrestricted token, for the dynamic-scope cases.
//
// Mint exactly one token per config: they share a name, so a second one leaves
// two registrations racing for it.
func healthdConfig(t *testing.T, c *client.Client, projects ...string) config {
	t.Helper()

	url, ok := os.LookupEnv("INCUS_COMPOSE_HEALTHD_INCUS")
	if !ok {
		if !c.IsRemote() {
			t.Skip("healthd registers over HTTPS; set INCUS_COMPOSE_HEALTHD_INCUS for a socket remote")
		}

		url = c.Config().URL
	}

	return config{
		DataDir:            t.TempDir(),
		SecretsDir:         t.TempDir(),
		IncusURL:           url,
		Token:              newToken(t, c, projects...),
		Projects:           projects,
		ProjectMarker:      shared.HealthEnabledKey,
		ProjectMarkerValue: "true",
	}
}

// runDaemon starts everything the sidecar runs bar the signal handling: the one
// shared lifecycle listener, the router and a scheduler per watched project. It
// returns the reload channel, which starts a fresh generation the way SIGHUP does.
func runDaemon(t *testing.T, c *client.Client, cfg config) chan<- struct{} {
	t.Helper()

	t.Cleanup(func() { revokeCert(t, c) })

	// The daemon takes its logger from the context, so everything it says
	// lands under this test instead of in the shared stderr of a parallel run.
	log := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(withLogger(t.Context(), log))

	conn, err := connect(ctx, cfg)
	require.NoError(t, err)

	done := make(chan struct{})
	reload := make(chan struct{}, 1)

	go func() {
		defer close(done)

		runProjects(ctx, conn, cfg, reload)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not stop when its context was canceled")
		}
	})

	return reload
}

// runScheduler watches one project by name, the way a sidecar created by
// incus-compose does.
func runScheduler(t *testing.T, c *client.Client) {
	t.Helper()

	_ = runDaemon(t, c, healthdConfig(t, c, c.IncusProject()))
}

// requireStatus waits for the daemon's verdict to land on the instance, and
// reports what it last saw.
func requireStatus(t *testing.T, conn *iclient.Connection, name, want string, within time.Duration) {
	t.Helper()

	var status, state string

	ok := assert.Eventually(t, func() bool {
		inst, _, err := conn.GetInstance(t.Context(), name, nil)
		if err != nil {
			state = err.Error()
			return false
		}

		status = inst.Config[shared.HealthStatusKey]
		state = inst.Status

		return status == want
	}, within, 500*time.Millisecond)

	require.True(t, ok, "instance %s should report %s; last seen status %q while %s",
		name, want, status, state)
}

// setState drives an instance from the test side and waits for the change.
// Every raw write here races the daemon's status writes for the instance's
// operation lock, so it waits the lock out and retries.
func setState(t *testing.T, conn *iclient.Connection, name string, req incusApi.InstanceStatePut) {
	t.Helper()

	err := retry.New(
		retry.Attempts(10),
		retry.Delay(250*time.Millisecond),
		retry.LastErrorOnly(true),
		retry.RetryIf(func(err error) bool {
			return errors.Is(err, iclient.ErrInstanceBusy)
		}),
	).Do(func() error {
		err := conn.WaitInstanceBusy(t.Context(), name)
		if err != nil {
			return err
		}

		op, err := conn.UpdateInstanceState(t.Context(), name, req, "")
		if err != nil {
			return err
		}

		_, err = iclient.WaitOperation(t.Context(), op)

		return err
	})
	require.NoError(t, err)
}

// TestE2EConnectRegistersThenReuses covers the whole first-run dance: consume
// the one-time token, persist the pair, and come back without a token.
func TestE2EConnectRegistersThenReuses(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-connect-")

	cfg := healthdConfig(t, c, c.IncusProject())
	t.Cleanup(func() { revokeCert(t, c) })

	conn, err := connect(t.Context(), cfg)
	require.NoError(t, err)

	_, err = conn.WithProject(c.IncusProject()).GetInstanceNames(t.Context(), nil)
	require.NoError(t, err, "the registered certificate must be usable against its project")

	require.FileExists(t, filepath.Join(cfg.DataDir, certFile))
	require.FileExists(t, filepath.Join(cfg.DataDir, keyFile))

	// A restart of the sidecar has no token left: it must reuse the pair.
	reuse := cfg
	reuse.Token = ""

	conn, err = connect(t.Context(), reuse)
	require.NoError(t, err, "a restarted daemon must reuse its persisted certificate")

	_, err = conn.WithProject(c.IncusProject()).GetInstanceNames(t.Context(), nil)
	require.NoError(t, err)
}

// TestE2EConnectRegistersFromATokenFile covers how the sidecar really gets its
// token: a file in secrets-dir, not the environment.
func TestE2EConnectRegistersFromATokenFile(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-connect-file-")

	cfg := healthdConfig(t, c, c.IncusProject())
	t.Cleanup(func() { revokeCert(t, c) })

	// The trailing newline is deliberate: a written token file has one.
	require.NoError(t, os.WriteFile(
		filepath.Join(cfg.SecretsDir, tokenFile), []byte(cfg.Token+"\n"), 0o600))
	cfg.Token = ""

	conn, err := connect(t.Context(), cfg)
	require.NoError(t, err)

	_, err = conn.WithProject(c.IncusProject()).GetInstanceNames(t.Context(), nil)
	require.NoError(t, err, "a certificate registered from a token file must be usable")
}

// TestE2EConnectWithoutTokenOrCert pins the guard that keeps a misconfigured
// sidecar from looking healthy while it can do nothing.
func TestE2EConnectWithoutTokenOrCert(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-connect-bare-")

	cfg := healthdConfig(t, c, c.IncusProject())
	cfg.Token = ""

	_, err := connect(t.Context(), cfg)
	require.Error(t, err, "with no token and nothing persisted there is no way to authenticate")
}

// TestE2ESchedulerReportsHealthy is the path incus-compose waits on for
// depends_on: { condition: service_healthy }.
func TestE2ESchedulerReportsHealthy(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-healthy-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
	}), true)

	runScheduler(t, c)

	requireStatus(t, testConn(t, c), name, shared.HealthStatusHealthy, 60*time.Second)
}

// TestE2ESchedulerReportsUnhealthyAfterRetries pins that a single failure is not
// a verdict: the status only turns after the configured run of failures.
func TestE2ESchedulerReportsUnhealthyAfterRetries(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-unhealthy-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/false"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
		"restart":  "no",
	}), true)

	runScheduler(t, c)

	requireStatus(t, testConn(t, c), name, shared.HealthStatusUnhealthy, 60*time.Second)
}

// TestE2ESchedulerRestartsACrashedInstance covers a stop nobody asked for: the
// raw API stop leaves no intent marker, so the restart policy applies.
func TestE2ESchedulerRestartsACrashedInstance(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-crash-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
		"restart":  "always",
	}), true)

	conn := testConn(t, c)
	runScheduler(t, c)

	requireStatus(t, conn, name, shared.HealthStatusHealthy, 60*time.Second)

	setState(t, conn, name, incusApi.InstanceStatePut{Action: "stop", Timeout: -1, Force: true})

	require.Eventually(t, func() bool {
		state, _, err := conn.GetInstanceState(t.Context(), name)
		return err == nil && state.StatusCode == incusApi.Running
	}, 90*time.Second, time.Second, "a crashed instance should be restarted")
}

// TestE2EMultipleProjects is the multi-project feature itself: one daemon, one
// lifecycle listener, and a verdict in each project.
func TestE2EMultipleProjects(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	one := testProject(t, "healthd-multi-a-")
	two := testProject(t, "healthd-multi-b-")

	keys := healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
	})

	nameOne := testContainer(t, one, "web", keys, true)
	nameTwo := testContainer(t, two, "web", keys, true)

	runDaemon(t, one, healthdConfig(t, one, one.IncusProject(), two.IncusProject()))

	requireStatus(t, testConn(t, one), nameOne, shared.HealthStatusHealthy, 60*time.Second)
	requireStatus(t, testConn(t, two), nameTwo, shared.HealthStatusHealthy, 60*time.Second)
}

// TestE2EReloadKeepsWatching covers a fresh listener generation, which is what a
// reconnect and a SIGHUP both do. It asserts through a scheduler: a crash after
// the reload is only repaired if that scheduler is alive and still fed.
func TestE2EReloadKeepsWatching(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-reload-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
		"restart":  "always",
	}), true)

	conn := testConn(t, c)
	reload := runDaemon(t, c, healthdConfig(t, c, c.IncusProject()))

	requireStatus(t, conn, name, shared.HealthStatusHealthy, 60*time.Second)

	reload <- struct{}{}

	setState(t, conn, name, incusApi.InstanceStatePut{Action: "stop", Timeout: -1, Force: true})

	require.Eventually(t, func() bool {
		state, _, err := conn.GetInstanceState(t.Context(), name)
		return err == nil && state.StatusCode == incusApi.Running
	}, 90*time.Second, time.Second,
		"a crash after a reload must still be repaired: the scheduler outlives the listener")
}

// TestE2EDynamicScope covers the daemon started with no project list: it watches
// what carries the marker and leaves everything else alone.
func TestE2EDynamicScope(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	marked := testProject(t, "healthd-scope-on-")
	plain := testProject(t, "healthd-scope-off-")

	// A key of our own, so this also proves the marker is not hardcoded.
	const marker = "user.healthcheck.watched-by-this-test"

	markProject(t, marked, marker, "true")

	keys := healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
	})

	watched := testContainer(t, marked, "web", keys, true)
	ignored := testContainer(t, plain, "web", keys, true)

	// The token allows both projects, so only the marker can be what keeps the
	// unmarked one out.
	cfg := healthdConfig(t, marked, marked.IncusProject(), plain.IncusProject())
	cfg.Projects = nil
	cfg.ProjectMarker = marker

	runDaemon(t, marked, cfg)

	requireStatus(t, testConn(t, marked), watched, shared.HealthStatusHealthy, 60*time.Second)

	// The unmarked project is visible to the certificate and still not watched,
	// which is the whole point of the marker.
	conn := testConn(t, plain)
	require.Never(t, func() bool {
		inst, _, err := conn.GetInstance(t.Context(), ignored, nil)
		return err == nil && inst.Config[shared.HealthStatusKey] == shared.HealthStatusHealthy
	}, 20*time.Second, time.Second, "a project without the marker must not be watched")
}

// TestE2EProjectRenamed covers the router's rename branch: stop watching the
// old name, pick up the new one. Incus only renames empty projects.
//
// The token is unrestricted on purpose: a rename does not refresh incusd's
// certificate cache, so a restricted daemon stays filtered on the old name.
func TestE2EProjectRenamed(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-rename-")

	const marker = "user.healthcheck.watched-by-rename-test"

	markProject(t, c, marker, "true")

	// No projects: an unrestricted token, so the daemon still sees the project
	// under its new name.
	cfg := healthdConfig(t, c)
	cfg.ProjectMarker = marker

	runDaemon(t, c, cfg)

	gConn, err := c.GlobalConnection()
	require.NoError(t, err)

	// Let the first generation settle so the rename lands as an event rather
	// than as part of the startup sweep.
	require.Eventually(t, func() bool {
		_, _, err := gConn.GetProject(t.Context(), c.IncusProject())
		return err == nil
	}, 30*time.Second, time.Second)

	renamed := c.IncusProject() + "-renamed"

	op, err := gConn.RenameProject(t.Context(), c.IncusProject(), incusApi.ProjectPost{Name: renamed})
	require.NoError(t, err, "only empty projects can be renamed; this one must still be empty")

	_, err = iclient.WaitOperation(t.Context(), op)
	require.NoError(t, err)

	t.Cleanup(func() { _ = gConn.DeleteProject(context.Background(), renamed, nil) })

	// The daemon must be watching the new name: an instance created there gets
	// a verdict, which only a scheduler for that project can produce.
	renamedClient, err := c.Global().EnsureProject(renamed)
	require.NoError(t, err)

	name := testContainer(t, renamedClient, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
	}), true)

	requireStatus(t, testConn(t, renamedClient), name, shared.HealthStatusHealthy, 90*time.Second)
}

// TestE2EStatusIsRepairedAfterAnotherWriter pins that the daemon notices the
// status key moving under it: it only writes on a transition, so a value it did
// not write must still end up corrected.
func TestE2EStatusIsRepairedAfterAnotherWriter(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-restatus-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
	}), true)

	conn := testConn(t, c)
	runScheduler(t, c)

	requireStatus(t, conn, name, shared.HealthStatusHealthy, 60*time.Second)

	require.NoError(t, conn.WaitInstanceBusy(t.Context(), name))

	// Exactly what client.Instance.SetHealthCheckingStopped writes on a start.
	require.NoError(t, patchInstanceConfig(t.Context(), conn, name,
		map[string]string{shared.HealthStatusKey: shared.HealthStatusStarting}))

	requireStatus(t, conn, name, shared.HealthStatusHealthy, 60*time.Second)
}

// TestE2ENoBounceAfterAnExternalStart covers the shape of `incus-compose
// restart`: a stop, then a start by someone else before the daemon's backoff
// elapses, so the restart it queued must not fire.
func TestE2ENoBounceAfterAnExternalStart(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-bounce-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
		"restart":  "always",
	}), true)

	conn := testConn(t, c)
	runScheduler(t, c)

	requireStatus(t, conn, name, shared.HealthStatusHealthy, 60*time.Second)

	setState(t, conn, name, incusApi.InstanceStatePut{Action: "stop", Timeout: -1, Force: true})
	setState(t, conn, name, incusApi.InstanceStatePut{Action: "start", Timeout: -1})

	state, _, err := conn.GetInstanceState(t.Context(), name)
	require.NoError(t, err)
	require.NotZero(t, state.Pid, "a running container has an init pid, which is what a bounce would change")

	pid := state.Pid

	// Well past the 5s backoff floor this instance's interval and retries give.
	require.Never(t, func() bool {
		state, _, err := conn.GetInstanceState(t.Context(), name)
		return err != nil || state.StatusCode != incusApi.Running || state.Pid != pid
	}, 45*time.Second, time.Second, "an instance that came back on its own must not be restarted again")
}

// TestE2ESchedulerHonoursAnIntentionalStop pins the inverse: `incus-compose
// stop` marks the instance, and unless-stopped must leave it alone.
func TestE2ESchedulerHonoursAnIntentionalStop(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	c := testProject(t, "healthd-marked-")
	name := testContainer(t, c, "web", healthKeys(map[string]string{
		"test":     `["CMD","/bin/true"]`,
		"interval": "2s",
		"timeout":  "5s",
		"retries":  "2",
		"restart":  "unless-stopped",
	}), true)

	conn := testConn(t, c)
	runScheduler(t, c)

	requireStatus(t, conn, name, shared.HealthStatusHealthy, 60*time.Second)

	require.NoError(t, conn.WaitInstanceBusy(t.Context(), name))

	require.NoError(t, patchInstanceConfig(t.Context(), conn, name,
		map[string]string{shared.HealthStoppedKey: "true"}))

	setState(t, conn, name, incusApi.InstanceStatePut{Action: "stop", Timeout: -1, Force: true})

	// Outlast the 5s backoff floor several times over.
	require.Never(t, func() bool {
		state, _, err := conn.GetInstanceState(t.Context(), name)
		return err == nil && state.StatusCode == incusApi.Running
	}, 45*time.Second, time.Second, "a deliberately stopped instance must stay stopped")
}
