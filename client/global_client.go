package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"

	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
)

// SwapWriter is an io.Writer whose destination can be swapped at runtime.
type SwapWriter struct {
	mu sync.RWMutex
	w  io.Writer
}

// NewSwapWriter creates a LogWriter initially writing to w.
func NewSwapWriter(w io.Writer) *SwapWriter {
	return &SwapWriter{w: w}
}

func (l *SwapWriter) Write(b []byte) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.w.Write(b)
}

// Swap replaces the destination and returns the previous one.
func (l *SwapWriter) Swap(w io.Writer) io.Writer {
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.w
	l.w = w
	return old
}

// ClientConfig holds configuration options for the Client.
type ClientConfig struct {
	// URL is the Incus server URL to connect to.
	URL string

	// Logger to use within this client.
	Logger *slog.Logger

	// Stdout is the stdout writer to use.
	Stdout io.Writer

	// Stderr is the swappable stderr writer.
	Stderr *SwapWriter

	// NetworkPrefix is the prefix for new networks (default: "ic-").
	NetworkPrefix string

	// DefaultStoragePool is the storage pool to use for volumes (default: "default").
	DefaultStoragePool string

	// DescriptionFormat is the format string for resource descriptions (default: "incus-compose: %s").
	DescriptionFormat string

	// ProvidedConnection allows injecting an existing connection (for testing).
	ProvidedConnection *iclient.Connection

	// CacheProject is the project name to use as image cache.
	// If set, the project will be created if it doesn't exist.
	CacheProject string

	// SystemProject is the project holding the instances the library runs.
	SystemProject string
}

// ClientOption is a functional option for configuring the Client.
type ClientOption func(*ClientConfig)

// ClientURL sets the Incus server ClientURL.
func ClientURL(u string) ClientOption {
	return func(c *ClientConfig) { c.URL = u }
}

// ClientLogger sets the client to use within the created client.
func ClientLogger(l *slog.Logger) ClientOption {
	return func(c *ClientConfig) { c.Logger = l }
}

// ClientStdout sets the clients stdout.
func ClientStdout(w io.Writer) ClientOption {
	return func(c *ClientConfig) { c.Stdout = w }
}

// ClientStderrWriter sets the swappable writer.
func ClientStderrWriter(lw *SwapWriter) ClientOption {
	return func(c *ClientConfig) { c.Stderr = lw }
}

// ClientDefaultStoragePool sets the default storage pool name.
func ClientDefaultStoragePool(n string) ClientOption {
	return func(c *ClientConfig) { c.DefaultStoragePool = n }
}

// ClientNetworkPrefix sets the prefix for network names.
func ClientNetworkPrefix(n string) ClientOption {
	return func(c *ClientConfig) { c.NetworkPrefix = n }
}

// ClientDescriptionFormat sets the format string for resource descriptions.
func ClientDescriptionFormat(n string) ClientOption {
	return func(c *ClientConfig) { c.DescriptionFormat = n }
}

// ClientProvideConnection injects an existing Incus connection.
func ClientProvideConnection(conn *iclient.Connection) ClientOption {
	return func(c *ClientConfig) {
		c.ProvidedConnection = conn
	}
}

// ClientCacheProject sets the project name to use as image cache.
func ClientCacheProject(n string) ClientOption {
	return func(c *ClientConfig) { c.CacheProject = n }
}

// ClientSystemProject sets the project holding the library's own instances.
func ClientSystemProject(n string) ClientOption {
	return func(c *ClientConfig) { c.SystemProject = n }
}

