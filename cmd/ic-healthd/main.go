// Command ic-healthd runs health checks and restart policies on incus-compose
// instances, as an ievent chain configured by flags.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"go.uber.org/automaxprocs/maxprocs"

	// Importing this package sets GOMEMLIMIT automatically from the cgroup memory limit.
	// By default, it sets GOMEMLIMIT to 90% of the cgroup memory limit.
	// Set the AUTOMEMLIMIT environment variable to a ratio in (0.0, 1.0], or "off".
	_ "github.com/KimMachineGun/automemlimit"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/checker"
	"github.com/lxc/incus-compose/ievent/source"
	"github.com/lxc/incus-compose/incustrust"
	"github.com/lxc/incus-compose/shared"
)

// certName is what this binary registers itself as in the Incus trust store,
// and what the daemon logs the connection as.
const certName = "ic-healthd"

// drainTimeout bounds handing on what the chain still holds at shutdown. Only
// an action that ignored its cancel ever takes this long.
const drainTimeout = 30 * time.Second

// statusTimeout bounds one write of the daemon's own health status.
const statusTimeout = 30 * time.Second

func main() {
	err := command().Run(context.Background(), os.Args)
	if err != nil {
		// A usage error happens before there is a logger worth the name.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// command is the whole command line.
func command() *cli.Command {
	return &cli.Command{
		Name:  "ic-healthd",
		Usage: "Health check daemon for incus-compose",
		Commands: []*cli.Command{
			runCommand(newConfig()),
			versionCommand(),
		},
	}
}

// runCommand is the flags and the environment together: every flag reads
// INCUS_COMPOSE_HEALTHD_<NAME> when it is not given.
func runCommand(cfg *config) *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Run the health check daemon until told to stop",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "incus",
				Usage:       "URL of the Incus API",
				Destination: &cfg.IncusURL,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_INCUS"),
			},
			&cli.StringFlag{
				Name:        "token",
				Usage:       "One-time trust token; a token file under --secrets-dir is read when this is empty",
				Destination: &cfg.Token,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_TOKEN"),
			},
			&cli.StringFlag{
				Name: "data-dir",
				Usage: "Persistent directory holding the enrolled certificate; " +
					"empty keeps none",
				Value:       defaultDataDir,
				Destination: &cfg.DataDir,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_DATA_DIR"),
			},
			&cli.StringFlag{
				Name:        "secrets-dir",
				Usage:       "Tmpfs directory holding the one-time trust token",
				Value:       defaultSecretsDir,
				Destination: &cfg.SecretsDir,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_SECRETS_DIR"),
			},
			&cli.StringFlag{
				Name:        "client-cert",
				Usage:       "Certificate to present instead of enrolling; needs --client-key",
				Destination: &cfg.ClientCert,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_CLIENT_CERT"),
			},
			&cli.StringFlag{
				Name:        "client-key",
				Usage:       "Key for --client-cert",
				Destination: &cfg.ClientKey,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_CLIENT_KEY"),
			},
			&cli.StringFlag{
				Name:        "remote",
				Usage:       "Connect as a remote from the Incus CLI configuration; needs --use-remote, empty means the default remote",
				Destination: &cfg.Remote,
				Sources:     cli.EnvVars("INCUS_REMOTE"),
			},
			&cli.BoolFlag{
				Name:        "use-remote",
				Usage:       "Allow the Incus CLI configuration to be used when there is no certificate and no token",
				Destination: &cfg.UseRemote,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_USE_REMOTE"),
			},

			&cli.StringSliceFlag{
				Name:        "project",
				Usage:       "Project(s) to watch; empty means every visible project carrying --project-marker",
				Destination: &cfg.Projects,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_PROJECTS"),
			},
			&cli.StringFlag{
				Name:    "project-marker",
				Usage:   "Project config `KEY=VALUE` that opts a project in when --project is empty; a bare KEY means KEY=true",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_PROJECT_MARKER"),
				Action: func(ctx context.Context, c *cli.Command, s string) error {
					marker, value := parseMarker(s)
					cfg.ProjectMarker = marker
					cfg.ProjectMarkerValue = value

					return nil
				},
			},

			&cli.StringFlag{
				Name:        "own-project",
				Usage:       "Project the daemon's own container runs in",
				Destination: &cfg.OwnProject,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_OWN_PROJECT"),
			},
			&cli.StringFlag{
				Name:        "own-name",
				Usage:       "The daemon's own instance name; empty means it skips itself",
				Destination: &cfg.OwnName,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_OWN_NAME"),
			},

			&cli.IntFlag{
				Name:        "workers",
				Usage:       "Health checks in flight at once, over every watched project",
				Destination: &cfg.Workers,
				Value:       defaultWorkers,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_WORKERS"),
			},
			&cli.IntFlag{
				Name:        "restart-workers",
				Usage:       "Restarts in flight at once, over every watched project",
				Destination: &cfg.RestartWorkers,
				Value:       defaultRestartWorkers,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_RESTART_WORKERS"),
			},
			&cli.StringFlag{
				Name:        "http-address",
				Usage:       "Address to serve /metrics, /health and /ready on; empty disables it",
				Destination: &cfg.HTTPAddr,
				Value:       defaultHTTPAddr,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_HTTP_ADDRESS"),
			},
			&cli.BoolFlag{
				Name:        "metrics",
				Usage:       "Serve Prometheus metrics on /metrics",
				Value:       true,
				Destination: &cfg.Metrics,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_METRICS"),
			},

			&cli.BoolFlag{
				Name:        "debug",
				Usage:       "Enable verbose logging",
				Destination: &cfg.Debug,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_DEBUG"),
			},
			&cli.BoolFlag{
				Name:        "trace",
				Usage:       "Enable per-event logging, which implies --debug",
				Destination: &cfg.Trace,
				Sources:     cli.EnvVars("INCUS_COMPOSE_HEALTHD_TRACE"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			level := slog.LevelInfo
			if cfg.Debug {
				level = slog.LevelDebug
			}
			if cfg.Trace {
				level = shared.LevelTrace
			}

			logger := slog.New(newLogHandler(cmd.ErrWriter, level))

			// Container CPU limits are a quota, not a core count, so GOMAXPROCS has to
			// be told.
			undo, err := maxprocs.Set(maxprocs.Logger(func(format string, args ...any) {
				logger.Info(fmt.Sprintf(format, args...))
			}))
			if err != nil {
				logger.Warn("setting GOMAXPROCS", "err", err)
			}

			defer undo()

			logger.Info("Starting",
				"version", version,
				"pid", os.Getpid(),
				"incus", cfg.endpoint(),
				"http", cfg.HTTPAddr,
			)

			// One attribute each rather than a struct printed with %+v, so a single
			// field can be grepped out.
			logger.Debug("configuration",
				"projects", cfg.Projects,
				"project_marker", cfg.ProjectMarker+"="+cfg.ProjectMarkerValue,
				"own_project", cfg.OwnProject,
				"own_name", cfg.OwnName,
				"data_dir", cfg.DataDir,
				"secrets_dir", cfg.SecretsDir,
				"token", cfg.redacted().Token,
				"workers", cfg.Workers,
				"restart_workers", cfg.RestartWorkers,
				"metrics", cfg.Metrics,
			)

			return run(ctx, logger, cfg)
		},
	}
}

