package enricher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/panjf2000/ants/v2"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// pollInterval is how often a worker re-reads an instance's state while waiting
// for its IP address.
const pollInterval = 150 * time.Millisecond

// call is one read, and every event waiting on it. Owned by the goroutine Run
// owns; workers never read its fields.
type call struct {
	key     string
	project string
	name    string

	// kind is which of the three reads this is: kindInstance, kindNetwork or
	// kindProject. Set on every call, since it decides what submit asks for.
	kind string

	items []*item

	// ev is the event a fan-out would emit, held here rather than pushed: it goes
	// in the line only if the read found something new.
	ev *iutil.Event

	// wantInterfaces says whether any event waiting on this read asked for
	// EnrichedInstanceWithInterfaces.
	wantInterfaces bool
}

// join folds a second call for the same key into this one; the first ev is
// kept, later ones being the same event made twice.
func (c *call) join(other *call) {
	c.items = append(c.items, other.items...)

	if c.ev == nil {
		c.ev = other.ev
	}

	c.wantInterfaces = c.wantInterfaces || other.wantInterfaces
}

// result is what a worker hands back, carrying the call rather than a key so
// nothing has to find the waiting events again.
type result struct {
	call *call

	instance *incusapi.Instance
	state    *incusapi.InstanceState

	// network and project are set when the call read one of those instead.
	network *incusapi.Network
	project *incusapi.Project

	err error
}

// readFunc is one instance read. A function rather than the connection itself,
// so a test can answer with Incus values it built instead of ones a daemon
// returned.
type readFunc func(ctx context.Context, project, name string, wantInterfaces bool) (*incusapi.Instance, *incusapi.InstanceState, error)

// netReadFunc is one network read. Its own type beside readFunc so a test can
// answer either without a daemon.
type netReadFunc func(ctx context.Context, project, name string) (*incusapi.Network, string, error)

// projectReadFunc is one project read, for the configuration an instance event
// in that project carries.
type projectReadFunc func(ctx context.Context, name string) (*incusapi.Project, string, error)

// hasNICDevices reports whether the instance configuration attaches any NIC devices.
func hasNICDevices(inst *incusapi.Instance) bool {
	if inst == nil {
		return false
	}

	devices := inst.ExpandedDevices
	if len(devices) == 0 {
		devices = inst.Devices
	}

	for _, dev := range devices {
		if dev["type"] == "nic" {
			return true
		}
	}

	return false
}

// hasAddresses reports whether any non-loopback interface in the instance state
// has acquired at least one global IP address.
func hasAddresses(state *incusapi.InstanceState) bool {
	if state == nil {
		return false
	}

	for _, iface := range state.Network {
		if iface.Type == "loopback" {
			continue
		}

		for _, addr := range iface.Addresses {
			if addr.Scope != "global" || addr.Address == "" {
				continue
			}

			ip, err := netip.ParseAddr(addr.Address)
			if err != nil {
				continue
			}

			if ip.Is4() {
				return true
			}
		}
	}

	return false
}

// incusReader reads one instance and its state through the connection. When
// interfaces are requested, it polls until global IP addresses appear or the
// timeout expires.
func incusReader(logger *slog.Logger, conn *iclient.Connection, ipTimeout time.Duration) readFunc {
	return func(ctx context.Context, project, name string, wantInterfaces bool) (*incusapi.Instance, *incusapi.InstanceState, error) {
		inst, _, err := conn.GetInstance(ctx, project, name, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("reading instance %s/%s: %w", project, name, err)
		}

		state, _, err := conn.GetInstanceState(ctx, project, name)
		if err != nil {
			return nil, nil, fmt.Errorf("reading the state of %s/%s: %w", project, name, err)
		}

		if !wantInterfaces || inst.StatusCode != incusapi.Running || state.StatusCode != incusapi.Running || !hasNICDevices(&inst.Instance) || hasAddresses(state) {
			return &inst.Instance, state, nil
		}

		pollCtx, cancel := context.WithTimeout(ctx, ipTimeout)
		defer cancel()

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-pollCtx.Done():
				logger.Warn("timed out waiting for instance IP address",
					"project", project,
					"instance", name,
					"timeout", ipTimeout,
				)

				return &inst.Instance, state, nil

			case <-ticker.C:
				newState, _, err := conn.GetInstanceState(pollCtx, project, name)
				if err != nil {
					if pollCtx.Err() != nil {
						logger.Warn("timed out waiting for instance IP address",
							"project", project,
							"instance", name,
							"timeout", ipTimeout,
						)

						return &inst.Instance, state, nil
					}

					continue
				}

				state = newState
				if state.StatusCode != incusapi.Running || hasAddresses(state) {
					return &inst.Instance, state, nil
				}
			}
		}
	}
}