// GlobalClient provides a high-level interface to Incus operations.
type GlobalClient struct {
	ctx    context.Context
	config ClientConfig

	logger *slog.Logger

	stdout    io.Writer
	stderr    *SwapWriter
	projects  []*Client
	cliConfig *iclient.Config

	// mu guards the connection state below; Connect writes it while workers read it.
	mu sync.Mutex

	incus      *iclient.Connection
	imageCache *Client
	unix       bool
	connected  bool

	// Read once at connect: GetServer memoizes into the shared *ProtocolIncus,
	// so calling it per service/network/port both races and re-queries.
	apiExtensions []string
	httpsAddress  string

	// Cache for ConnectionIP()
	connectionIPs []net.IP

	// hookBefore is called hookBefore any action.
	hookBefore func(ctx context.Context, action Action, r Resource, args Options, err error) error

	// hookAfter is called hookAfter any action.
	hookAfter func(ctx context.Context, action Action, r Resource, args Options, err error) error

	progressHandler func(action Action, r Resource, args Options, p Progress)

	// outputHandler is called when a resource produces output (e.g., logs).
	outputHandler func(action Action, r Resource, data []byte)

	hookOperation func(ctx context.Context, action Action, r Resource, args Options, op <-chan incusApi.Operation, err error) error
}

// New creates a new Client with the provided context and logger.
func New(ctx context.Context, opts ...ClientOption) *GlobalClient {
	config := ClientConfig{
		Logger:             slog.Default(),
		DefaultStoragePool: "detect",
		NetworkPrefix:      "ic-",
		DescriptionFormat:  DefaultSystemProject + ": %s",
		SystemProject:      DefaultSystemProject,
		Stdout:             os.Stdout,
		Stderr:             NewSwapWriter(os.Stderr),
	}

	for _, o := range opts {
		o(&config)
	}

	// Load CLI config by default for image server resolution
	cliConf, err := iclient.ReadConfig("")
	if err != nil {
		cliConf = iclient.ConfigDefaultConfig()
	}

	c := &GlobalClient{
		ctx:       ctx,
		config:    config,
		logger:    config.Logger,
		stdout:    config.Stdout,
		stderr:    config.Stderr,
		cliConfig: cliConf,
	}

	c.hookBefore = func(_ context.Context, action Action, r Resource, args Options, err error) error {
		return err
	}

	c.hookAfter = func(_ context.Context, action Action, r Resource, args Options, err error) error {
		if err == nil {
			return nil
		}

		var cError *Error
		if ok := errors.As(err, &cError); ok {
			return cError.WithResource(r)
		}

		return ErrUnknown.WithResource(r).Wrap(err)
	}

	c.hookOperation = func(ctx context.Context, action Action, r Resource, args Options, op <-chan incusApi.Operation, err error) error {
		if err != nil {
			return err
		}

		if op == nil {
			return ErrNilPointer
		}

		_, err = iclient.WaitOperation(ctx, c.watchOperation(ctx, action, r, args, op))
		if err != nil {
			return ErrOperation.Wrap(err)
		}

		return nil
	}

	return c
}