// versionCommand prints what this build is.
func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print version information",
		Action: func(_ context.Context, _ *cli.Command) error {
			fmt.Println("ic-healthd version", version)

			return nil
		},
	}
}

// run is everything with a lifetime, in the order it starts and the reverse of
// the order it stops.
func run(ctx context.Context, logger *slog.Logger, cfg *config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := connect(ctx, logger, cfg)
	if err != nil {
		return err
	}

	if cfg.OwnProject != "" && cfg.OwnName != "" {
		err := writeOwnStatus(ctx, logger, conn, cfg, shared.HealthStatusHealthy)
		if err != nil {
			return fmt.Errorf("writing my own health status: %w", err)
		}
	}

	plugins, runners := chain(logger, cfg)

	// Wiring only: nothing is dialed and no goroutine starts, so a configuration
	// that cannot work is refused before anything is running.
	src, err := source.New(logger, conn, plugins)
	if err != nil {
		return fmt.Errorf("building the source: %w", err)
	}

	// Two contexts, because the source and the chain do not stop at the same
	// time. main owns every goroutine, so the shutdown order is written down here.
	sourceCtx, stopSource := context.WithCancel(ctx)

	var srcWg, pluginWg sync.WaitGroup

	startSource := func() {
		srcWg.Go(func() {
			err := src.Run(sourceCtx)
			if err != nil {
				logger.Error("running the source", "err", err)
				cancel()
			}
		})
	}

	startSource()

	for _, r := range runners {
		pluginWg.Go(func() {
			err := r.Run(ctx)
			if err != nil {
				logger.Error("running a plugin", "plugin", r.Name(), "err", err)
				cancel()
			}

			// It will answer nothing now, so a drain waiting on it stops.
			src.Finished(r)
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	defer signal.Stop(sig)

	for {
		select {
		case s := <-sig:
			if s == syscall.SIGHUP {
				// A manual resync: end the generation, start a fresh one. The
				// reconnect is what the enricher re-sweeps on, and the checker
				// keeps its state across it.
				logger.Info("manual resync requested", "signal", s.String())

				stopSource()
				srcWg.Wait()

				sourceCtx, stopSource = context.WithCancel(ctx)
				startSource()

				continue
			}

			logger.Info("shutting down", "signal", s.String())
		case <-ctx.Done():
		}

		break
	}

	// The source stops first, so nothing new enters the chain; Drain then asks
	// each plugin in turn; cancel is the abort for whatever ignored the question.
	stopSource()
	srcWg.Wait()

	src.Drain(drainContext(ctx))

	cancel()
	pluginWg.Wait()

	if cfg.OwnProject != "" && cfg.OwnName != "" {
		// ctx is already done on the way out, so the write needs its own budget.
		err := writeOwnStatus(context.WithoutCancel(ctx), logger, conn, cfg, shared.HealthStatusUnhealthy)
		if err != nil {
			logger.Warn("writing my own health status on the way out", "err", err)
		}
	}

	return nil
}

// connect retries until Incus answers: the sidecar can start ahead of the
// daemon it dials, and a token is only spent once an enrollment lands. A
// configuration that cannot authenticate is refused at once, not retried.
func connect(ctx context.Context, logger *slog.Logger, cfg *config) (*iclient.Connection, error) {
	trust := incustrust.Config{
		Name:       certName,
		UserAgent:  certName + "/" + version,
		URL:        cfg.IncusURL,
		ClientCert: cfg.ClientCert,
		ClientKey:  cfg.ClientKey,
		Token:      cfg.Token,
		DataDir:    cfg.DataDir,
		SecretsDir: cfg.SecretsDir,
		Remote:     cfg.Remote,
		UseRemote:  cfg.UseRemote,
	}

	for {
		conn, err := incustrust.Connect(ctx, trust)
		if err == nil {
			logger.Info("Connected to Incus")

			return conn, nil
		}

		if errors.Is(err, incustrust.ErrNoCredentials) {
			return nil, err
		}

		logger.Error("connecting to Incus", "err", err)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connecting to Incus: %w", err)
		case <-time.After(time.Second):
		}
	}
}

// writeOwnStatus is the daemon's report on itself, the one status write that
// is not the checker's.
func writeOwnStatus(ctx context.Context, logger *slog.Logger, conn *iclient.Connection, cfg *config, status string) error {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	return checker.WriteStatus(ctx, logger, conn, cfg.OwnProject, cfg.OwnName, status)
}

// drainContext bounds the shutdown with a budget of its own, because ctx is
// what a plugin aborts on and draining is the opposite of that.
func drainContext(ctx context.Context) context.Context {
	out, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)

	// The caller returns straight after Drain, so the budget dies with it.
	context.AfterFunc(out, cancel)

	return out
}

// newLogHandler names the trace level, so it does not print as slog's
// "DEBUG-4".
func newLogHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key != slog.LevelKey {
				return a
			}

			if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == shared.LevelTrace {
				a.Value = slog.StringValue("TRACE")
			}

			return a
		},
	})
}
