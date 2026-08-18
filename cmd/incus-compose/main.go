// Package main provides the incus-compose CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/cmd/incus-compose/version"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

// managedKey marks a project or an instance as created by incus-compose. It
// records ownership only; watching is HealthEnabledKey, a separate decision.
const managedKey = "user.incus-compose.managed"

const systemProject = "incus-compose"

type noColorKey struct{}

// errLogged is an internal sentinel error, return it to silence the error but exit 1.
var errLogged = client.NewError("Logged error")

// buildLoadOptions converts CLI flags to project.LoadOption slice.
func buildLoadOptions(cmd *cli.Command) []project.LoadOption {
	// The project package stamps nothing unless its caller asks. On a project,
	// HealthEnabledKey is what a hand-run healthd looks for.
	loadOpts := []project.LoadOption{
		project.LoadInstanceMarks(map[string]string{managedKey: "true"}),
		project.LoadProjectMarks(map[string]string{
			managedKey:              "true",
			shared.HealthEnabledKey: "true",
		}),
	}

	if name := cmd.Root().String("project-name"); name != "" {
		loadOpts = append(loadOpts, project.LoadName(name))
	}

	files := cmd.Root().StringSlice("file")
	dir := cmd.Root().String("project-directory")

	composeFileNames := []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

	if len(files) == 0 {
		for _, name := range composeFileNames {
			var candidate string
			var err error
			if dir != "" {
				candidate, err = filepath.Abs(filepath.Join(dir, name))
			} else {
				candidate, err = filepath.Abs(name)
			}
			if err == nil {
				if _, err := os.Stat(candidate); err == nil {
					files = append(files, candidate)
					break
				}
			}
		}
	}

	for _, f := range files {
		absf, err := filepath.Abs(f)
		if err != nil {
			continue
		}

		ext := filepath.Ext(f)
		incusCFile := filepath.Join(
			filepath.Dir(absf),
			strings.TrimSuffix(
				filepath.Base(absf),
				ext)+".incus"+ext,
		)
		_, err = os.Stat(incusCFile)
		if err == nil {
			files = append(files, incusCFile)
		}
	}

	if len(files) > 0 {
		loadOpts = append(loadOpts, project.LoadFiles(files))
	}

	if dir != "" {
		loadOpts = append(loadOpts, project.LoadWorkingDir(dir))
	}

	if envFiles := cmd.StringSlice("env-file"); len(envFiles) > 0 {
		loadOpts = append(loadOpts, project.LoadEnvFiles(envFiles))
	}

	if profiles := cmd.StringSlice("profile"); len(profiles) > 0 {
		loadOpts = append(loadOpts, project.LoadProfiles(profiles))
	}

	if cmd.Bool("os-env") {
		loadOpts = append(loadOpts, project.LoadOsEnv())
	}

	return loadOpts
}

// initLogger builds a logger scoped to this single command invocation. It
// must not become or mutate any process-wide global (slog.SetDefault, or a
// shared writer) - cmd.Run is called once per real process in production,
// but tests run many invocations concurrently in one process, and a shared
// global there causes one command's log lines to leak into another's output.
//
// The returned LogWriter backs the logger's handler, so a progress renderer
// can call client.SwapLogWriter to redirect log lines above a live display
// without touching any process-wide state.
func initLogger(debug bool, noColor bool, writer io.Writer) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	if !noColor && runtime.GOOS == "windows" {
		writer = colorable.NewColorable(os.Stderr)
	}

	logger := slog.New(tint.NewTextHandler(
		writer,
		&tint.Options{
			NoColor:    noColor,
			Level:      level,
			TimeFormat: "15:04",
		},
	))

	return logger
}

type clientKey struct{}

func resolveHealthdImage(image string) string {
	v := version.Current()
	if v[0] == 'v' {
		v = v[1:]
	}

	return strings.ReplaceAll(image, "{version}", v)
}

