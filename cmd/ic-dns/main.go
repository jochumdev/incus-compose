// Command ic-dns is the ievent-based DNS server for Incus. It uses CoreDNS's
// plugins but composes the chain itself, at compile time, configured by flags.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"go.uber.org/automaxprocs/maxprocs"

	// Importing this package sets GOMEMLIMIT automatically from the cgroup memory limit.
	// By default, it sets GOMEMLIMIT to 90% of the cgroup memory limit.
	// Set the AUTOMEMLIMIT environment variable to a ratio in (0.0, 1.0], or "off".
	_ "github.com/KimMachineGun/automemlimit"

	ievlog "github.com/lxc/incus-compose/ievent/log"
	"github.com/lxc/incus-compose/ievent/source"
	"github.com/lxc/incus-compose/incustrust"
	"github.com/lxc/incus-compose/shared"
)

// certName is what this binary registers itself as in the Incus trust store,
// and what the daemon logs the connection as.
const certName = "ic-dns"

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
		Name:  "ic-dns",
		Usage: "DNS for Incus instances, per querier",
		Commands: []*cli.Command{
			runCommand(newConfig()),
			versionCommand(),
		},
	}
}

// runCommand is the flags and the environment together: every flag reads
// DNS_<NAME> when it is not given. Apart from command() so a test can drive it.
func runCommand(cfg *config) *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Serve DNS until told to stop",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "incus",
				Usage:       "URL of the Incus API",
				Destination: &cfg.IncusURL,
				Sources:     cli.EnvVars("DNS_INCUS"),
			},
			&cli.StringFlag{
				Name:        "token",
				Usage:       "One-time trust token; a token file under --secrets-dir is read when this is empty",
				Destination: &cfg.Token,
				Sources:     cli.EnvVars("DNS_TOKEN"),
			},
			&cli.StringFlag{
				Name: "data-dir",
				Usage: "Persistent directory holding the enrolled certificate and " +
					"what was last served; empty keeps neither",
				Value:       defaultDataDir,
				Destination: &cfg.DataDir,
				Sources:     cli.EnvVars("DNS_DATA_DIR"),
			},
			&cli.StringFlag{
				Name:        "secrets-dir",
				Usage:       "Tmpfs directory holding the one-time trust token",
				Value:       defaultSecretsDir,
				Destination: &cfg.SecretsDir,
				Sources:     cli.EnvVars("DNS_SECRETS_DIR"),
			},
			&cli.StringFlag{
				Name:        "client-cert",
				Usage:       "Certificate to present instead of enrolling; needs --client-key",
				Destination: &cfg.ClientCert,
				Sources:     cli.EnvVars("DNS_CLIENT_CERT"),
			},
			&cli.StringFlag{
				Name:        "client-key",
				Usage:       "Key for --client-cert",
				Destination: &cfg.ClientKey,
				Sources:     cli.EnvVars("DNS_CLIENT_KEY"),
			},
			&cli.BoolFlag{
				Name:        "restricted",
				Usage:       "Enroll a certificate confined to --project; off means one server answers for every visible project",
				Destination: &cfg.Restricted,
				Sources:     cli.EnvVars("DNS_RESTRICTED"),
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
				Sources:     cli.EnvVars("DNS_USE_REMOTE"),
			},

			&cli.StringFlag{
				Name:        "suffix",
				Usage:       "TLD every project's zone is built under",
				Value:       defaultSuffix,
				Destination: &cfg.Suffix,
				Sources:     cli.EnvVars("DNS_SUFFIX"),
			},
			&cli.StringSliceFlag{
				Name:        "project",
				Usage:       "Project(s) to serve; empty means every visible project carrying --project-marker",
				Destination: &cfg.Projects,
				Sources:     cli.EnvVars("DNS_PROJECTS"),
			},
			&cli.StringFlag{
				Name:    "project-marker",
				Usage:   "Project config `KEY=VALUE` that opts a project in when --project is empty; a bare KEY means KEY=true",
				Sources: cli.EnvVars("DNS_PROJECT_MARKER"),
				Action: func(ctx context.Context, c *cli.Command, s string) error {
					marker, value := parseMarker(s)
					cfg.ProjectMarker = marker
					cfg.ProjectMarkerValue = value

					return nil
				},
			},

			&cli.StringFlag{
				Name:        "listen",
				Usage:       "Address to answer DNS on, UDP and TCP",
				Value:       defaultDNSAddr,
				Destination: &cfg.DNSAddr,
				Sources:     cli.EnvVars("DNS_LISTEN"),
			},
			&cli.StringFlag{
				Name:        "http",
				Usage:       "Address to serve /metrics, /health and /ready on; empty disables it",
				Value:       defaultHTTPAddr,
				Destination: &cfg.HTTPAddr,
				Sources:     cli.EnvVars("DNS_HTTP"),
			},
			&cli.StringSliceFlag{
				Name:        "forward",
				Usage:       "Upstream(s) for names we do not serve; empty refuses them instead",
				Destination: &cfg.Forward,
				Sources:     cli.EnvVars("DNS_FORWARD"),
			},

			&cli.UintFlag{
				Name:        "ttl",
				Usage:       "Seconds a record is served for; 0-" + strconv.Itoa(maxTTL),
				Value:       defaultTTL,
				Destination: &cfg.TTL,
				Sources:     cli.EnvVars("DNS_TTL"),
			},
			&cli.DurationFlag{
				Name:        "debounce-window",
				Usage:       "How long a key must be quiet before the last of its burst is handed on",
				Value:       defaultDebounceWindow,
				Destination: &cfg.DebounceWindow,
				Sources:     cli.EnvVars("DNS_DEBOUNCE_WINDOW"),
			},
			&cli.IntFlag{
				Name:        "workers",
				Usage:       "Incus reads in flight at once",
				Value:       defaultWorkers,
				Destination: &cfg.Workers,
				Sources:     cli.EnvVars("DNS_WORKERS"),
			},
			&cli.DurationFlag{
				Name:        "read-timeout",
				Usage:       "Budget for one read of the daemon",
				Value:       defaultReadTimeout,
				Destination: &cfg.ReadTimeout,
				Sources:     cli.EnvVars("DNS_READ_TIMEOUT"),
			},
			&cli.DurationFlag{
				Name:        "sweep-project-delay",
				Usage:       "Gap between one project of a round and the next",
				Value:       defaultProjectDelay,
				Destination: &cfg.ProjectDelay,
				Sources:     cli.EnvVars("DNS_SWEEP_PROJECT_DELAY"),
			},
			&cli.DurationFlag{
				Name:        "sweep-read-delay",
				Usage:       "Gap between the reads inside one project",
				Value:       defaultReadDelay,
				Destination: &cfg.ReadDelay,
				Sources:     cli.EnvVars("DNS_SWEEP_READ_DELAY"),
			},
			&cli.BoolFlag{
				Name:        "echo-subnet",
				Usage:       "Echo the RFC 7871 client subnet back on replies",
				Destination: &cfg.EchoSubnet,
				Sources:     cli.EnvVars("DNS_ECHO_SUBNET"),
			},
			&cli.BoolFlag{
				Name:        "metrics",
				Usage:       "Record the engine's counters and gauges; off leaves them registered at zero",
				Destination: &cfg.Metrics,
				Sources:     cli.EnvVars("DNS_METRICS"),
			},
			&cli.StringSliceFlag{
				Name:    "allow-transfer",
				Usage:   "CIDR(s) that may ask for a zone transfer; empty allows nobody",
				Sources: cli.EnvVars("DNS_ALLOW_TRANSFER"),
				Action: func(ctx context.Context, c *cli.Command, s []string) error {
					allow, err := prefixes(s)
					if err != nil {
						return err
					}

					cfg.AllowTransfer = allow

					return nil
				},
			},
			&cli.StringSliceFlag{
				Name:        "exclude",
				Usage:       "Chain position(s) to leave out; only the optional ones, and an unknown name is an error",
				Destination: &cfg.Exclude,
				Sources:     cli.EnvVars("DNS_EXCLUDE"),
			},

			&cli.StringFlag{
				Name: "log",
				Usage: "Level the chain's log positions print at, and the process's own level: " +
					"TRACE, DEBUG, INFO, WARN, ERROR. Empty leaves every position out and the process at INFO",
				Destination: &cfg.Log,
				Sources:     cli.EnvVars("DNS_LOG"),
			},

			&cli.BoolFlag{
				Name:        "pprof",
				Usage:       "Serve /debug/pprof on the --http address; for profiling, never for a deployment",
				Destination: &cfg.Pprof,
				Sources:     cli.EnvVars("DNS_PPROF"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return action(ctx, cmd, cfg)
		},
	}
}