// watchOperation reports an operation's updates to the progress handler on the way past.
func (c *GlobalClient) watchOperation(ctx context.Context, action Action, r Resource, args Options, updates <-chan incusApi.Operation) <-chan incusApi.Operation {
	if c.progressHandler == nil {
		return updates
	}

	out := make(chan incusApi.Operation)

	go func() {
		defer close(out)

		for update := range updates {
			c.reportProgress(action, r, args, update)

			select {
			case out <- update:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out
}

// NewTestClient creates a new GlobalClient for testing.
func NewTestClient(ctx context.Context) (*GlobalClient, error) {
	remote, ok := os.LookupEnv("INCUS_REMOTE")
	if !ok {
		remote = "local"
	}

	slog.DebugContext(ctx, "Connecting", "remote", remote)

	conn, err := DialRemote("", remote)
	if err != nil {
		return nil, err
	}

	opts := []ClientOption{ClientProvideConnection(conn)}

	pool, ok := os.LookupEnv("INCUS_COMPOSE_STORAGE_POOL")
	if ok {
		opts = append(opts, ClientDefaultStoragePool(pool))
	}

	cacheProject := "incus-compose-tests-cache"
	p, ok := os.LookupEnv("INCUS_COMPOSE_IMAGE_CACHE")
	if ok {
		cacheProject = p
	}

	// Use own cache project for tests.
	opts = append(opts, ClientCacheProject(cacheProject))

	c := New(ctx, opts...)
	if err := c.Connect(); err != nil {
		return nil, err
	}

	return c, nil
}

// NewOfflineClient creates a disconnected project client for resource planning.
// It can create in-memory resources, but cannot run Incus operations.
func NewOfflineClient(ctx context.Context, projectName string) *Client {
	gc := New(ctx)
	gc.unix = true
	config := gc.config
	config.DescriptionFormat = fmt.Sprintf(config.DescriptionFormat, projectName) + ":%s"

	return &Client{
		ctx:          ctx,
		globalClient: gc,
		config:       config,
		project:      projectName,
		incusProject: SanitizeProjectName(projectName),
		logger:       gc.logger.With("project", projectName),
	}
}

// DialRemote connects to a remote of the Incus CLI configuration at path.
// An empty path is the default location, an empty remote the default remote.
func DialRemote(path string, remote string) (*iclient.Connection, error) {
	config, err := iclient.ReadConfig(path)
	if err != nil {
		return nil, ErrConnectionFailed.Wrap(err)
	}

	info, err := config.RemoteInfos(remote)
	if err != nil {
		return nil, ErrConnectionFailed.Wrap(err)
	}

	conn, err := iclient.NewConnection(info)
	if err != nil {
		return nil, ErrConnectionFailed.Wrap(err)
	}

	return conn, nil
}

// Connect establishes a connection to the Incus server.
func (c *GlobalClient) Connect() error {
	if c.config.ProvidedConnection == nil {
		return errors.New("provide a ProvidedConnection")
	}

	// Force "default" project for global client - project-scoped clients are created via EnsureProject.
	incus := c.config.ProvidedConnection.WithProject("default")

	info, err := incus.GetConnectionInfo(c.ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConnectionFailed, err)
	}

	c.mu.Lock()
	c.config.URL = info.URL
	c.unix = info.SocketPath != ""
	c.incus = incus
	c.connected = true
	c.mu.Unlock()

	c.logger.Debug("Connected", "url", c.config.URL)

	// Image caching copies images between Incus projects using pull mode, which
	// needs the server reachable over the network. Fail early with a clear
	// message instead of a cryptic copy error later.
	server, _, err := c.incus.GetServer(c.ctx)
	if err != nil {
		return ErrConnectionFailed.Wrap(err)
	}

	if server.Config["core.https_address"] == "" {
		return ErrServerNotListening
	}

	c.mu.Lock()
	c.apiExtensions = server.APIExtensions
	c.httpsAddress = server.Config["core.https_address"]
	c.mu.Unlock()

	if c.config.DefaultStoragePool == "detect" {
		if err := c.detectStoragePool(); err != nil {
			return err
		}
	}

	return c.setupImageCache()
}

// Connection returns the connection scoped to the default project, safe for concurrent use.
func (c *GlobalClient) Connection() (*iclient.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil, ErrDisconnected
	}

	return c.incus, nil
}

// detectStoragePool prefers the storage pool used by the default profile's
// root disk device, falling back to the first storage pool on the server.
func (c *GlobalClient) detectStoragePool() error {
	profile, _, err := c.incus.GetProfile(c.ctx, "default")
	if err == nil {
		for _, device := range profile.Devices {
			if device["type"] == "disk" && device["path"] == "/" && device["pool"] != "" {
				c.config.DefaultStoragePool = device["pool"]
				return nil
			}
		}
	}

	names, err := c.incus.GetStoragePoolNames(c.ctx)
	if err != nil {
		return fmt.Errorf("detecting storage pool: %w", err)
	}

	if len(names) == 0 {
		return fmt.Errorf("detecting storage pool: no storage pools found on server")
	}

	c.config.DefaultStoragePool = names[0]
	return nil
}

// setupImageCache configures the image cache based on CacheProject or defaults.
func (c *GlobalClient) setupImageCache() error {
	if c.config.CacheProject != "" {
		// Use dedicated cache project (create if needed)
		cacheClient, err := c.EnsureProject(c.config.CacheProject, EnsureProjectWithCreate())
		if err != nil {
			// Try again without create for parallel creates.
			cacheClient, err = c.EnsureProject(c.config.CacheProject)
			if err != nil {
				return fmt.Errorf("ensuring cache project %s: %w", c.config.CacheProject, err)
			}
		}
		c.imageCache = cacheClient
	}

	return nil
}

// LogError logs an error.
// The `any` here is ok.
func (c *GlobalClient) LogError(msg string, args ...any) {
	c.logger.ErrorContext(c.ctx, msg, args...)
}

// LogWarn logs a warning.
// The `any` here is ok.
func (c *GlobalClient) LogWarn(msg string, args ...any) {
	c.logger.WarnContext(c.ctx, msg, args...)
}

// LogInfo logs an info message.
// The `any` here is ok.
func (c *GlobalClient) LogInfo(msg string, args ...any) {
	c.logger.InfoContext(c.ctx, msg, args...)
}

// SwapStderr redirects this client's log output to w.
func (c *GlobalClient) SwapStderr(w io.Writer) io.Writer {
	return c.stderr.Swap(w)
}

// Stdout returns this clients standard output.
func (c *GlobalClient) Stdout() io.Writer {
	return c.stdout
}

// Stderr returns this clients error output.
func (c *GlobalClient) Stderr() io.Writer {
	return c.stderr
}

// LogDebug logs a debug message.
// The `any` here is ok.
func (c *GlobalClient) LogDebug(msg string, args ...any) {
	c.logger.Log(c.ctx, slog.LevelDebug, msg, args...)
}

// IsDebugging returns true if debug logging is enabled.
func (c *GlobalClient) IsDebugging() bool {
	return c.logger.Enabled(c.ctx, slog.LevelDebug)
}

// IsConnected returns true if the client is connected.
func (c *GlobalClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.connected
}

// IsRemote returns true if connected via network (not unix socket).
func (c *GlobalClient) IsRemote() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return !c.unix
}

