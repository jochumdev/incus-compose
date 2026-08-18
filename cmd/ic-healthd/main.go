// ic-healthd monitors incus-compose instances with healthchecks and restarts unhealthy ones.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/avast/retry-go/v5"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/cmd/ic-healthd/version"
	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/shared"
)

const (
	defaultDataDir    = "/var/lib/ic-healthd"
	defaultSecretsDir = "/run/secrets"
)

const (
	certFile  = "client.crt"
	keyFile   = "client.key"
	tokenFile = "token"
)

func main() {
	app := newRootCommand()

	if err := app.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func newRootCommand() *cli.Command {
	return &cli.Command{
		Name:  "ic-healthd",
		Usage: "Health check daemon for incus-compose",
		Commands: []*cli.Command{
			newRunCommand(),
			newVersionCommand(),
		},
	}
}

func newRunCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Run the health check daemon",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "incus",
				Usage:   "URL of the incus api",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_INCUS"),
			},
			&cli.StringFlag{
				Name:    "token",
				Usage:   "Token for registering our cert (use for debugging only)",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_TOKEN"),
			},
			&cli.StringSliceFlag{
				Name:    "project",
				Usage:   "Project(s) to manage; empty means every visible project carrying --project-marker",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_PROJECTS"),
			},
			&cli.StringFlag{
				Name:    "project-marker",
				Usage:   "Project config `KEY=VALUE` that opts a project in when --project is empty; a bare KEY means KEY=true",
				Value:   defaultProjectMarker,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_PROJECT_MARKER"),
			},
			&cli.StringFlag{
				Name:    "own-project",
				Usage:   "Project the daemon's own container runs in",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_OWN_PROJECT"),
			},
			&cli.StringFlag{
				Name:    "own-name",
				Usage:   "The daemon's own instance name; empty means it skips itself",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_OWN_NAME"),
			},
			&cli.StringFlag{
				Name:    "data-dir",
				Usage:   "Persistent volume directory containing the generated cert/key",
				Value:   defaultDataDir,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_DATA_DIR"),
			},
			&cli.StringFlag{
				Name:    "secrets-dir",
				Usage:   "Tmpfs directory containing the one-time registration token",
				Value:   defaultSecretsDir,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_SECRETS_DIR"),
			},
			&cli.IntFlag{
				Name:    "workers",
				Usage:   "Health checks to run at once, over every watched project",
				Value:   defaultWorkers,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_WORKERS"),
			},
			&cli.IntFlag{
				Name:    "restart-workers",
				Usage:   "Restarts to run at once, over every watched project",
				Value:   defaultRestartWorkers,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_RESTART_WORKERS"),
			},
			&cli.BoolFlag{
				Name:    "debug",
				Usage:   "Enable verbose logging",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_DEBUG"),
			},
			&cli.BoolFlag{
				Name:    "trace",
				Usage:   "Enable per-event logging, which implies --debug",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_TRACE"),
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			lvl := logLevel(cmd.Bool("debug"), cmd.Bool("trace"))

			logger := slog.New(newLogHandler(cmd.Writer, lvl))
			ctx = withLogger(ctx, logger)

			for range 10 {
				if hasDefaultRoute(procNetRoute) {
					return ctx, nil
				}
				time.Sleep(time.Second)
			}
			return ctx, nil
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			log := logger(ctx)
			cfg := configFromCommand(cmd)

			log.Info("Version", "version", version.Current(), "pid", os.Getpid())

			redacted := cfg
			redacted.Token = fmt.Sprintf("<redacted-(%d)>", len(cfg.Token))
			log.Debug("My config", "config", redacted)

			return mainAction(ctx, cfg)
		},
	}
}

// configFromCommand is here so we can test it.
func configFromCommand(cmd *cli.Command) config {
	marker, value := parseProjectMarker(cmd.String("project-marker"))

	return config{
		DataDir:            cmd.String("data-dir"),
		SecretsDir:         cmd.String("secrets-dir"),
		IncusURL:           cmd.String("incus"),
		Token:              cmd.String("token"),
		Projects:           cmd.StringSlice("project"),
		ProjectMarker:      marker,
		ProjectMarkerValue: value,
		OwnProject:         cmd.String("own-project"),
		OwnName:            cmd.String("own-name"),

		Workers:        cmd.Int("workers"),
		RestartWorkers: cmd.Int("restart-workers"),
	}
}

