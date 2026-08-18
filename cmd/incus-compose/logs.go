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
	"github.com/lxc/incus-compose/project"
)

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
func (f *logHandler) write(_ client.Action, r client.Resource, data []byte) {
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
func (f *logHandler) startStream(ctx context.Context, inst *client.Instance) {
	name := inst.IncusName()
	if _, running := f.cancels.Load(name); running {
		return
	}

	f.registerService(inst.Name())

	logCtx, cancel := context.WithCancel(ctx)
	f.cancels.Store(name, cancel)

	go func() {
		_ = client.RunAction(logCtx, inst, client.ActionLog, client.OptionFollow())
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

// logsArgs holds the logs() options, mirroring the logs command's flags.
type logsArgs struct {
	Follow bool
	Writer io.Writer
}

// logs streams or dumps the project's container logs.
func logs(ctx context.Context, p *project.Project, c *client.Client, args logsArgs) error {
	noColor := noColor(ctx)

	formatter := newLogFormatter(args.Writer, noColor)
	c.Global().SetOutputHandler(formatter.write)

	knownNames := p.InstanceNames()
	knownInstances := make(map[string]*client.Instance, len(knownNames))
	for _, name := range knownNames {
		r, err := c.Resource(client.KindInstance, name, &client.InstanceConfig{})
		if err != nil {
			continue
		}

		inst, ok := r.(*client.Instance)
		if !ok {
			continue
		}

		knownInstances[inst.IncusName()] = inst
	}

	if !args.Follow {
		for _, inst := range knownInstances {
			formatter.registerService(inst.Name())
			_ = inst.Log(ctx)
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

	events, err := conn.ListenEvents(listenCtx, []string{incusApi.EventTypeLifecycle}, false)
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
				formatter.startStream(ctx, inst)
			case incusApi.EventLifecycleInstanceStopped, incusApi.EventLifecycleInstanceDeleted, incusApi.EventLifecycleInstanceShutdown:
				formatter.stopStream(lifecycle.Name)
			}
		}
	}()

	for _, inst := range knownInstances {
		formatter.startStream(ctx, inst)
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

			return logs(ctx, p, c, logsArgs{
				Follow: cmd.Bool("follow"),
				Writer: cmd.Root().Writer,
			})
		},
	}
}