// CliConfig returns the CLI config for image server resolution.
func (c *GlobalClient) CliConfig() *iclient.Config {
	return c.cliConfig
}

func (c *GlobalClient) getProject(name string) (*Client, error) {
	incusName := SanitizeProjectName(name)

	_, _, err := c.incus.GetProject(c.ctx, incusName)
	if err != nil {
		return nil, err
	}

	c.logger.DebugContext(c.ctx, "Got project", "name", name, "incus_name", incusName)
	return c.newProjectClient(name, incusName, false)
}

// EnsureProjectOption is a functional option for configuring project creation.
type EnsureProjectOption func(*ensureProjectOptions)

type ensureProjectOptions struct {
	create      bool
	config      map[string]string
	skipHealthd bool
}

// EnsureProjectWithCreate enables project creation if the project doesn't exist.
func EnsureProjectWithCreate() EnsureProjectOption {
	return func(opts *ensureProjectOptions) {
		opts.create = true
	}
}

// EnsureProjectWithConfig sets configuration options to apply when creating the project.
// These options are merged with default features configuration.
func EnsureProjectWithConfig(config map[string]string) EnsureProjectOption {
	return func(opts *ensureProjectOptions) {
		opts.config = config
	}
}

// EnsureProjectWithSkipHealthd skips as the name says any healthd calls.
func EnsureProjectWithSkipHealthd() EnsureProjectOption {
	return func(opts *ensureProjectOptions) {
		opts.skipHealthd = true
	}
}