// parseProjectMarker splits the marker; a bare key means "true".
func parseProjectMarker(marker string) (key, value string) {
	key, value, found := strings.Cut(marker, "=")
	if !found {
		return key, "true"
	}

	return key, value
}

func newVersionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print version information",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			fmt.Printf("ic-healthd version %s\n", version.Current())
			return nil
		},
	}
}

// dial opens a connection to the Incus API with the daemon's client cert.
//
// The server certificate cannot be pinned: the daemon is handed a URL and a
// token and has never seen the server before.
func dial(cfg config, certPEM, keyPEM []byte) (*iclient.Connection, error) {
	return iclient.NewConnection(&iclient.ConfigRemoteInfo{
		Name:               "incus",
		Addrs:              []string{cfg.IncusURL},
		Protocol:           "incus",
		ClientCert:         string(certPEM),
		ClientKey:          string(keyPEM),
		InsecureSkipVerify: true,
		UserAgent:          "ic-healthd/" + version.Current(),
	})
}

// register has Incus trust a self-signed cert via the one-time token, then persists it.
func register(ctx context.Context, cfg config) (*iclient.Connection, error) {
	logger := logger(ctx)

	certPEM, keyPEM, err := generateClientCert()
	if err != nil {
		return nil, fmt.Errorf("generating cert: %w", err)
	}

	conn, err := dial(cfg, certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("connecting to register cert: %w", err)
	}

	post := incusApi.CertificatesPost{
		CertificatePut: incusApi.CertificatePut{Name: "ic-healthd-" + cfg.ID()},
		TrustToken:     cfg.Token,
	}

	// A restricted certificate needs a fixed project list, which a daemon that
	// watches by marker does not have.
	if len(cfg.Projects) > 0 {
		post.Restricted = true
		post.Projects = cfg.Projects
	}

	registerCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	err = conn.CreateCertificate(registerCtx, post)
	if err != nil {
		return nil, fmt.Errorf("registering cert with token: %w", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data-dir %v: %w", cfg.DataDir, err)
	}

	keyDataPath := filepath.Join(cfg.DataDir, keyFile)
	if err := os.WriteFile(keyDataPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("saving key %v: %w", keyDataPath, err)
	}

	certDataPath := filepath.Join(cfg.DataDir, certFile)
	if err := os.WriteFile(certDataPath, certPEM, 0o600); err != nil {
		return nil, fmt.Errorf("saving cert %v: %w", certDataPath, err)
	}

	logger.Debug("certificate registered and persisted")

	// Reading /1.0 is lazy, so this connection knows nothing it learned while
	// untrusted and needs no redial.
	return conn, nil
}

// connect returns an authenticated Incus client, registering a cert on first run.
func connect(ctx context.Context, cfg config) (*iclient.Connection, error) {
	logger := logger(ctx)

	tokenPath := filepath.Join(cfg.SecretsDir, tokenFile)

	certDataPath := filepath.Join(cfg.DataDir, certFile)
	keyDataPath := filepath.Join(cfg.DataDir, keyFile)

	if !fileExists(certDataPath) && (cfg.Token != "" || fileExists(tokenPath)) {
		logger.Debug("Fresh token performing first-run registration")

		if cfg.Token == "" {
			tokenBytes, err := os.ReadFile(tokenPath)
			if err != nil {
				return nil, fmt.Errorf("reading token: %w", err)
			}

			cfg.Token = strings.TrimSpace(string(tokenBytes))
			if cfg.Token == "" {
				return nil, errors.New("token file is empty")
			}

			logger.Debug("Got the token from a file", "length", len(cfg.Token))
		}

		conn, err := register(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("first-run registration: %w", err)
		}

		return conn, nil
	} else if !fileExists(keyDataPath) || !fileExists(certDataPath) {
		return nil, fmt.Errorf("no token and no registration happened before")
	}

	logger.Debug("Reusing persisted cert from data dir")

	certPEM, err := os.ReadFile(certDataPath)
	if err != nil {
		return nil, fmt.Errorf("reading cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyDataPath)
	if err != nil {
		return nil, fmt.Errorf("reading key: %w", err)
	}

	return dial(cfg, certPEM, keyPEM)
}

// discoverProject re-reads the whole project on its own goroutine and closes
// with a roster, so the loop can drop the instances that are gone.
func discoverProject(ctx context.Context, conn *iclient.Connection, results chan<- instanceResult) {
	go func() {
		send := func(res instanceResult) bool {
			select {
			case results <- res:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var instances []incusApi.InstanceFull

		// A project that is briefly unreachable is the common case, so retry
		// before reporting a failure the loop can only log.
		err := retry.New(
			retry.Context(ctx),
			retry.Attempts(4),
			retry.Delay(time.Second),
			retry.LastErrorOnly(true),
		).Do(func() error {
			callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
			defer cancel()

			got, err := conn.GetInstances(callCtx, nil)

			instances = got

			return err
		})
		if err != nil {
			send(instanceResult{kind: instanceResultRoster, err: err})
			return
		}

		names := make([]string, 0, len(instances))
		for _, inst := range instances {
			names = append(names, inst.Name)

			cfg, err := parseInstanceConfig(&inst.Instance)
			if !send(instanceResult{
				kind:   instanceResultDiscovered,
				name:   inst.Name,
				config: cfg,
				status: inst.Config[shared.HealthStatusKey],
				err:    err,
			}) {
				return
			}
		}

		// Last, so the roster only prunes what this pass really did not see.
		send(instanceResult{kind: instanceResultRoster, names: names})
	}()
}

// projectScheduler runs one project's health loop until ctx is done.
func projectScheduler(ctx context.Context, conn *iclient.Connection, pool *pools, project string, events <-chan instanceEvent) {
	logger := logger(ctx).With("project", project)
	ctx = withLogger(ctx, logger)

	// Scoped once here rather than per call, so every read this scheduler makes
	// is in its own project.
	conn = conn.WithProject(project)

	instances := map[string]*instance{}
	results := make(chan instanceResult, resultBuffer)

	discoverProject(ctx, conn, results)

	nextCheck := time.NewTimer(time.Hour)
	defer nextCheck.Stop()

	for {
		earliest := runInstanceActions(ctx, conn, pool, instances, results)
		nextCheck.Stop()
		if earliest.IsZero() {
			nextCheck.Reset(time.Hour)
		} else {
			nextCheck.Reset(time.Until(earliest))
		}

		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if ev.Action == instanceResync {
				logger.Debug("Resyncing the project")
				discoverProject(ctx, conn, results)

				continue
			}

			handleInstanceEvent(ctx, conn, instances, results, ev)
		case res := <-results:
			handleInstanceResult(ctx, conn, instances, results, res)
		case <-nextCheck.C:
			_ = runInstanceActions(ctx, conn, pool, instances, results)
		}
	}
}

// decodeLifecycle turns one raw event into a routable one, reporting false for
// everything the daemon does not act on.
func decodeLifecycle(ctx context.Context, raw incusApi.Event) (lifecycleEvent, bool) {
	log := logger(ctx)

	var lc incusApi.EventLifecycle

	err := json.Unmarshal(raw.Metadata, &lc)
	if err != nil {
		log.Warn("Decoding lifecycle event", "error", err)

		return lifecycleEvent{}, false
	}

	ev := lifecycleEvent{Project: raw.Project}

	switch lc.Action {
	case incusApi.EventLifecycleProjectCreated:
		ev.ProjectAction = projectCreated
	case incusApi.EventLifecycleProjectUpdated:
		ev.ProjectAction = projectUpdated
	case incusApi.EventLifecycleProjectDeleted:
		ev.ProjectAction = projectDeleted
	case incusApi.EventLifecycleProjectRenamed:
		ev.ProjectAction = projectRenamed

		// A project event names nothing: Project and Name are both empty on
		// it, so the old name is only ever in the context.
		old, ok := lc.Context["old_name"].(string)
		if !ok || old == "" {
			log.Warn("Project renamed without an old name", "project", raw.Project)

			return lifecycleEvent{}, false
		}

		ev.OldName = old
	default:
		if lc.Name == "" {
			return lifecycleEvent{}, false
		}

		switch lc.Action {
		// A resume leaves the instance in the shape a start does, and it is
		// the only thing that takes one parked by a pause back off the shelf.
		case incusApi.EventLifecycleInstanceStarted, incusApi.EventLifecycleInstanceRestarted,
			incusApi.EventLifecycleInstanceResumed:
			ev.Instance.Action = instanceRestarted
		case incusApi.EventLifecycleInstanceUpdated:
			ev.Instance.Action = instanceUpdated
		case incusApi.EventLifecycleInstanceStopped, incusApi.EventLifecycleInstanceShutdown:
			ev.Instance.Action = instanceStopped
		case incusApi.EventLifecycleInstanceDeleted:
			ev.Instance.Action = instanceDeleted
		default:
			return lifecycleEvent{}, false
		}

		ev.Instance.Instance = lc.Name
	}

	log.Log(ctx, levelTrace, "New lifecycle event", "project", raw.Project, "instance", lc.Name, "action", lc.Action)

	return ev, true
}

// projectData is one watched project's handle on the router side.
type projectData struct {
	cancel context.CancelFunc
	events chan instanceEvent

	// done closes when the scheduler has returned, so a routed send cannot
	// block forever on a project that is on its way out.
	done chan struct{}
}

// runProjects owns the listener, the watched projects and their events. One
// generation is one listener; schedulers hang off ctx and outlive a reconnect.
func runProjects(ctx context.Context, conn *iclient.Connection, cfg config, reload <-chan struct{}) {
	log := logger(ctx)

	pool, err := newPools(cfg.Workers, cfg.RestartWorkers)
	if err != nil {
		log.Error("Building the worker pools", "error", err)

		return
	}

	defer releasePools(pool)

	log.Debug("Worker pools", "checks", cfg.Workers, "restarts", cfg.RestartWorkers)

	projects := map[string]*projectData{}

	// The registry lives on this goroutine alone, which is what lets start and
	// stop run without a lock.
	start := func(name string) {
		if _, ok := projects[name]; ok {
			return
		}

		log.Info("Watching project", "project", name)

		pCtx, cancel := context.WithCancel(ctx)
		p := &projectData{
			cancel: cancel,
			events: make(chan instanceEvent, projectBuffer),
			done:   make(chan struct{}),
		}

		go func() {
			defer close(p.done)

			projectScheduler(pCtx, conn, pool, name, p.events)
		}()

		projects[name] = p
	}

	// Deliberately does not wait on done: a wedged scheduler must not be able
	// to hang the router that is trying to be rid of it.
	stop := func(name string) {
		p, ok := projects[name]
		if !ok {
			return
		}

		log.Info("Dropping project", "project", name)

		p.cancel()
		delete(projects, name)
	}

	// inScope answers for one project. A false is re-checked below: incusd sends
	// project-updated before applying it, so a fast read sees the old config.
	inScope := func(name string) bool {
		if len(cfg.Projects) > 0 {
			return slices.Contains(cfg.Projects, name)
		}

		// Bounded because this runs on the router goroutine.
		callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
		defer cancel()

		project, _, err := conn.GetProject(callCtx, name)
		if err != nil {
			log.Warn("Reading a project failed, not watching it", "project", name, "error", err)
			return false
		}

		return project.Config[cfg.ProjectMarker] == cfg.ProjectMarkerValue
	}

	// recheck holds the projects that lost that race, against the tries each has
	// left; a zero recheckAt means none are due, so a quiet server arms nothing.
	recheck := map[string]int{}
	var recheckAt time.Time

	recheckTimer := time.NewTimer(time.Hour)
	defer recheckTimer.Stop()

	// deferRecheck schedules another look at a project that read out of scope.
	deferRecheck := func(name string) {
		if _, ok := recheck[name]; ok {
			return
		}

		recheck[name] = scopeRecheckTries
		if recheckAt.IsZero() {
			recheckAt = time.Now().Add(scopeRecheckDelay)
		}
	}

	for {
		// evCtx is one listener generation: closing it closes the event socket.
		// The schedulers hang off ctx and are untouched by it.
		evCtx, evCancel := context.WithCancel(ctx)

		events, err := conn.ListenEvents(evCtx, []string{incusApi.EventTypeLifecycle}, true)
		if err != nil {
			log.Error("Opening the event listener", "error", err)
			evCancel()

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}

		// Reconcile against the listener, not before it, so a project created
		// in the gap arrives as an event rather than being missed by both.
		scope := cfg.Projects
		if len(scope) == 0 {
			scopeCtx, scopeCancel := context.WithTimeout(evCtx, apiTimeout)
			all, err := conn.GetProjects(scopeCtx)
			scopeCancel()

			if err != nil {
				evCancel()
				log.Error("Listing projects", "error", err)

				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
					continue
				}
			}

			scope = make([]string, 0, len(all))
			for _, project := range all {
				if project.Config[cfg.ProjectMarker] == cfg.ProjectMarkerValue {
					scope = append(scope, project.Name)
				}
			}
		}

		for name := range projects {
			if !slices.Contains(scope, name) {
				stop(name)
			}
		}

		for _, name := range scope {
			start(name)
		}

		if len(projects) == 0 {
			log.Info("Watching no project",
				"marker", cfg.ProjectMarker, "value", cfg.ProjectMarkerValue,
				"configured", len(cfg.Projects))
		}

		// Whatever happened while there was no listener is invisible, so every
		// surviving scheduler re-reads its project.
		for _, p := range projects {
			select {
			case p.events <- instanceEvent{Action: instanceResync}:
			case <-p.done:
			case <-ctx.Done():
				evCancel()

				return
			}
		}

		generation := true
		for generation {
			recheckTimer.Stop()
			if recheckAt.IsZero() {
				recheckTimer.Reset(time.Hour)
			} else {
				recheckTimer.Reset(time.Until(recheckAt))
			}

			select {
			case <-ctx.Done():
				evCancel()

				return

			case <-recheckTimer.C:
				for name, tries := range recheck {
					if inScope(name) {
						delete(recheck, name)
						start(name)

						continue
					}

					if tries--; tries <= 0 {
						delete(recheck, name)

						continue
					}

					recheck[name] = tries
				}

				recheckAt = time.Time{}
				if len(recheck) > 0 {
					recheckAt = time.Now().Add(scopeRecheckDelay)
				}

			case <-reload:
				log.Info("Manual resync requested")
				evCancel()
				generation = false

			case raw, open := <-events:
				// The channel closes with the socket, which is the only way a
				// generation ends other than a reload.
				if !open {
					log.Warn("Event listener disconnected, reconnecting")
					evCancel()
					generation = false

					continue
				}

				ev, ok := decodeLifecycle(evCtx, raw)
				if !ok {
					continue
				}

				if ev.ProjectAction != "" {
					switch ev.ProjectAction {
					case projectCreated, projectUpdated:
						if inScope(ev.Project) {
							delete(recheck, ev.Project)
							start(ev.Project)
						} else {
							stop(ev.Project)
							deferRecheck(ev.Project)
						}
					case projectDeleted:
						delete(recheck, ev.Project)
						stop(ev.Project)
					case projectRenamed:
						// Incus only renames empty projects, so nothing is lost. A
						// restricted daemon 403s until incusd refreshes its cache.
						log.Info("Project renamed", "from", ev.OldName, "to", ev.Project)

						delete(recheck, ev.OldName)
						stop(ev.OldName)

						if inScope(ev.Project) {
							start(ev.Project)
						} else {
							deferRecheck(ev.Project)
						}
					}

					continue
				}

				p, ok := projects[ev.Project]
				if !ok {
					continue
				}

				select {
				case p.events <- ev.Instance:
				case <-p.done:
				case <-ctx.Done():
					evCancel()

					return
				}
			}
		}
	}
}

func mainAction(ctx context.Context, cfg config) error {
	logger := logger(ctx)

	ctx, cancel := context.WithCancel(ctx)

	reload := make(chan struct{}, 1)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigChan)

	go func() {
		for {
			select {
			case sig := <-sigChan:
				switch sig {
				case syscall.SIGHUP:
					slog.Info("received signal, reloading", "signal", sig)
					select {
					case reload <- struct{}{}:
					default:
						slog.Debug("reload already pending")
					}
				default:
					slog.Info("received signal, shutting down", "signal", sig)
					cancel()
					return
				}
			case <-ctx.Done():
				cancel()
				return
			}
		}
	}()

	var conn *iclient.Connection

	for conn == nil {
		c, err := connect(ctx, cfg)
		if err != nil {
			logger.Error("connecting to incus", "error", err)

			select {
			case <-ctx.Done():
				cancel()
				return fmt.Errorf("failed to connect to incus: %w", err)
			case <-time.After(time.Second):
				continue
			}
		}

		conn = c
	}

	ownConn := conn.WithProject(cfg.OwnProject)

	logger.Info("Health daemon connected")

	if cfg.OwnProject != "" && cfg.OwnName != "" {
		err := writeInstanceStatus(ctx, ownConn, cfg.OwnName, shared.HealthStatusHealthy)
		if err != nil {
			cancel()
			return fmt.Errorf("failed to update my status: %w", err)
		}
	}

	runProjects(ctx, conn, cfg, reload)

	if cfg.OwnProject != "" && cfg.OwnName != "" {
		// ctx is already done on the way out, so the write needs its own budget.
		shutCtx, shutCancel := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
		statusErr := writeInstanceStatus(shutCtx, ownConn, cfg.OwnName, shared.HealthStatusUnhealthy)
		shutCancel()

		if statusErr != nil {
			logger.Warn("Failed to update my status", "error", statusErr)
		}
	}

	cancel()

	return nil
}
