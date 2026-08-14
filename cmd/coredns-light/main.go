// Command coredns-light is cmd/coredns with nothing behind the engine: it is
// authoritative for the zones it builds and refuses every other name, so there
// is no --forward and no cache, loop or forward position to serve it with.
//
// Everything else is the same binary - the same flags, the same environment,
// and the same data directory, so a deployment can be moved either way without
// re-enrolling.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"go.uber.org/automaxprocs/maxprocs"

	ievlog "github.com/lxc/incus-compose/ievent/log"
	"github.com/lxc/incus-compose/ievent/source"
	"github.com/lxc/incus-compose/incustrust"
)

// certName is what this binary registers itself as in the Incus trust store,
// and what the daemon logs the connection as.
const certName = "coredns-incus"

// drainTimeout bounds handing on what the chain still holds at shutdown. Only a
// plugin that has stopped answering ever takes this long.
const drainTimeout = 30 * time.Second

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
		Name:  "coredns-incus",
		Usage: "DNS for Incus instances, per querier",
		Commands: []*cli.Command{
			runCommand(),
			versionCommand(),
		},
	}
}

// runCommand is the flags and the environment together: every flag reads
// COREDNS_<NAME> when it is not given. Apart from command() so a test can drive it.
func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Serve DNS until told to stop",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "incus",
				Usage:   "URL of the Incus API",
				Sources: cli.EnvVars("COREDNS_INCUS"),
			},
			&cli.StringFlag{
				Name:    "token",
				Usage:   "One-time trust token; a token file under --secrets-dir is read when this is empty",
				Sources: cli.EnvVars("COREDNS_TOKEN"),
			},
			&cli.StringFlag{
				Name: "data-dir",
				Usage: "Persistent directory holding the enrolled certificate and " +
					"what was last served; empty keeps neither",
				Value:   defaultDataDir,
				Sources: cli.EnvVars("COREDNS_DATA_DIR"),
			},
			&cli.StringFlag{
				Name:    "secrets-dir",
				Usage:   "Tmpfs directory holding the one-time trust token",
				Value:   defaultSecretsDir,
				Sources: cli.EnvVars("COREDNS_SECRETS_DIR"),
			},
			&cli.StringFlag{
				Name:    "client-cert",
				Usage:   "Certificate to present instead of enrolling; needs --client-key",
				Sources: cli.EnvVars("COREDNS_CLIENT_CERT"),
			},
			&cli.StringFlag{
				Name:    "client-key",
				Usage:   "Key for --client-cert",
				Sources: cli.EnvVars("COREDNS_CLIENT_KEY"),
			},
			&cli.BoolFlag{
				Name:    "restricted",
				Usage:   "Enroll a certificate confined to --project; off means one server answers for every visible project",
				Sources: cli.EnvVars("COREDNS_RESTRICTED"),
			},
			&cli.StringFlag{
				Name:    "remote",
				Usage:   "Connect as a remote from the Incus CLI configuration; needs --use-remote, empty means the default remote",
				Sources: cli.EnvVars("INCUS_REMOTE"),
			},
			&cli.BoolFlag{
				Name:    "use-remote",
				Usage:   "Allow the Incus CLI configuration to be used when there is no certificate and no token",
				Sources: cli.EnvVars("COREDNS_USE_REMOTE"),
			},

			&cli.StringFlag{
				Name:    "suffix",
				Usage:   "TLD every project's zone is built under",
				Value:   defaultSuffix,
				Sources: cli.EnvVars("COREDNS_SUFFIX"),
			},
			&cli.StringSliceFlag{
				Name:    "project",
				Usage:   "Project(s) to serve; empty means every visible project carrying --project-marker",
				Sources: cli.EnvVars("COREDNS_PROJECTS"),
			},
			&cli.StringFlag{
				Name:    "project-marker",
				Usage:   "Project config `KEY=VALUE` that opts a project in when --project is empty; a bare KEY means KEY=true",
				Value:   defaultProjectMarker,
				Sources: cli.EnvVars("COREDNS_PROJECT_MARKER"),
			},

			&cli.StringFlag{
				Name:    "listen",
				Usage:   "Address to answer DNS on, UDP and TCP",
				Value:   defaultDNSAddr,
				Sources: cli.EnvVars("COREDNS_LISTEN"),
			},
			&cli.StringFlag{
				Name:    "http",
				Usage:   "Address to serve /metrics, /health and /ready on; empty disables it",
				Value:   defaultHTTPAddr,
				Sources: cli.EnvVars("COREDNS_HTTP"),
			},

			&cli.UintFlag{
				Name:    "ttl",
				Usage:   "Seconds a record is served for; 0-" + strconv.Itoa(maxTTL),
				Value:   defaultTTL,
				Sources: cli.EnvVars("COREDNS_TTL"),
			},
			&cli.DurationFlag{
				Name:    "debounce-window",
				Usage:   "How long a key must be quiet before the last of its burst is handed on",
				Value:   defaultDebounceWindow,
				Sources: cli.EnvVars("COREDNS_DEBOUNCE_WINDOW"),
			},
			&cli.IntFlag{
				Name:    "workers",
				Usage:   "Incus reads in flight at once",
				Value:   defaultWorkers,
				Sources: cli.EnvVars("COREDNS_WORKERS"),
			},
			&cli.DurationFlag{
				Name:    "read-timeout",
				Usage:   "Budget for one read of the daemon",
				Value:   defaultReadTimeout,
				Sources: cli.EnvVars("COREDNS_READ_TIMEOUT"),
			},
			&cli.DurationFlag{
				Name:    "sweep-interval",
				Usage:   "Gap between whole-fleet passes",
				Value:   defaultSweepInterval,
				Sources: cli.EnvVars("COREDNS_SWEEP_INTERVAL"),
			},
			&cli.BoolFlag{
				Name:    "echo-subnet",
				Usage:   "Echo the RFC 7871 client subnet back on replies",
				Sources: cli.EnvVars("COREDNS_ECHO_SUBNET"),
			},
			&cli.BoolFlag{
				Name:    "metrics",
				Usage:   "Record the engine's counters and gauges; off leaves them registered at zero",
				Sources: cli.EnvVars("COREDNS_METRICS"),
			},
			&cli.StringSliceFlag{
				Name:    "exclude",
				Usage:   "Chain position(s) to leave out; only the optional ones, and an unknown name is an error",
				Sources: cli.EnvVars("COREDNS_EXCLUDE"),
			},

			&cli.BoolFlag{
				Name:    "debug",
				Usage:   "Verbose logging",
				Sources: cli.EnvVars("COREDNS_DEBUG"),
			},
			&cli.BoolFlag{
				Name:    "trace",
				Usage:   "Per-event logging in arrival order, which implies --debug",
				Sources: cli.EnvVars("COREDNS_TRACE"),
			},
			&cli.BoolFlag{
				Name:    "pprof",
				Usage:   "Serve /debug/pprof on the --http address; for profiling, never for a deployment",
				Sources: cli.EnvVars("COREDNS_PPROF"),
			},
		},
		Action: action,
	}
}