// loadProjectClient loads the compose project and opens its per-project Incus
// client, without creating the project if it doesn't exist yet.
func loadProjectClient(ctx context.Context, cmd *cli.Command) (*project.Project, *client.Client, error) {
	globalClient, err := clientFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := globalClient.Connect(); err != nil {
		return nil, nil, err
	}

	p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
	if err != nil {
		globalClient.LogError("Configuring the project", "error", err)
		return nil, nil, err
	}

	c, err := globalClient.EnsureProject(p.Name)
	if err != nil {
		globalClient.LogError("Getting the incus project", "error", err)
		return nil, nil, errLogged
	}

	if err := c.Open(); err != nil {
		globalClient.LogError("Opening the project client", "error", err)
		return nil, nil, errLogged.Wrap(err)
	}

	return p, c, nil
}

func clientFromContext(ctx context.Context) (*client.GlobalClient, error) {
	ca := ctx.Value(clientKey{})
	c, ok := ca.(*client.GlobalClient)
	if !ok {
		return nil, errors.New("failed to retrieve the client from context")
	}

	return c, nil
}

func noColor(ctx context.Context) bool {
	if ctx.Value(noColorKey{}) == nil {
		return false
	}

	ok, v := ctx.Value(noColorKey{}).(bool)
	if !ok {
		return false
	}

	return v
}

// selfUpdateWritable reports whether the running executable can be replaced in
// place. Self-update writes a new binary into the executable's directory and
// renames it over the target, so directory writability is what matters -- the
// running file itself cannot be opened O_WRONLY (ETXTBSY on Linux, locked on
// Windows). The check is delegated to a platform-specific dirWritable.
func selfUpdateWritable() bool {
	exe, err := selfupdate.ExecutablePath() // resolves symlinks to the real file
	if err != nil {
		return false
	}
	return dirWritable(filepath.Dir(exe))
}

