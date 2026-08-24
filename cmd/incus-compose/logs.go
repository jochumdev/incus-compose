package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/project"
)

// logWriter takes an instance's console output.
type logWriter func(client.Resource, []byte)

// instanceLog writes an instance's console log to out, then keeps streaming it
// until the context is canceled when follow is set.
func instanceLog(ctx context.Context, c *client.Client, inst *client.Instance, out logWriter, follow bool) error {
	conn, err := c.Connection()
	if err != nil {
		return err
	}

	err = logBuffer(ctx, conn, c.IncusProject(), inst, out)
	if err != nil || !follow {
		return err
	}

	return logStream(ctx, conn, c.IncusProject(), inst, out)
}

// logBuffer reads the saved console log buffer via GET /console (equivalent to
// `incus console --show-log`).
func logBuffer(ctx context.Context, conn *iclient.Connection, project string, inst *client.Instance, out logWriter) error {
	reader, err := conn.GetInstanceConsoleLog(ctx, project, inst.IncusName())
	if err != nil {
		return client.ErrOperation.WithText("getting console log").Wrap(err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return client.ErrOperation.WithText("reading console log").Wrap(err)
	}

	out(inst, data)

	return nil
}

// logStream streams the console until the context is canceled.
func logStream(ctx context.Context, conn *iclient.Connection, project string, inst *client.Instance, out logWriter) error {
	req := incusApi.InstanceConsolePost{
		Type:  "console",
		Force: true, // Take over existing console connections
	}

	op, err := conn.ConsoleInstance(ctx, project, inst.IncusName(), req, &iclient.InstanceConsoleArgs{
		Output: &logOutput{resource: inst, out: out},
	})
	if err != nil {
		return client.ErrOperation.WithText("connecting to console").Wrap(err)
	}

	_, err = iclient.WaitOperation(ctx, op)

	// Context cancellation (including timeout) is not an error
	if ctx.Err() != nil {
		return nil //nolint:nilerr // caller-initiated cancellation, not a streaming failure
	}

	if err != nil {
		return client.ErrOperation.WithText("console streaming").Wrap(err)
	}

	return nil
}

// logOutput hands the console stream to out.
type logOutput struct {
	resource client.Resource
	out      logWriter
}

func (t *logOutput) Write(p []byte) (int, error) {
	t.out(t.resource, p)

	return len(p), nil
}

// ANSI color codes for log output.
var logColors = []string{
	"36",   // cyan
	"33",   // yellow
	"32",   // green
	"35",   // magenta
	"34",   // blue
	"36;1", // intense cyan
	"33;1", // intense yellow
	"32;1", // intense green
	"35;1", // intense magenta
	"34;1", // intense blue
}

// logHandler handles formatting, output of log lines, and per-instance log goroutine tracking.
type logHandler struct {
	mu         sync.Mutex
	out        io.Writer
	colors     map[string]string // resource name -> color code
	colorIndex int
	maxWidth   int
	noColor    bool
	buffers    map[string]*bytes.Buffer // resource name -> line buffer
	cancels    sync.Map                 // incus name -> context.CancelFunc
}

// newLogFormatter creates a new log formatter.
func newLogFormatter(out io.Writer, noColor bool) *logHandler {
	return &logHandler{
		out:     out,
		colors:  make(map[string]string),
		buffers: make(map[string]*bytes.Buffer),
		noColor: noColor,
	}
}

// registerService registers a service and assigns it a color.
func (f *logHandler) registerService(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.colors[name]; ok {
		return
	}

	f.colors[name] = logColors[f.colorIndex%len(logColors)]
	f.colorIndex++
	f.buffers[name] = &bytes.Buffer{}

	if len(name) > f.maxWidth {
		f.maxWidth = len(name)
	}
}

// write handles incoming log data from a resource.
func (f *logHandler) write(r client.Resource, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := r.Name()

	if _, ok := f.colors[name]; !ok {
		f.colors[name] = logColors[f.colorIndex%len(logColors)]
		f.colorIndex++
		f.buffers[name] = &bytes.Buffer{}
		if len(name) > f.maxWidth {
			f.maxWidth = len(name)
		}
	}

	buf := f.buffers[name]
	buf.Write(data)

	for {
		line, err := buf.ReadBytes('\n')
		if err != nil {
			buf.Write(line)
			break
		}

		f.writeLine(name, line)
	}
}

// writeLine outputs a single line with prefix and color.
func (f *logHandler) writeLine(name string, line []byte) {
	prefix := fmt.Sprintf("%-*s | ", f.maxWidth, name)

	if f.noColor {
		_, _ = fmt.Fprintf(f.out, "%s%s", prefix, line)
	} else {
		color := f.colors[name]
		_, _ = fmt.Fprintf(f.out, "\033[%sm%s\033[0m%s", color, prefix, line)
	}
}

// flush outputs any remaining buffered data.
func (f *logHandler) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for name, buf := range f.buffers {
		if buf.Len() > 0 {
			line := buf.Bytes()
			f.writeLine(name, append(line, '\n'))
			buf.Reset()
		}
	}
}