// versionCommand prints what this build is.
func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print version information",
		Action: func(_ context.Context, _ *cli.Command) error {
			fmt.Println("ic-dns version", version)

			return nil
		},
	}
}

// action turns the command line into a running process.
func action(ctx context.Context, cmd *cli.Command, cfg *config) error {
	err := cfg.validate()
	if err != nil {
		return err
	}

	// --log sets the process's own level too, so the handler passes what the
	// chain's log positions print rather than turning them on and showing nothing.
	level := shared.StringToSlogLevel(cfg.Log)

	logger := slog.New(slog.NewTextHandler(cmd.ErrWriter, &slog.HandlerOptions{Level: level}))

	// CoreDNS's own logger, and ecs_view's, arriving as slog records.
	ievlog.Hook(level)

	// Container CPU limits are a quota, not a core count, so GOMAXPROCS has to
	// be told.
	undo, err2 := maxprocs.Set(maxprocs.Logger(func(format string, args ...any) {
		logger.Info(fmt.Sprintf(format, args...))
	}))
	if err2 != nil {
		logger.Warn("setting GOMAXPROCS", "err", err2)
	}

	defer undo()

	logger.Info("Starting",
		"version", version,
		"pid", os.Getpid(),
		"incus", cfg.endpoint(),
		"dns", cfg.DNSAddr,
		"http", cfg.HTTPAddr,
	)

	// One attribute each rather than a struct printed with %+v, so a single
	// field can be grepped out.
	logger.Debug("configuration",
		"projects", cfg.Projects,
		"project_marker", cfg.ProjectMarker+"="+cfg.ProjectMarkerValue,
		"forward", cfg.Forward,
		"ttl", cfg.TTL,
		"data_dir", cfg.DataDir,
		"secrets_dir", cfg.SecretsDir,
		"token", cfg.redacted().Token,
		"debounce_window", cfg.DebounceWindow,
		"workers", cfg.Workers,
		"read_timeout", cfg.ReadTimeout,
		"sweep_project_delay", cfg.ProjectDelay,
		"sweep_read_delay", cfg.ReadDelay,
		"echo_subnet", cfg.EchoSubnet,
		"metrics", cfg.Metrics,
		"pprof", cfg.Pprof,
	)

	return run(ctx, logger, *cfg)
}

// run is everything with a lifetime, in the order it starts and the reverse of
// the order it stops.
func run(ctx context.Context, logger *slog.Logger, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	plugins, runners, err := assemble(chain(logger, cfg), cfg.Exclude)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Name())
	}

	logger.Info("chain", "plugins", names)

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
	src, err := source.New(logger, conn, plugins)
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
			logger.Error("running the source", "err", err)
			cancel()
		}
	})

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
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(sig)

	select {
	case s := <-sig:
		logger.Info("shutting down", "signal", s.String())
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

// prefixes parses the CIDRs a flag carries. A bad one is refused here rather
// than dropped, or an allow-list would silently be narrower than it reads.
func prefixes(list []string) ([]netip.Prefix, error) {
	if len(list) == 0 {
		return nil, nil
	}

	out := make([]netip.Prefix, 0, len(list))

	for _, text := range list {
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, fmt.Errorf("allow-transfer %q: %w", text, err)
		}

		out = append(out, prefix.Masked())
	}

	return out, nil
}