// ProjectConfig returns the Incus project's config, empty if it does not exist.
func (c *GlobalClient) ProjectConfig(name string) (map[string]string, error) {
	incusName := SanitizeProjectName(name)

	project, _, err := c.incus.GetProject(c.ctx, incusName)
	if incusApi.StatusErrorCheck(err, http.StatusNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading project %s: %w", incusName, err)
	}

	return project.Config, nil
}

// ProjectsWithConfig returns the names of the Incus projects carrying key set
// to value.
func (c *GlobalClient) ProjectsWithConfig(key, value string) ([]string, error) {
	projects, err := c.incus.GetProjects(c.ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}

	var names []string
	for _, project := range projects {
		if project.Config[key] == value {
			names = append(names, project.Name)
		}
	}

	return names, nil
}

// AddMissingProjectConfig adds declared config keys the project does not have yet.
func (c *GlobalClient) AddMissingProjectConfig(name string, config map[string]string) error {
	if len(config) == 0 {
		return nil
	}

	incusName := SanitizeProjectName(name)

	project, etag, err := c.incus.GetProject(c.ctx, incusName)
	if err != nil {
		return fmt.Errorf("reading project %s: %w", incusName, err)
	}

	missing := []string{}
	writable := project.Writable()

	if writable.Config == nil {
		writable.Config = incusApi.ConfigMap{}
	}

	for key, value := range config {
		_, ok := writable.Config[key]
		if ok {
			continue
		}

		writable.Config[key] = value
		missing = append(missing, key)
	}

	if len(missing) == 0 {
		return nil
	}

	slices.Sort(missing)
	c.logger.DebugContext(c.ctx, "Adding missing project config", "name", name, "incus_name", incusName, "keys", missing)

	err = c.incus.UpdateProject(c.ctx, incusName, writable, etag)
	if err != nil {
		return fmt.Errorf("updating project %s: %w", incusName, err)
	}

	return nil
}

