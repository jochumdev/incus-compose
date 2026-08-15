// Package client - a High-level Incus API wrapper with resource management and parallel execution.
//
// This package provides a compose-spec friendly interface for managing Incus resources:
// instances, networks, volumes, profiles, and images.
//
// It also adds support for container image building by using `podman`.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/util"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/shared"
)

// Client wraps a project-scoped Incus client with resource management.
type Client struct {
	ctx          context.Context
	globalClient *GlobalClient
	config       ClientConfig

	project      string
	incusProject string
	created      bool

	incus      *iclient.Connection
	imageCache *Client
	logger     *slog.Logger

	// healthdMu guards the caches below; every instance start asks for them.
	healthdMu sync.Mutex

	// Cache for FindHealthd
	healthd string

	// Cache for healthdTarget, which may point at another project.
	healthdConn *iclient.Connection
	healthdName string

	// Resource storage
	resources ResourceStore

	// clonesMu guards clones.
	clonesMu sync.Mutex

	// clones keep their own resources, which this client's event listener must reach.
	clones []*Client

	// hookBefore is called before any action
	hookBefore func(ctx context.Context, action Action, r Resource, args Options, err error) error

	// hookAfter is called after any action
	hookAfter func(ctx context.Context, action Action, r Resource, args Options, err error) error

	hookOperation func(ctx context.Context, action Action, r Resource, args Options, op <-chan incusApi.Operation, err error) error

	// hookConnected is called once when the client is ready, before any action.
	hookConnected func(err error) error

	// hookDone is called once when the client's work is complete, for cleanup.
	hookDone func(err error) error
}

func (c *GlobalClient) newProjectClient(name, incusName string, created bool) (*Client, error) {
	config := c.config
	config.DescriptionFormat = fmt.Sprintf(config.DescriptionFormat, name) + ":%s"

	cp := &Client{
		ctx:          c.ctx,
		globalClient: c,
		config:       config,
		project:      name,
		incusProject: incusName,
		created:      created,
		incus:        c.incus.WithProject(incusName),
		imageCache:   c.imageCache,
		logger:       c.logger.With("project", name),

		hookBefore: c.hookBefore,
		hookAfter:  c.hookAfter,

		hookOperation: c.hookOperation,

		hookConnected: func(err error) error { return err },
		hookDone:      func(err error) error { return err },
	}

	addEventHook(cp)

	if c.IsDebugging() {
		cp.logger = cp.logger.With("incus_project", incusName)
	}

	c.projects = append(c.projects, cp)

	if c.IsDebugging() {
		// Debug logging hooks
		c.AddHookBefore(func(_ context.Context, action Action, r Resource, args Options, err error) error {
			c.LogDebug("Running", "action", action, "kind", r.Kind(), "name", r.Name(), "incus_name", r.IncusName())
			return err
		})
		c.AddHookAfter(func(_ context.Context, action Action, r Resource, args Options, err error) error {
			if err != nil {
				c.LogWarn("Result with error", "action", action, "kind", r.Kind(), "name", r.Name(), "incus_name", r.IncusName(), "created", r.Created(), "error", err)
				return err
			}

			c.LogDebug("Run", "action", action, "kind", r.Kind(), "name", r.Name(), "incus_name", r.IncusName(), "created", r.Created())
			return nil
		})
	}

	return cp, nil
}

// Clone returns a copy of the client, where you can add independent hooks and resources.
// Resources are NOT shared.
func (c *Client) Clone() *Client {
	c.healthdMu.Lock()
	healthd, healthdConn, healthdName := c.healthd, c.healthdConn, c.healthdName
	c.healthdMu.Unlock()

	clone := &Client{
		ctx:          c.ctx,
		globalClient: c.globalClient,
		config:       c.config,
		project:      c.project,
		incusProject: c.incusProject,
		created:      c.created,
		incus:        c.incus,
		imageCache:   c.imageCache,
		logger:       c.logger,

		hookBefore: c.hookBefore,
		hookAfter:  c.hookAfter,

		hookOperation: c.hookOperation,

		hookConnected: c.hookConnected,
		hookDone:      c.hookDone,

		healthd:     healthd,
		healthdConn: healthdConn,
		healthdName: healthdName,
	}

	c.clonesMu.Lock()
	c.clones = append(c.clones, clone)
	c.clonesMu.Unlock()

	return clone
}

// rangeResources runs f over this client's resources and every clone's.
func (c *Client) rangeResources(f func(r Resource)) {
	c.resources.Range(f)

	c.clonesMu.Lock()
	clones := slices.Clone(c.clones)
	c.clonesMu.Unlock()

	for _, clone := range clones {
		clone.rangeResources(f)
	}
}

// Global returns the GlobalClient associated with this project client.
func (c *Client) Global() *GlobalClient {
	return c.globalClient
}

