// Package project loads Docker Compose files and configures client resources.
//
// This package is not a passive loader. It actively drives resource creation
// by calling into the client package. The typical flow is:
//
//  1. CLI creates a client.Client
//  2. project.Load() parses the compose file
//  3. project.ToStack() configures resources on the client and builds a Stack
//  4. CLI runs the Stack to execute operations on Incus
package project

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/errdefs"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/compose-spec/compose-go/v2/utils"
	"github.com/dominikbraun/graph"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

// ErrNoComposeFile says there was no compose file to load, so a caller that can
// work without one can tell that apart from a broken one.
var ErrNoComposeFile = errors.New("no compose.yaml found, either change to a directory with a `compose.yaml` or use `--file`")

// LoadOptions holds configuration for Load and LoadModel.
type LoadOptions struct {
	// Project name
	Name string

	// Compose configuration file paths
	Files []string

	// Working directory (if empty, uses current directory or path of first file)
	WorkingDir string

	// Alternative environment files
	EnvFiles []string

	// Profiles to enable
	Profiles []string

	// OsEnv includes OS environment variables in project env (default: false for portability)
	OsEnv bool

	// InstanceMarks is config stamped on every instance the project creates.
	InstanceMarks map[string]string

	// ProjectMarks is config stamped on the Incus project itself.
	ProjectMarks map[string]string
}

// LoadOption is a functional option for LoadProject.
type LoadOption func(*LoadOptions)

// LoadName sets the project name.
func LoadName(name string) LoadOption {
	return func(o *LoadOptions) {
		o.Name = name
	}
}

// LoadFiles sets the compose configuration file paths.
func LoadFiles(files []string) LoadOption {
	return func(o *LoadOptions) {
		o.Files = files
	}
}

// LoadWorkingDir sets the working directory.
func LoadWorkingDir(dir string) LoadOption {
	return func(o *LoadOptions) {
		o.WorkingDir = dir
	}
}

// LoadEnvFiles sets alternative environment files.
func LoadEnvFiles(files []string) LoadOption {
	return func(o *LoadOptions) {
		o.EnvFiles = files
	}
}

// LoadProfiles sets the profiles to enable.
func LoadProfiles(profiles []string) LoadOption {
	return func(o *LoadOptions) {
		o.Profiles = profiles
	}
}

// LoadInstanceMarks stamps config on every instance the project creates.
func LoadInstanceMarks(marks map[string]string) LoadOption {
	return func(o *LoadOptions) {
		o.InstanceMarks = marks
	}
}

// LoadProjectMarks stamps config on the Incus project itself.
func LoadProjectMarks(marks map[string]string) LoadOption {
	return func(o *LoadOptions) {
		o.ProjectMarks = marks
	}
}

// LoadOsEnv includes OS environment variables in the project environment.
// Without this, only .env files and compose file env vars are used (more portable).
func LoadOsEnv() LoadOption {
	return func(o *LoadOptions) {
		o.OsEnv = true
	}
}

// NewLoadOptions creates LoadOptions with the given options applied.
func NewLoadOptions(opts ...LoadOption) LoadOptions {
	res := LoadOptions{
		Files:    []string{},
		Profiles: []string{},
	}

	for _, o := range opts {
		o(&res)
	}

	return res
}

// LoadModel loads the raw compose model without interpolation.
// Useful for extracting variable definitions before resolution.
func LoadModel(ctx context.Context, opts ...LoadOption) (map[string]any, error) {
	options := NewLoadOptions(opts...)

	cliOptions, err := buildProjectOptions(options, cli.WithInterpolation(false))
	if err != nil {
		return nil, err
	}

	model, err := cliOptions.LoadModel(ctx)
	if errors.Is(err, errdefs.ErrNotFound) {
		return nil, ErrNoComposeFile
	}

	return model, err
}

// Project wraps a Docker Compose project with Incus client integration.
type Project struct {
	*types.Project `yaml:",inline"`

	ClientConfig XICProject `json:"-" yaml:"-"`

	// InstanceMarks is stamped on every instance; see LoadInstanceMarks.
	InstanceMarks map[string]string `json:"-" yaml:"-"`
}

// XICProject is the typed view of the top-level x-incus-compose extension.
type XICProject struct {
	Backup  client.BackupConfig `mapstructure:"backup"`
	Healthd XICHealthd
	XIncus  map[string]string
}

// XICHealthd is the x-incus-compose.healthd block.
type XICHealthd struct {
	Incus    string
	Network  string
	External bool

	// Scope is HealthScopeProject or HealthScopeGlobal; empty means unset.
	Scope string

	// Workers and RestartWorkers size the daemon's pools; 0 means unset.
	Workers        int
	RestartWorkers int

	// XIncus is Incus instance config for the sidecar, e.g. limits.*.
	XIncus map[string]string
}

// New creates a new Project.
func New() *Project {
	return &Project{ClientConfig: XICProject{
		Healthd: XICHealthd{XIncus: map[string]string{}},
		XIncus:  map[string]string{},
	}}
}