// deferred is every Incus read this plugin has sent and not answered yet: the
// one in flight per key, what the pool refused, and what is held back until the
// first whole-fleet pass lands.
//
// Owned by the goroutine Run owns. A pool worker touches results and nothing
// else here, which is what keeps this free of a mutex.
type deferred struct {
	// read, readNet, readProject and fleet are fields rather than the connection
	// itself, so a test can supply Incus values without a daemon. fleet is what a
	// run lists; the other three are what one read asks for.
	read        readFunc
	readNet     netReadFunc
	readProject projectReadFunc
	fleet       sweeperConn

	pool    *ants.Pool
	timeout time.Duration

	// results is buffered to the worker count, so a worker never blocks handing
	// one back.
	results chan result

	// calls is the read in flight for each key; a second event on a key joins
	// it rather than issuing another.
	calls map[string]*call

	// pendingProject is instance reads held until the project's own read lands.
	pendingProject map[string][]string

	// warm says the first whole-fleet pass has landed, so every network an
	// instance might sit on is known. Never cleared once set.
	warm bool

	// asked says the run reached the end of its networks. The flush waits for
	// that and for owed to reach zero, whichever is later.
	asked bool

	// owed is how many project reads the run still has out. An instance event
	// that overtook them would walk without the project it belongs to, and
	// withProject would take the project for one no run had reached.
	owed int

	// cold is every instance key an event arrived for before warm, in arrival
	// order; sent for real once the first pass lands.
	cold []string

	// waiting is what the pool refused, oldest first; timer is when to offer
	// them again.
	waiting []*call
	timer   *time.Timer
}

// newDeferred prepares the table. The pool is opened by start, whose lifetime
// belongs to Run rather than to the plugin.
func newDeferred(workers int, timeout time.Duration) *deferred {
	timer := time.NewTimer(poolDelay)
	timer.Stop()

	return &deferred{
		timeout:        timeout,
		results:        make(chan result, workers),
		calls:          map[string]*call{},
		pendingProject: map[string][]string{},
		timer:          timer,
	}
}

// start opens the read pool.
func (d *deferred) start(workers int) error {
	pool, err := ants.NewPool(workers, ants.WithNonblocking(true))
	if err != nil {
		return fmt.Errorf("creating the read pool: %w", err)
	}

	d.pool = pool

	return nil
}

// stop releases the pool and disarms the retry. Reads in flight are abandoned
// rather than waited for.
func (d *deferred) stop() {
	d.pool.Release()
	d.timer.Stop()
}

// send sends one read, or joins the one already out for that key: coalescing
// saves the read, not the event.
//
// An instance read arriving cold joins the table and waits for flush instead:
// every network it might sit on is unknown until the first whole-fleet run
// lands. A network read never waits - it is what the others resolve against.
func (d *deferred) send(ctx context.Context, c *call) {
	out, running := d.calls[c.key]
	if running {
		out.join(c)

		return
	}

	d.calls[c.key] = c

	if c.kind == kindProject {
		d.owed++
	}

	if !d.warm && c.kind == kindInstance {
		d.cold = append(d.cold, c.key)

		return
	}

	if c.kind == kindInstance {
		projKey := resourceKey(kindProject, c.project, "")
		_, projRunning := d.calls[projKey]
		if projRunning {
			d.pendingProject[c.project] = append(d.pendingProject[c.project], c.key)

			return
		}
	}

	err := d.submit(ctx, c)
	if err != nil {
		// Refused, not failed: it keeps its place and is offered again shortly.
		d.waiting = append(d.waiting, c)
		d.timer.Reset(poolDelay)
	}
}

// flush sends every read held cold, in arrival order. Called once, by the first
// run to land, after which nothing is held back again.
func (d *deferred) flush(ctx context.Context) {
	d.asked = true

	if d.warm || d.owed > 0 {
		return
	}

	cold := d.cold

	d.cold = nil
	d.warm = true

	for _, key := range cold {
		c := d.calls[key]
		delete(d.calls, key)

		d.send(ctx, c)
	}
}

// retry offers what the pool refused again, in refusal order. The first refusal
// stops it, since the pool is still full.
func (d *deferred) retry(ctx context.Context) {
	for len(d.waiting) > 0 {
		err := d.submit(ctx, d.waiting[0])
		if err != nil {
			break
		}

		d.waiting = d.waiting[1:]
	}

	if len(d.waiting) > 0 {
		d.timer.Reset(poolDelay)
	}
}

// owes is how many project reads the run still has out.
func (d *deferred) owes() int { return d.owed }

// done drops the call for one key, its read having landed. The last project
// read a run owes is what releases the instances held behind it.
func (d *deferred) done(ctx context.Context, c *call) {
	delete(d.calls, c.key)

	if c.kind != kindProject {
		return
	}

	d.owed--

	if d.asked {
		d.flush(ctx)
	}

	pending := d.pendingProject[c.project]
	delete(d.pendingProject, c.project)

	for _, key := range pending {
		instCall, ok := d.calls[key]
		if !ok {
			continue
		}

		err := d.submit(ctx, instCall)
		if err != nil {
			d.waiting = append(d.waiting, instCall)
			d.timer.Reset(poolDelay)
		}
	}
}

// submit offers one call to the pool, and reports whether it was taken.
//
// The deadline is set inside the task rather than around the submit, so a read
// that waited for a worker still gets its whole budget.
func (d *deferred) submit(ctx context.Context, c *call) error {
	project := c.project
	name := c.name
	kind := c.kind
	wantInterfaces := c.wantInterfaces

	err := d.pool.Submit(func() {
		readCtx, cancel := context.WithTimeout(ctx, d.timeout)
		defer cancel()

		res := result{call: c}

		switch kind {
		case kindNetwork:
			res.network, _, res.err = d.readNet(readCtx, project, name)

		case kindProject:
			res.project, _, res.err = d.readProject(readCtx, project)

		default:
			res.instance, res.state, res.err = d.read(readCtx, project, name, wantInterfaces)
		}

		select {
		case d.results <- res:
		case <-ctx.Done():
		}
	})
	if err != nil {
		if errors.Is(err, ants.ErrPoolOverload) {
			return err
		}

		return fmt.Errorf("submitting a read: %w", err)
	}

	return nil
}