// GlobalConnection returns the global incus connection (with the default project).
func (c *Client) GlobalConnection() (*iclient.Connection, error) {
	return c.globalClient.Connection()
}

// Project returns the user-facing project name.
func (c *Client) Project() string {
	return c.project
}

// IncusProject returns the sanitized Incus project name.
func (c *Client) IncusProject() string {
	return c.incusProject
}

// IsRemote returns true if connected via network (not unix socket).
func (c *Client) IsRemote() bool {
	return c.globalClient.IsRemote()
}

// IsDebugging returns if debugging is enabled.
func (c *Client) IsDebugging() bool {
	return c.globalClient.IsDebugging()
}

// LogDebug logs an debug message.
// The `any` here is ok.
func (c *Client) LogDebug(msg string, args ...any) {
	c.logger.DebugContext(c.ctx, msg, args...)
}

// LogInfo logs an info message.
// The `any` here is ok.
func (c *Client) LogInfo(msg string, args ...any) {
	c.logger.InfoContext(c.ctx, msg, args...)
}

// LogWarn logs an warning message.
// The `any` here is ok.
func (c *Client) LogWarn(msg string, args ...any) {
	c.logger.WarnContext(c.ctx, msg, args...)
}

// LogError logs an error.
// The `any` here is ok.
func (c *Client) LogError(msg string, args ...any) {
	c.logger.ErrorContext(c.ctx, msg, args...)
}

// WarnError logs a warning if the given error is not nil, use in deferred for example.
func (c *Client) WarnError(f func() error, message string) {
	err := f()
	if err != nil {
		if message == "" {
			message = "Error happened"
		}
		c.LogWarn(message, "error", err)
	}
}

// Connection returns the project-scoped Incus connection, safe for concurrent use.
func (c *Client) Connection() (*iclient.Connection, error) {
	if c.incus == nil {
		return nil, ErrDisconnected
	}

	return c.incus, nil
}

// Config returns a copy of the clients config.
func (c *Client) Config() ClientConfig {
	return c.config
}

// IsConnected reports whether the project client can run Incus operations.
func (c *Client) IsConnected() bool {
	return c != nil && c.incus != nil
}

// // HasResource returns if the client has the given resource.
// func (c *Client) HasResource(kind Kind, incusName string) bool {
// 	idx := slices.IndexFunc(c.resources.All(), func(r Resource) bool {
// 		return r.Kind() == kind && r.IncusName() == incusName
// 	})

// 	return idx != -1
// }

// Resource returns an existing resource or creates a new one. You might use a nil config for lookups.
func (c *Client) Resource(kind Kind, name string, config Config) (Resource, error) {
	if config == nil {
		res := c.resources.Get(kind, name, false)
		if res != nil {
			return res, nil
		}

		res = c.resources.Get(kind, name, true)
		if res == nil {
			return nil, ErrNotFound.WithKindName(kind, name)
		}
		return res, nil
	}

	return c.resources.GetOrCreate(kind, name, func() (Resource, error) {
		switch kind {
		case KindProfile:
			return newProfile(c, name, config)
		case KindNetwork:
			return newNetwork(c, name, config)
		case KindStorageVolume:
			return newStorageVolume(c, name, config)
		case KindImage:
			return newImage(c, name, config)
		case KindInstance:
			return newInstance(c, name, config)
		default:
			return nil, ErrUnknownResource.WithText(string(kind))
		}
	})
}

// AddHookBefore adds a hook that will be executed before any action.
// You may use it for abort control.
func (c *Client) AddHookBefore(hook func(ctx context.Context, action Action, r Resource, args Options, err error) error) {
	prevHook := c.hookBefore
	newHook := func(ctx context.Context, action Action, r Resource, args Options, err error) error {
		// Run previous hooks FIRST (FIFO)
		if err := prevHook(ctx, action, r, args, err); err != nil {
			return err
		}
		// Then run the new hook
		return hook(ctx, action, r, args, nil)
	}

	c.hookBefore = newHook
}

// AddHookAfter adds a hook that will be executed after any action (LIFO order).
func (c *Client) AddHookAfter(hook func(ctx context.Context, action Action, r Resource, args Options, err error) error) {
	prevHook := c.hookAfter
	newHook := func(ctx context.Context, action Action, r Resource, args Options, err error) error {
		// Run new hook FIRST, then pass result to previous hooks (LIFO)
		err = hook(ctx, action, r, args, err)
		return prevHook(ctx, action, r, args, err)
	}

	c.hookAfter = newHook
}

// AddHookConnected adds a hook that will be executed when the client connects (FIFO order).
func (c *Client) AddHookConnected(hook func(err error) error) {
	prevHook := c.hookConnected
	newHook := func(err error) error {
		// Run previous hooks FIRST (FIFO)
		if err := prevHook(err); err != nil {
			return err
		}
		// Then run the new hook
		return hook(nil)
	}

	c.hookConnected = newHook
}