// Load loads a compose project with full interpolation and validation.
func (p *Project) Load(ctx context.Context, opts ...LoadOption) (*Project, error) {
	options := NewLoadOptions(opts...)

	cliOptions, err := buildProjectOptions(options)
	if err != nil {
		return p, err
	}

	cp, err := cliOptions.LoadProject(ctx)
	if errors.Is(err, errdefs.ErrNotFound) {
		return p, ErrNoComposeFile
	}

	if err != nil {
		return p, err
	}

	p.Project = cp

	if p.Extensions != nil {
		var ext struct {
			Backup  client.BackupConfig `mapstructure:"backup"`
			Healthd struct {
				Incus          string         `mapstructure:"incus"`
				Network        string         `mapstructure:"network"`
				External       bool           `mapstructure:"external"`
				Scope          string         `mapstructure:"scope"`
				Workers        int            `mapstructure:"workers"`
				RestartWorkers int            `mapstructure:"restart-workers"`
				XIncus         map[string]any `mapstructure:"x-incus"`
			} `mapstructure:"healthd"`
		}
		ok, err := p.Extensions.Get("x-incus-compose", &ext)
		if err != nil {
			return nil, err
		}
		if ok {
			switch ext.Healthd.Scope {
			case "", shared.HealthScopeProject, shared.HealthScopeGlobal:
			default:
				return nil, fmt.Errorf(
					"x-incus-compose.healthd.scope: %q must be %q or %q",
					ext.Healthd.Scope, shared.HealthScopeProject, shared.HealthScopeGlobal)
			}

			p.ClientConfig.Healthd.Incus = ext.Healthd.Incus
			p.ClientConfig.Healthd.Network = ext.Healthd.Network
			p.ClientConfig.Healthd.External = ext.Healthd.External
			p.ClientConfig.Healthd.Scope = ext.Healthd.Scope
			p.ClientConfig.Healthd.Workers = ext.Healthd.Workers
			p.ClientConfig.Healthd.RestartWorkers = ext.Healthd.RestartWorkers
			p.ClientConfig.Backup = ext.Backup

			for k, v := range ext.Healthd.XIncus {
				p.ClientConfig.Healthd.XIncus[k] = fmt.Sprint(v)
			}
		}

		var raw map[string]any
		ok, err = p.Extensions.Get("x-incus", &raw)
		if ok || err == nil {
			if p.ClientConfig.XIncus == nil {
				p.ClientConfig.XIncus = map[string]string{}
			}

			for k, v := range raw {
				p.ClientConfig.XIncus[k] = fmt.Sprint(v)
			}
		}
	}

	// Last, so x-incus cannot drop them.
	p.InstanceMarks = options.InstanceMarks
	maps.Copy(p.ClientConfig.XIncus, options.ProjectMarks)

	return p, nil
}

// InstanceNames returns the Incus instance names for all services.
func (p *Project) InstanceNames() []string {
	var names []string
	for _, svcName := range p.ServiceNames() {
		service, err := p.GetService(svcName)
		if err != nil {
			continue
		}

		replicas := 1
		if service.Deploy != nil && service.Deploy.Replicas != nil {
			replicas = *service.Deploy.Replicas
		}

		for i := 1; i <= replicas; i++ {
			names = append(names, instanceName(service, i, replicas))
		}
	}

	return names
}

// ResourcesOptions configures how services are converted to stack operations.
type ResourcesOptions struct {
	Full  bool
	Scale map[string]int // service name -> replica count override
	marks map[string]string
}

// ResourcesOption is a functional option for ToStack.
type ResourcesOption func(o *ResourcesOptions)

// ResourcesFull fetches complete instance state including image alias and full instance details.
func ResourcesFull() ResourcesOption {
	return func(o *ResourcesOptions) {
		o.Full = true
	}
}

// ResourcesScale sets replica count overrides for services.
func ResourcesScale(scale map[string]int) ResourcesOption {
	return func(o *ResourcesOptions) {
		o.Scale = scale
	}
}

// ServiceOrder returns the services in dependency order.
func (p *Project) ServiceOrder(reverse bool) ([]string, error) {
	return ServiceGraph(p.Services, reverse)
}