func newRootCommand() *cli.Command {
	commands := []*cli.Command{
		newUpCommand(),
		newDownCommand(),
		newBuildCommand(),
		newStartCommand(),
		newStopCommand(),
		newRestartCommand(),
		newBackupCommand(),
		newListCommand(),
		newPsCommand(),
		newPullCommand(),
		newConfigCommand(),
		newExecCommand(),
		newPortCommand(),
		newPortForwardCommand(),
		newLogsCommand(),
		newIncusCommand(),
		newHealthdCommand(),
		newVersionCommand(),
	}

	// "self-update" is only available if the executable is writeable and the version is not "latest".
	if version.Current() != "latest" && selfUpdateWritable() {
		commands = append(commands, newSelfUpdateCommand())
	}

	return &cli.Command{
		Usage: "Compose for incus",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "remote",
				Usage:   "remote to connect to",
				Value:   "",
				Sources: cli.EnvVars("INCUS_REMOTE"),
			},
			&cli.StringFlag{
				Name:    "ansi",
				Usage:   `Control when to print ANSI control character ("never", "always", "auto")`,
				Value:   "auto",
				Sources: cli.EnvVars("INCUS_COMPOSE_ANSI"),
			},
			&cli.StringSliceFlag{
				Name:    "env-file",
				Usage:   `Specify alternative environment files`,
				Sources: cli.EnvVars("INCUS_COMPOSE_ENV_FILE"),
			},
			&cli.StringSliceFlag{
				Name:    "profile",
				Usage:   `Specify profiles to enable`,
				Sources: cli.EnvVars("INCUS_COMPOSE_PROFILES"),
			},
			&cli.StringFlag{
				Name:    "project-directory",
				Aliases: []string{"P"},
				Usage:   `Specify an alternate working directory (default: the path of the, first specified, Compose file)`,
				Sources: cli.EnvVars("INCUS_COMPOSE_PROJECT_DIRECTORY"),
			},
			&cli.StringFlag{
				Name:    "project-name",
				Aliases: []string{"p"},
				Usage:   `Project name`,
				Sources: cli.EnvVars("INCUS_COMPOSE_PROJECT_NAME"),
			},
			&cli.StringFlag{
				Name:    "storage-pool",
				Usage:   `Default storage pool to use, 'detect' will auto detect the name`,
				Value:   "detect",
				Sources: cli.EnvVars("INCUS_COMPOSE_STORAGE_POOL"),
			},
			&cli.StringFlag{
				Name:    "image-cache",
				Usage:   `Image cache project to use, set empty to disable`,
				Value:   client.DefaultCacheProject,
				Sources: cli.EnvVars("INCUS_COMPOSE_IMAGE_CACHE"),
			},
			&cli.StringSliceFlag{
				Name:    "file",
				Aliases: []string{"f"},
				Usage:   `Compose configuration files`,
				Sources: cli.EnvVars("INCUS_COMPOSE_FILE"),
			},
			&cli.BoolFlag{
				Name:    "os-env",
				Sources: cli.EnvVars("INCUS_COMPOSE_OS_ENV"),
				Aliases: []string{"E"},
				Usage:   `Include OS environment variables for interpolation`,
			},
			&cli.BoolFlag{
				Name:    "debug",
				Usage:   `Enable debug logging`,
				Sources: cli.EnvVars("INCUS_COMPOSE_DEBUG"),
			},
			&cli.BoolFlag{
				Name:    "trace",
				Usage:   `Enable per-event logging, which implies --debug. Only ic-healthd reads it so far`,
				Sources: cli.EnvVars("INCUS_COMPOSE_TRACE"),
			},
			&cli.IntFlag{
				Name:    "workers",
				Usage:   `Number of concurrent workers`,
				Sources: cli.EnvVars("INCUS_COMPOSE_WORKERS"),
				Value:   4,
			},
		},
		Commands: commands,
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			noColor := false

			// NO_COLOR takes precedence (https://no-color.org/)
			if _, ok := os.LookupEnv("NO_COLOR"); ok {
				noColor = true
			} else {
				switch strings.ToLower(cmd.String("ansi")) {
				case "always":
					noColor = true
				case "auto":
					// Non-windows and no terminal.
					if runtime.GOOS == "windows" || isatty.IsTerminal(os.Stderr.Fd()) {
						noColor = false
					} else {
						noColor = true
					}
				case "never":
					noColor = true
				default:
					noColor = false
				}
			}

			logWriter := client.NewSwapWriter(cmd.ErrWriter)
			// --trace has no level of its own here yet; it turns on debug and
			// reaches ic-healthd, which does have one.
			logger := initLogger(cmd.Bool("debug") || cmd.Bool("trace"), noColor, logWriter)

			// Commands that don't need an Incus client connection
			noClientCommands := []string{"config", "version", "incus"}

			if slices.Contains(noClientCommands, cmd.Name) {
				return ctx, nil
			}

			// Connect to Incus server.
			conn, err := client.DialRemote("", cmd.String("remote"))
			if err != nil {
				return ctx, err
			}

			// cacheProject := cmd.String("image-cache")
			// if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			// 	cacheProject = ""
			// }

			opts := []client.ClientOption{
				client.ClientSystemProject(systemProject),
				client.ClientDescriptionFormat("incus-compose: %s"),
				client.ClientLogger(logger),
				client.ClientStdout(cmd.Writer),
				client.ClientStderrWriter(logWriter),
				client.ClientProvideConnection(conn),
				client.ClientCacheProject(cmd.String("image-cache")),
				client.ClientDefaultStoragePool(cmd.String("storage-pool")),
			}

			c := client.New(ctx, opts...)
			return context.WithValue(context.WithValue(ctx, clientKey{}, c), noColorKey{}, noColor), nil
		},
		After: func(ctx context.Context, cmd *cli.Command) error {
			return nil
		},
	}
}

func main() {
	if err := newRootCommand().Run(context.Background(), os.Args); err != nil {
		if errors.Is(err, errLogged) {
			os.Exit(1)
		}

		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