// AddHookDone adds a hook that will be executed when the client's work is complete (LIFO order).
func (c *Client) AddHookDone(hook func(err error) error) {
	prevHook := c.hookDone
	newHook := func(err error) error {
		// Run new hook FIRST, then pass result to previous hooks (LIFO)
		err = hook(err)
		return prevHook(err)
	}

	c.hookDone = newHook
}

// IgnoreError ignores an "warning" errors for the given kind and the rest of the session.
func (c *Client) IgnoreError(iAction Action, iErr error) {
	c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
		if err != nil && iAction == action {
			ok := errors.Is(err, iErr)
			if ok {
				c.LogDebug("Ignoring error", "action", action, "kind", r.Kind(), "name", r.Name(), "incus_name", r.IncusName(), "created", r.Created(), "error", err)
				return nil
			}
		}

		return err
	})
}

// Open fires the connected hooks. Call once after registering all hooks,
// before running any stack actions.
func (c *Client) Open() error {
	return c.hookConnected(nil)
}

// Done fires the done hooks. Call when the client's work is complete.
func (c *Client) Done() error {
	return c.hookDone(nil)
}

// FindHealthd returns the name of the healthd instance in the project,
// identified by user.healthcheck.daemon=true.
func (c *Client) FindHealthd() (string, error) {
	c.healthdMu.Lock()
	cached := c.healthd
	c.healthdMu.Unlock()

	if cached != "" {
		return cached, nil
	}

	if c.incus == nil {
		return "", ErrNotFound.WithText(": within FindHealthd")
	}

	instances, err := c.incus.GetInstances(c.ctx, nil)
	if err != nil {
		return "", ErrUnknown.Wrap(fmt.Errorf("listing instances: %w", err))
	}

	for _, inst := range instances {
		if util.IsTrue(inst.Config[HealthKeyPrefix+"daemon"]) {
			c.healthdMu.Lock()
			c.healthd = inst.Name
			c.healthdMu.Unlock()

			return inst.Name, nil
		}
	}

	return "", ErrNotFound.WithText(": within FindHealthd")
}

// HealthdRunning reports whether the daemon watching this project is up,
// looking wherever the project's stored scope says it lives.
func (c *Client) HealthdRunning() (bool, error) {
	conn, name, err := c.healthdTarget()
	if err != nil {
		return false, err
	}

	state, _, err := conn.GetInstanceState(c.ctx, name)
	if err != nil {
		return false, fmt.Errorf("getting the healthd %q instance state: %w", name, err)
	}

	return state.StatusCode == incusApi.Running, nil
}

// healthdTarget locates the daemon watching this project. Every instance start
// asks, so the answer is cached; it cannot change while a command runs.
func (c *Client) healthdTarget() (*iclient.Connection, string, error) {
	c.healthdMu.Lock()
	cachedConn, cachedName := c.healthdConn, c.healthdName
	c.healthdMu.Unlock()

	if cachedConn != nil {
		return cachedConn, cachedName, nil
	}

	if c.incus == nil {
		return nil, "", ErrNotFound.WithText(": within healthdTarget")
	}

	cfg, err := c.globalClient.ProjectConfig(c.project)
	if err != nil {
		return nil, "", err
	}

	conn := c.incus
	if cfg[shared.HealthScopeKey] == shared.HealthScopeGlobal {
		conn = c.globalClient.incus.WithProject(c.config.SystemProject)
	}

	instances, err := conn.GetInstances(c.ctx, nil)
	if err != nil {
		return nil, "", ErrUnknown.Wrap(fmt.Errorf("listing instances: %w", err))
	}

	for _, inst := range instances {
		if !util.IsTrue(inst.Config[HealthKeyPrefix+"daemon"]) {
			continue
		}

		c.healthdMu.Lock()
		c.healthdConn, c.healthdName = conn, inst.Name
		c.healthdMu.Unlock()

		return conn, inst.Name, nil
	}

	return nil, "", ErrNotFound.WithText(": within healthdTarget")
}

// InstanceExists reports whether an instance with the given name exists in Incus.
func (c *Client) InstanceExists(name string) (bool, error) {
	if c.incus == nil {
		return false, nil
	}

	_, _, err := c.incus.GetInstance(c.ctx, SanitizeIncusName(name, -1), nil)
	return err == nil, nil
}

// ResolveImageFingerprint returns the first alias name for the given fingerprint,
// or the fingerprint itself if no alias is found or the lookup fails.
func (c *Client) ResolveImageFingerprint(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	img, _, err := c.globalClient.incus.GetImage(c.ctx, fingerprint, nil)
	if err == nil && img != nil && len(img.Aliases) > 0 {
		return img.Aliases[0].Name
	}

	c.LogWarn("failed to resolve image", "fingerprint", fingerprint)
	return fingerprint
}