// startStream begins streaming logs for an instance in a background goroutine.
func (f *logHandler) startStream(ctx context.Context, c *client.Client, inst *client.Instance) {
	name := inst.IncusName()
	if _, running := f.cancels.Load(name); running {
		return
	}

	f.registerService(inst.Name())

	logCtx, cancel := context.WithCancel(ctx)
	f.cancels.Store(name, cancel)

	go func() {
		_ = instanceLog(logCtx, c, inst, f.write, true)
		f.cancels.Delete(name)
	}()
}

// stopStream cancels the log goroutine for the named instance.
func (f *logHandler) stopStream(incusName string) {
	v, ok := f.cancels.LoadAndDelete(incusName)
	if !ok {
		return
	}

	if cancel, ok := v.(context.CancelFunc); ok {
		cancel()
	}
}

// stopStreams cancels all running log goroutines.
func (f *logHandler) stopStreams() {
	f.cancels.Range(func(key, value any) bool {
		if cancel, ok := value.(context.CancelFunc); ok {
			cancel()
		}
		f.cancels.Delete(key)
		return true
	})
}

// projectInstances resolves the project's instance resources, skipping the ones
// the client cannot hand back.
func projectInstances(c *client.Client, p *project.Project) []*client.Instance {
	names := p.InstanceNames()
	instances := make([]*client.Instance, 0, len(names))

	for _, name := range names {
		r, err := c.Resource(client.KindInstance, name, &client.InstanceConfig{})
		if err != nil {
			continue
		}

		inst, ok := r.(*client.Instance)
		if !ok {
			continue
		}

		instances = append(instances, inst)
	}

	return instances
}

// logsArgs holds the logs() options, mirroring the logs command's flags.
type logsArgs struct {
	Instances []*client.Instance
	Follow    bool
	Writer    io.Writer
}

// logs streams or dumps the given instances' logs.
func logs(ctx context.Context, c *client.Client, args logsArgs) error {
	noColor := noColor(ctx)

	formatter := newLogFormatter(args.Writer, noColor)

	knownInstances := make(map[string]*client.Instance, len(args.Instances))
	for _, inst := range args.Instances {
		knownInstances[inst.IncusName()] = inst
	}

	if !args.Follow {
		for _, inst := range knownInstances {
			formatter.registerService(inst.Name())
			_ = instanceLog(ctx, c, inst, formatter.write, false)
		}

		formatter.flush()
		return nil
	}

	conn, err := c.Connection()
	if err != nil {
		c.LogError("Getting connection for events", "error", err)
		return errLogged.Wrap(err)
	}

	listenCtx, stopListening := context.WithCancel(ctx)
	defer stopListening()

	events, err := conn.ListenEvents(listenCtx, c.IncusProject(), []string{incusApi.EventTypeLifecycle})
	if err != nil {
		c.LogError("Subscribing to events", "error", err)
		return errLogged.Wrap(err)
	}

	defer formatter.stopStreams()

	projectGone := make(chan struct{})
	incusProject := c.IncusProject()

	go func() {
		for event := range events {
			var lifecycle incusApi.EventLifecycle
			if err := json.Unmarshal(event.Metadata, &lifecycle); err != nil {
				continue
			}

			if lifecycle.Action == incusApi.EventLifecycleProjectDeleted && lifecycle.Name == incusProject {
				close(projectGone)
				return
			}

			inst, known := knownInstances[lifecycle.Name]
			if !known {
				continue
			}

			switch lifecycle.Action {
			case incusApi.EventLifecycleInstanceStarted:
				formatter.startStream(ctx, c, inst)
			case incusApi.EventLifecycleInstanceStopped, incusApi.EventLifecycleInstanceDeleted, incusApi.EventLifecycleInstanceShutdown:
				formatter.stopStream(lifecycle.Name)
			}
		}
	}()

	for _, inst := range knownInstances {
		formatter.startStream(ctx, c, inst)
	}

	select {
	case <-ctx.Done():
	case <-projectGone:
		c.LogError("Project deleted")
		formatter.flush()
		return errLogged
	}

	formatter.flush()
	return nil
}

func newLogsCommand() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "View output from containers",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "Follow log output",
				Sources: cli.EnvVars("INCUS_COMPOSE_LOGS_FOLLOW"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, c, err := loadProject(ctx, cmd, client.EnsureProjectWithCreate())
			if err != nil {
				return err
			}

			err = c.Open()
			if err != nil {
				c.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = c.Done() }()

			return logs(ctx, c, logsArgs{
				Instances: projectInstances(c, p),
				Follow:    cmd.Bool("follow"),
				Writer:    cmd.Root().Writer,
			})
		},
	}
}