// versionCommand prints what this build is.
func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print version information",
		Action: func(_ context.Context, _ *cli.Command) error {
			fmt.Println("coredns-incus version", version)

			return nil
		},
	}
}

// action turns the command line into a running process.
func action(ctx context.Context, cmd *cli.Command) error {
	cfg := configFromCommand(cmd)

	err := cfg.validate()
	if err != nil {
		return err
	}

	// --trace implies --debug, or the position it adds logs at a level the
	// handler drops.
	level := slog.LevelInfo
	if cfg.Debug || cfg.Trace {
		level = slog.LevelDebug
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrWriter, &slog.HandlerOptions{Level: level})))

	// CoreDNS's own logger, and ecs_view's, arriving as slog records.
	ievlog.Hook(level)

	// Container CPU limits are a quota, not a core count, so GOMAXPROCS has to
	// be told.
	undo, err2 := maxprocs.Set(maxprocs.Logger(func(format string, args ...any) {
		slog.Info(fmt.Sprintf(format, args...))
	}))
	if err2 != nil {
		slog.Warn("setting GOMAXPROCS", "err", err2)
	}

	defer undo()

	slog.Info("Starting",
		"version", version,
		"pid", os.Getpid(),
		"incus", cfg.endpoint(),
		"dns", cfg.DNSAddr,
		"http", cfg.HTTPAddr,
	)

	// One attribute each rather than a struct printed with %+v, so a single
	// field can be grepped out.
	slog.Debug("configuration",
		"projects", cfg.Projects,
		"project_marker", cfg.ProjectMarker+"="+cfg.ProjectMarkerValue,
		"ttl", cfg.TTL,
		"data_dir", cfg.DataDir,
		"secrets_dir", cfg.SecretsDir,
		"token", cfg.redacted().Token,
		"debounce_window", cfg.DebounceWindow,
		"workers", cfg.Workers,
		"read_timeout", cfg.ReadTimeout,
		"sweep_interval", cfg.SweepInterval,
		"echo_subnet", cfg.EchoSubnet,
		"metrics", cfg.Metrics,
		"pprof", cfg.Pprof,
	)

	return run(ctx, cfg)
}