func (c *GlobalClient) createProject(name string, config map[string]string) (*Client, error) {
	incusName := SanitizeProjectName(name)

	// Merge user-provided config with defaults
	projectConfig := incusApi.ConfigMap{"features.profiles": "true"}
	for k, v := range config {
		projectConfig[k] = v
	}

	// Create project
	err := c.incus.CreateProject(c.ctx, incusApi.ProjectsPost{
		Name: incusName,
		ProjectPut: incusApi.ProjectPut{
			Description: fmt.Sprintf(c.config.DescriptionFormat, name),
			Config:      projectConfig,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating project %s (incus: %s): %w", name, incusName, err)
	}

	// c.logger.DebugContext(c.Ctx, "Created project", "name", name, "incus_name", incusName)
	return c.newProjectClient(name, incusName, true)
}

// EnsureProject ensures a project exists and returns a Client for it.
// Options control whether the project is created if it doesn't exist and what config to apply.
func (c *GlobalClient) EnsureProject(name string, opts ...EnsureProjectOption) (*Client, error) {
	if !c.IsConnected() {
		return nil, ErrDisconnected
	}

	// Apply options
	options := &ensureProjectOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Check if already known
	for _, p := range c.projects {
		if p != nil && p.project == name {
			return p, nil
		}
	}

	p, err := c.getProject(name)
	if err == nil {
		// Same reason as Instance.addMissingConfig.
		if err := c.AddMissingProjectConfig(name, options.config); err != nil {
			return nil, err
		}

		return p, nil
	}

	if !options.create {
		return nil, ErrNotFound.WithKindName(KindProject, name).Wrap(err)
	}

	return c.createProject(name, options.config)
}

// DeleteProject deletes a project and removes it from the cache.
func (c *GlobalClient) DeleteProject(name string, force bool) error {
	if !c.IsConnected() {
		return ErrDisconnected
	}

	incusName := SanitizeProjectName(name)

	// c.logger.DebugContext(c.Ctx, "Deleting project", "name", name, "incus_name", incusName)

	err := c.incus.DeleteProject(c.ctx, incusName, &iclient.DeleteProjectArgs{Force: force})
	if err != nil {
		return err
	}

	// Delete networks - they are global, not project-scoped
	for _, p := range c.projects {
		if p.project == name {
			// Delete networks first - they are global, not project-scoped
			networks, err := ByKind[*Network](p.resources.all(), KindNetwork)
			if err != nil {
				c.LogWarn("Deleting networks", "error", err)
				continue
			}

			for _, n := range networks {
				err = RunAction(c.ctx, n, ActionDelete, OptionForce())
				if err != nil {
					c.LogWarn("Deleting network", "network", n, "error", err)
				}
			}
			break
		}
	}

	// Remove from cache
	for i, p := range c.projects {
		if p.project == name {
			c.projects = append(c.projects[:i], c.projects[i+1:]...)
			break
		}
	}

	return nil
}

// Progress describes the live state of a long-running resource operation.
//
// Native Incus images report a real percentage ("rootfs: 42% (3.10MB/s)"),
// so Percent is set. OCI image pulls only emit status text ("Retrieving OCI
// image from registry"); for those Percent is -1 and only Text is meaningful,
// because the registry download runs as an opaque skopeo subprocess with no
// byte or percentage feedback.
type Progress struct {
	// Percent is 0-100, or -1 when the operation reports no percentage.
	Percent int

	// Text is the raw status text from Incus, empty when none was reported.
	Text string
}

func (c *GlobalClient) reportProgress(action Action, r Resource, args Options, opAPI incusApi.Operation) {
	if c.progressHandler == nil {
		return
	}

	// Incus stores progress in Metadata with keys ending in "_progress".
	p := Progress{Percent: -1}

	if opAPI.Metadata != nil {
		for key, val := range opAPI.Metadata {
			if strings.HasSuffix(key, "_progress") {
				if s, ok := val.(string); ok {
					p.Text = s
					p.Percent = parsePercent(s)
				}
				break
			}
		}
	}

	c.progressHandler(action, r, args, p)
}

// emitProgress sends a progress event that is not backed by an Incus operation,
// for in-client waits like blocking on a dependency's health. It is a no-op
// when no handler is registered. Like reportProgress it may run concurrently.
func (c *GlobalClient) emitProgress(action Action, r Resource, args Options, p Progress) {
	if c.progressHandler == nil {
		return
	}

	c.progressHandler(action, r, args, p)
}

// parsePercent extracts a "NN%" value from an Incus progress string, returning
// -1 when none is present (as with OCI status text). It finds the percent sign
// anywhere in the string, e.g. "rootfs: 42% (3.10MB/s)" -> 42.
func parsePercent(s string) int {
	idx := strings.Index(s, "%")
	if idx <= 0 {
		return -1
	}

	// Walk backwards from % to the start of the number.
	start := idx - 1
	for start >= 0 && s[start] >= '0' && s[start] <= '9' {
		start--
	}
	start++ // Move past the non-digit.
	if start >= idx {
		return -1
	}

	percent := -1
	if _, err := fmt.Sscanf(s[start:], "%d%%", &percent); err != nil {
		return -1
	}
	return percent
}

// AddHookBefore adds a hook that will be executed before any action (FIFO order).
// You may use it for abort control.
func (c *GlobalClient) AddHookBefore(hook func(ctx context.Context, action Action, r Resource, args Options, err error) error) {
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
func (c *GlobalClient) AddHookAfter(hook func(ctx context.Context, action Action, r Resource, args Options, err error) error) {
	prevHook := c.hookAfter
	newHook := func(ctx context.Context, action Action, r Resource, args Options, err error) error {
		// Run new hook FIRST, then pass result to previous hooks (LIFO)
		err = hook(ctx, action, r, args, err)
		return prevHook(ctx, action, r, args, err)
	}

	c.hookAfter = newHook
}

// SetOutputHandler sets the handler for resource output (e.g., logs).
// The handler receives raw bytes - formatting is the caller's responsibility.
func (c *GlobalClient) SetOutputHandler(handler func(action Action, r Resource, data []byte)) {
	c.outputHandler = handler
}

// SetProgressHandler sets the handler for live operation progress. Pass nil to
// disable. Operations run in parallel, so the handler may be called
// concurrently and must be safe for concurrent use.
func (c *GlobalClient) SetProgressHandler(handler func(action Action, r Resource, args Options, p Progress)) {
	c.progressHandler = handler
}

// NetworkBridgeIPs returns the IPv4 and IPv6 bridge addresses of an Incus network.
// The addresses are returned without CIDR notation.
// Addresses for which the network config key is absent or set to "none" are omitted.
func (c *GlobalClient) NetworkBridgeIPs(networkName string) (ipv4 []string, ipv6 []string, err error) {
	network, _, err := c.incus.GetNetwork(c.ctx, networkName)
	if err != nil {
		return nil, nil, fmt.Errorf("getting network %s: %w", networkName, err)
	}

	if v := network.Config["ipv4.address"]; v != "" && v != "none" {
		ip, _, err := net.ParseCIDR(v)
		if err == nil {
			ipv4 = append(ipv4, ip.String())
		}
	} else if v := network.Config["ipv6.address"]; v != "" && v != "none" {
		ip, _, err := net.ParseCIDR(v)
		if err == nil {
			ipv6 = append(ipv6, ip.String())
		}
	}

	return ipv4, ipv6, nil
}

// URL returns the URL of the connection.
func (c *GlobalClient) URL() (*url.URL, error) {
	if !c.IsRemote() {
		return nil, errors.New("GlobalClient.URL needs a non unix connection")
	}

	return url.Parse(c.config.URL)
}

// ConnectionIPs returns the IP of the current connection,
// this function is cached by GlobalClient.connectionIP.
func (c *GlobalClient) ConnectionIPs() ([]net.IP, error) {
	if !c.IsRemote() {
		return nil, errors.New("GlobalClient.ConnectionIP needs a non unix connection")
	}

	if c.connectionIPs != nil {
		return c.connectionIPs, nil
	}

	u, err := url.Parse(c.config.URL)
	if err != nil {
		return nil, fmt.Errorf("while parsing url %q: %w", c.config.URL, err)
	}

	ip := net.ParseIP(u.Hostname())
	if ip != nil {
		c.connectionIPs = []net.IP{ip}
		return c.connectionIPs, nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("while looking up the IP for %q: %w", c.config.URL, err)
	}

	ips := make([]net.IP, len(addrs))
	for i, addr := range addrs {
		ips[i] = addr.IP
	}

	c.connectionIPs = ips
	return c.connectionIPs, nil
}

// SameHost returns nil of the connected incus and the current host share the same ip else an error.
func (c *GlobalClient) SameHost() error {
	if c.unix {
		// Assume a unix socket is always on the same host.
		return nil
	}

	ips, err := c.ConnectionIPs()
	if err != nil {
		return err
	}

	// Source - https://stackoverflow.com/a/23558495
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("failed to get current hosts interfaces: %w", err)
	}

	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			return fmt.Errorf("failed to get ip addresses of %v: %w", i.Name, err)
		}

		for _, addr := range addrs {
			var ifaceIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ifaceIP = v.IP
			case *net.IPAddr:
				ifaceIP = v.IP
			}

			for _, ip := range ips {
				if slices.Equal(ifaceIP, ip) {
					return nil
				}
			}
		}
	}

	return errors.New("not on the same host")
}

// HTTPSAddress returns the core.https_address of the incus server.
func (c *GlobalClient) HTTPSAddress() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return "", ErrDisconnected
	}

	if c.httpsAddress == "" {
		return "", NewError("core.https_address is not set on the server")
	}

	return c.httpsAddress, nil
}

// HasExtension returns whether the server has the given extension.
func (c *GlobalClient) HasExtension(ext string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Contains(c.apiExtensions, ext)
}
