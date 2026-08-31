package enricher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	incusutil "github.com/lxc/incus/v7/shared/util"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

type sweepAction int

const (
	sweepActionDone sweepAction = iota
	sweepActionFailed

	// The three listings, each the whole of one scope. What a listing does not
	// have has gone, and the fold acts on that where the message arrives -
	// right behind the read, rather than at the end of a run that takes minutes.
	sweepActionProjects
	sweepActionNetworks
	sweepActionInstances

	// sweepActionNetwork is one network, read whole.
	sweepActionNetwork

	// sweepActionNetworksWarm says every network of this run has been read, so
	// an instance read can place the NICs it finds.
	sweepActionNetworksWarm
)

// sweepMsg is one thing the sweeper says.
type sweepMsg struct {
	action sweepAction

	// project scopes a listing of networks or instances.
	project string

	// names is the whole of one scope, as the listing answered it.
	names []string

	// network is one network read whole.
	network *incusapi.Network

	err error
}

// sweeperConn is what the sweeper lists. An interface rather than the connection, so
// a test answers with Incus values instead of running a daemon.
type sweeperConn interface {
	GetProjectNames(ctx context.Context) ([]string, error)
	GetProject(ctx context.Context, name string) (*incusapi.Project, string, error)
	GetNetworkNames(ctx context.Context, project string) ([]string, error)
	GetInstanceNames(ctx context.Context, project string, args *iclient.GetInstancesArgs) ([]string, error)
}

func sweepSend(ctx context.Context, out chan<- sweepMsg, msg sweepMsg) error {
	select {
	case out <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sweepWait pauses for d, and reports whether the run may continue.
func sweepWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sweepArgs is everything one run needs. A bundle rather than eight parameters,
// since every phase takes most of them.
type sweepArgs struct {
	conn    sweeperConn
	readNet netReadFunc

	// sweepOut carries what the run found; evOut is the plugin's own inbox,
	// where an instance the run names enters as an ordinary event.
	sweepOut chan<- sweepMsg
	evOut    chan<- *iutil.Event

	// serves decides which projects this binary is for. Nil serves every one the
	// certificate can see.
	serves func(p *incusapi.Project) bool

	timeout time.Duration
	delay   time.Duration
}

// runSweep reads the fleet once: the projects, then every network of every
// project, then the instances. The order is the point - an instance read before
// its network is known cannot be placed.
func runSweep(ctx context.Context, a sweepArgs) error {
	served, projects, err := sweepProjects(ctx, a)
	if err != nil {
		return err
	}

	err = sweepNetworks(ctx, a, projects)
	if err != nil {
		return err
	}

	err = sweepSend(ctx, a.sweepOut, sweepMsg{action: sweepActionNetworksWarm})
	if err != nil {
		return err
	}

	return sweepInstances(ctx, a, served)
}

// sweepProjects reads every project and answers two lists: served is where the
// instances are swept, projects is where the networks are listed. They differ
// because a project without features.networks has none of its own, and uses the
// default project's bridges instead.
//
// A project that could not be read still counts as served, or it would be
// pruned on the strength of a failed request.
func sweepProjects(ctx context.Context, a sweepArgs) (served, projects []string, err error) {
	readCtx, cancel := context.WithTimeout(ctx, a.timeout)
	names, err := a.conn.GetProjectNames(readCtx)

	cancel()

	if err != nil {
		return nil, nil, fmt.Errorf("listing projects: %w", err)
	}

	// Every bridge lives in the default project unless a project has networks of
	// its own, so it is listed whether or not it is served.
	served = make([]string, 0, len(names))
	projects = []string{iutil.DefaultProject}

	for _, projectName := range names {
		readCtx, cancel := context.WithTimeout(ctx, a.timeout)
		project, _, err := a.conn.GetProject(readCtx, projectName)

		cancel()

		if err != nil {
			slog.Warn("a project of the fleet could not be read, keeping what is held",
				"plugin", name, "project", projectName, "err", err)

			served = append(served, projectName)

			continue
		}

		if a.serves != nil && !a.serves(project) {
			continue
		}

		served = append(served, projectName)

		if projectName != iutil.DefaultProject && incusutil.IsTrue(project.Config[iutil.FeaturesNetworks]) {
			projects = append(projects, projectName)
		}

		err = sweepWait(ctx, a.delay)
		if err != nil {
			return nil, nil, err
		}
	}

	err = sweepSend(ctx, a.sweepOut, sweepMsg{action: sweepActionProjects, names: served})
	if err != nil {
		return nil, nil, err
	}

	return served, projects, nil
}

// sweepNetworks reads every network of every project.
//
// Per project rather than one listing: a project with features.networks has
// networks of its own, and asking one project answers with that project's alone.
func sweepNetworks(ctx context.Context, a sweepArgs, projects []string) error {
	for _, project := range projects {
		readCtx, cancel := context.WithTimeout(ctx, a.timeout)
		names, err := a.conn.GetNetworkNames(readCtx, project)

		cancel()

		if err != nil {
			// One project failing costs its own networks rather than the fleet's.
			slog.Warn("the networks of one project could not be listed, keeping what is held",
				"plugin", name, "project", project, "err", err)

			continue
		}

		err = sweepSend(ctx, a.sweepOut, sweepMsg{
			action:  sweepActionNetworks,
			project: project,
			names:   names,
		})
		if err != nil {
			return err
		}

		for _, netName := range names {
			readCtx, cancel := context.WithTimeout(ctx, a.timeout)
			net, _, err := a.readNet(readCtx, project, netName)

			cancel()

			msg := sweepMsg{action: sweepActionNetwork, network: net}
			if err != nil {
				msg = sweepMsg{action: sweepActionFailed, err: err}
			}

			err = sweepSend(ctx, a.sweepOut, msg)
			if err != nil {
				return err
			}

			err = sweepWait(ctx, a.delay)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// sweepInstances lists each served project's instances and trickles the names.
// The listing goes over as one message before any of it is trickled, so absence
// is decided against what was held when the request went out.
func sweepInstances(ctx context.Context, a sweepArgs, served []string) error {
	for _, project := range served {
		readCtx, cancel := context.WithTimeout(ctx, a.timeout)
		names, err := a.conn.GetInstanceNames(readCtx, project, nil)

		cancel()

		if err != nil {
			slog.Warn("the instances of one project could not be listed, keeping what is held",
				"plugin", name, "project", project, "err", err)

			continue
		}

		err = sweepSend(ctx, a.sweepOut, sweepMsg{
			action:  sweepActionInstances,
			project: project,
			names:   names,
		})
		if err != nil {
			return err
		}

		for _, instanceName := range names {
			ev := iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, project, instanceName, "")

			select {
			case a.evOut <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}

			err = sweepWait(ctx, a.delay)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// runSweeper goes round the fleet for the life of ctx, on a goroutine of its
// own.
//
// The first run goes at no delay, because nothing is served until it lands, and
// so does one that follows a run that broke.
func runSweeper(ctx context.Context, a sweepArgs, interval time.Duration) {
	go func() {
		fast := true

		for ctx.Err() == nil {
			run := a
			if fast {
				run.delay = 0
			}

			err := runSweep(ctx, run)

			fast = err != nil

			if ctx.Err() != nil {
				return
			}

			// Done only where the run reached its end: it is what says the
			// fleet has been read whole, and warm rides with it.
			msg := sweepMsg{action: sweepActionDone}
			if err != nil {
				msg = sweepMsg{action: sweepActionFailed, err: err}
			}

			err = sweepSend(ctx, a.sweepOut, msg)
			if err != nil {
				return
			}

			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return
			}
		}
	}()
}