// run is everything with a lifetime, in the order it starts and the reverse of
// the order it stops.
func run(ctx context.Context, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	plugins, runners, err := assemble(chain(cfg), cfg.Exclude)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Name())
	}

	slog.Info("chain", "plugins", names)

	conn, err := incustrust.Connect(ctx, incustrust.Config{
		Name:       certName,
		UserAgent:  certName + "/" + version,
		URL:        cfg.IncusURL,
		ClientCert: cfg.ClientCert,
		ClientKey:  cfg.ClientKey,
		Token:      cfg.Token,
		DataDir:    cfg.DataDir,
		SecretsDir: cfg.SecretsDir,
		Restricted: cfg.Restricted,
		Projects:   cfg.Projects,
		Remote:     cfg.Remote,
		UseRemote:  cfg.UseRemote,
	})
	if err != nil {
		return fmt.Errorf("connecting to Incus: %w", err)
	}

	// Wiring only: nothing is dialed and no goroutine starts, so a configuration
	// that cannot work is refused before anything is running.
	src, err := source.New(ctx, conn, plugins)
	if err != nil {
		return fmt.Errorf("building the source: %w", err)
	}

	// Two contexts, because the source and the chain do not stop at the same
	// time. main owns every goroutine, so the shutdown order is written down here.
	sourceCtx, stopSource := context.WithCancel(ctx)
	defer stopSource()

	var srcWg, pluginWg sync.WaitGroup

	srcWg.Go(func() {
		err := src.Run(sourceCtx)
		if err != nil {
			slog.Error("running the source", "err", err)
			cancel()
		}
	})

	for _, r := range runners {
		pluginWg.Go(func() {
			err := r.Run(ctx)
			if err != nil {
				slog.Error("running a plugin", "plugin", r.Name(), "err", err)
				cancel()
			}

			// It will answer nothing now, so a drain waiting on it stops.
			src.Finished(r)
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(sig)

	select {
	case s := <-sig:
		slog.Info("shutting down", "signal", s.String())
	case <-ctx.Done():
	}

	// The source stops first, so nothing new enters the chain; Drain then asks
	// each plugin in turn; cancel is the abort for whatever ignored the question.
	stopSource()
	srcWg.Wait()

	src.Drain(drainContext(ctx))

	cancel()
	pluginWg.Wait()

	return nil
}

// drainContext bounds the shutdown with a budget of its own, because ctx is
// what a plugin aborts on and draining is the opposite of that.
func drainContext(ctx context.Context) context.Context {
	out, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)

	// The caller returns straight after Drain, so the budget dies with it.
	context.AfterFunc(out, cancel)

	return out
}

// configFromCommand is apart from the action so it can be tested.
func configFromCommand(cmd *cli.Command) config {
	marker, value := parseMarker(cmd.String("project-marker"))

	return config{
		IncusURL:   cmd.String("incus"),
		Token:      cmd.String("token"),
		DataDir:    cmd.String("data-dir"),
		SecretsDir: cmd.String("secrets-dir"),
		ClientCert: cmd.String("client-cert"),
		ClientKey:  cmd.String("client-key"),
		Restricted: cmd.Bool("restricted"),
		Remote:     cmd.String("remote"),
		UseRemote:  cmd.Bool("use-remote"),

		Suffix:             cmd.String("suffix"),
		Projects:           cmd.StringSlice("project"),
		ProjectMarker:      marker,
		ProjectMarkerValue: value,

		DNSAddr:  cmd.String("listen"),
		HTTPAddr: cmd.String("http"),

		TTL:            uint32(cmd.Uint("ttl")),
		DebounceWindow: cmd.Duration("debounce-window"),
		Workers:        cmd.Int("workers"),
		ReadTimeout:    cmd.Duration("read-timeout"),
		SweepInterval:  cmd.Duration("sweep-interval"),
		EchoSubnet:     cmd.Bool("echo-subnet"),
		Metrics:        cmd.Bool("metrics"),
		Exclude:        cmd.StringSlice("exclude"),

		Debug: cmd.Bool("debug"),
		Trace: cmd.Bool("trace"),
		Pprof: cmd.Bool("pprof"),
	}
}