// Resources converts the compose project services to client resources.
func (p *Project) Resources(c *client.Client, opts ...ResourcesOption) (map[string][]client.Resource, error) {
	options := &ResourcesOptions{}
	for _, o := range opts {
		o(options)
	}

	options.marks = p.InstanceMarks

	result := map[string][]client.Resource{}

	var errs error

	// Configure instances
	for _, service := range p.Services {
		// Determine the desired count: CLI --scale > deploy.replicas > 1.
		// A plain `up` reconciles to deploy.replicas in both directions, matching
		// `docker compose up`: a manual --scale applies only to that invocation,
		// and the next plain `up` restores replicas (scaling up or down).
		desired := 1
		if s, ok := options.Scale[service.Name]; ok {
			desired = s
		} else if service.Deploy != nil && service.Deploy.Replicas != nil {
			desired = *service.Deploy.Replicas
		}

		// Discover existing instances above the desired count so they can be
		// reconciled away (highest index first) during Ensure.
		scale := desired
		for {
			instanceName := fmt.Sprintf("%s-%d", service.Name, scale+1)
			if ok, err := c.InstanceExists(instanceName); !ok || err != nil {
				break
			}

			scale++
		}

		instances := []*client.Instance{}
		for i := 1; i <= scale; i++ {
			instance, instanceResources, err := serviceToInstance(c, p.Project, service.Name, options, i, scale)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}

			result[service.Name] = append(result[service.Name], instance)
			result[service.Name] = append(result[service.Name], instanceResources...)

			instances = append(instances, instance)
		}

		// Reconcile down: instances beyond the desired count are marked for
		// deletion (highest index first) and torn down during Ensure.
		if len(instances) > desired {
			slices.Reverse(instances)

			for idx := range len(instances) - desired {
				instances[idx].MarkDelete()
			}
		}
	}

	if errs != nil {
		return nil, errs
	}

	return result, nil
}

// ServiceGraph returns services in dependency order using topological sort.
// If reverse is true, returns reverse order (useful for shutdown).
func ServiceGraph(serviceConfigs types.Services, reverse bool) ([]string, error) {
	g := graph.New(graph.StringHash, graph.Directed(), graph.PreventCycles())

	// Add vertices
	for s := range maps.Values(serviceConfigs) {
		_ = g.AddVertex(s.Name)
	}

	// Add edges for dependencies that are in scope.
	for s := range maps.Values(serviceConfigs) {
		for dep := range s.DependsOn {
			if _, ok := serviceConfigs[dep]; !ok {
				continue
			}
			// Edge from dependency to dependent (dep must start before n)
			err := g.AddEdge(dep, s.Name)
			if err != nil && !errors.Is(err, graph.ErrEdgeAlreadyExists) {
				return nil, fmt.Errorf("adding dependency edge %s -> %s: %w", dep, s.Name, err)
			}
		}
	}

	order, err := graph.TopologicalSort(g)
	if err != nil {
		return nil, fmt.Errorf("topological sort: %w", err)
	}

	if reverse {
		slices.Reverse(order)
	}

	return order, nil
}

// buildProjectOptions creates cli.ProjectOptions from LoadOptions.
func buildProjectOptions(options LoadOptions, extraOpts ...cli.ProjectOptionsFn) (*cli.ProjectOptions, error) {
	projectOptions := []cli.ProjectOptionsFn{}

	if options.WorkingDir != "" {
		projectOptions = append(projectOptions, cli.WithWorkingDirectory(options.WorkingDir))
	}

	// Include OS env if requested (full docker-compose compatibility)
	if options.OsEnv {
		projectOptions = append(projectOptions, cli.WithOsEnv)
	}

	// Load .env files with OS env available for interpolation but not added to project.
	// cli.WithDefaultConfigPath resolves the working directory from the discovered
	// compose file when options.WorkingDir wasn't set, so the env file options are
	// deliberately re-applied afterwards to resolve relative paths against it.
	//nolint:gocritic // intentional re-application after working dir resolution, not a copy/paste duplicate
	projectOptions = append(projectOptions,
		cli.WithEnvFiles(options.EnvFiles...),
		withDotEnvAndOsEnv, // Custom handler: uses OS env for interpolation only
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
		cli.WithEnvFiles(options.EnvFiles...),
		withDotEnvAndOsEnv,
	)

	if options.Name != "" {
		projectOptions = append(projectOptions, cli.WithName(options.Name))
	}

	if len(options.Profiles) > 0 {
		projectOptions = append(projectOptions, cli.WithProfiles(options.Profiles))
	}

	// Add any extra options (e.g., WithoutEnvironmentResolution)
	projectOptions = append(projectOptions, extraOpts...)

	return cli.NewProjectOptions(
		options.Files,
		projectOptions...,
	)
}

// getOsEnv returns OS environment variables as a map.
func getOsEnv() map[string]string {
	return utils.GetAsEqualsMap(os.Environ())
}

// withDotEnvAndOsEnv loads .env files using OS env for interpolation only.
// OS env variables are NOT added to the project environment unless LoadOsEnv is used.
// This provides portability while allowing .env files to reference system variables.
func withDotEnvAndOsEnv(o *cli.ProjectOptions) error {
	// Get OS env for interpolation
	osEnv := getOsEnv()

	// Merge current project env with OS env for lookups
	lookupEnv := make(map[string]string, len(osEnv)+len(o.Environment))
	maps.Copy(lookupEnv, osEnv)
	maps.Copy(lookupEnv, o.Environment)

	// Parse .env files using combined env for interpolation
	envMap, err := dotenv.GetEnvFromFile(lookupEnv, o.EnvFiles)
	if err != nil {
		return err
	}

	// Only merge the .env results into project environment
	o.Environment.Merge(envMap)
	return nil
}
