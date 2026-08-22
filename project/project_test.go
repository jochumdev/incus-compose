package project

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/shared"
)

// fixturePath returns the path to a test fixture.
func fixturePath(name string) string {
	return filepath.Join("..", "test", "fixtures", name)
}

// TestLoadBasicProject tests basic project loading.
func TestLoadBasicProject(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("simple")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "simple", proj.Name)
	assert.Len(t, proj.Services, 1)

	service, exists := proj.Services["web"]
	assert.True(t, exists, "web service should exist")
	assert.Equal(t, "web", service.Name)
	assert.Equal(t, "docker.io/alpine:edge", service.Image)
}

// TestLoadWordPressStack tests WordPress stack with volumes and dependencies.
func TestLoadWordPressStack(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("wordpress")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "wordpress", proj.Name)
	assert.Len(t, proj.Services, 2)

	// Check db service.
	db, exists := proj.Services["db"]
	assert.True(t, exists, "db service should exist")
	assert.Equal(t, "docker.io/library/mysql:9.5.0", db.Image)
	assert.Contains(t, db.Environment, "MYSQL_ROOT_PASSWORD")

	// Check wordpress service.
	wordpress, exists := proj.Services["wordpress"]
	assert.True(t, exists, "wordpress service should exist")
	assert.Equal(t, "docker.io/library/wordpress:6.9.0-php8.1-apache", wordpress.Image)
	assert.NotEmpty(t, wordpress.DependsOn)

	// Check volumes.
	assert.Len(t, proj.Volumes, 2)
	assert.Contains(t, proj.Volumes, "db_data")
	assert.Contains(t, proj.Volumes, "wordpress_data")
}

// TestLoadPostgresRedisWithEnv tests PostgreSQL + Redis with environment variables.
func TestLoadPostgresRedisWithEnv(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("postgres-redis")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "postgres-redis", proj.Name)
	assert.Len(t, proj.Services, 3)

	// Check postgres service.
	postgres, exists := proj.Services["postgres"]
	assert.True(t, exists, "postgres service should exist")
	assert.Contains(t, postgres.Environment, "POSTGRES_USER")
	assert.Contains(t, postgres.Environment, "POSTGRES_PASSWORD")
	assert.Contains(t, postgres.Environment, "POSTGRES_DB")
	assert.NotNil(t, postgres.HealthCheck)

	// Check redis service.
	redis, exists := proj.Services["redis"]
	assert.True(t, exists, "redis service should exist")
	assert.NotNil(t, redis.HealthCheck)

	// Check api service.
	_, exists = proj.Services["api"]
	assert.True(t, exists, "api service should exist")
}

// TestLoadProxyMultiNetwork tests the proxy fixture with multiple networks.
func TestLoadProxyMultiNetwork(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("proxy")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Len(t, proj.Services, 3)

	// Check networks.
	assert.Len(t, proj.Networks, 1)
	require.Contains(t, proj.Networks, "default")
}

// TestLoadMicroservices tests microservices architecture.
func TestLoadMicroservices(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("microservices")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "microservices", proj.Name)

	// Should have many services.
	assert.GreaterOrEqual(t, len(proj.Services), 8)

	// Check for different database types.
	_, hasAuthDB := proj.Services["auth-db"]
	assert.True(t, hasAuthDB, "Should have auth-db (PostgreSQL)")

	_, hasUserDB := proj.Services["user-db"]
	assert.True(t, hasUserDB, "Should have user-db (MySQL)")

	_, hasOrderDB := proj.Services["order-db"]
	assert.True(t, hasOrderDB, "Should have order-db (MongoDB)")

	// Check multiple networks for isolation.
	assert.GreaterOrEqual(t, len(proj.Networks), 5)
}

// TestLoadWithCustomProjectName tests custom project name.
func TestLoadWithCustomProjectName(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("simple")),
		LoadName("my-custom-project"),
	)

	require.NoError(t, err)
	assert.Equal(t, "my-custom-project", proj.Name)
}

// TestLoadWithCustomFiles tests custom compose file path.
func TestLoadWithCustomFiles(t *testing.T) {
	t.Parallel()

	composePath := filepath.Join(fixturePath("wordpress"), "compose.yaml")

	proj, err := New().Load(
		t.Context(), LoadFiles([]string{composePath}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Len(t, proj.Services, 2)
}

// TestLoadWithDefaultEnvFile tests environment file loading.
func TestLoadWithDefaultEnvFile(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-env")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Environment variables should be loaded from .env.
	app, exists := proj.Services["app"]
	assert.True(t, exists, "app service should exist")

	// Check that env vars from .env file are present.
	env := app.Environment
	assert.Contains(t, env, "DB_HOST")
	assert.Contains(t, env, "DB_PORT")
	assert.Contains(t, env, "DB_NAME")
}

// TestLoadWithCustomEnvFile tests custom environment file.
func TestLoadWithCustomEnvFile(t *testing.T) {
	t.Parallel()

	prodEnvPath := filepath.Join(fixturePath("with-env"), "production.env")

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-env")),
		LoadEnvFiles([]string{prodEnvPath}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Should load production.env instead of .env.
	app, exists := proj.Services["app"]
	assert.True(t, exists, "app service should exist")
	env := app.Environment

	// These values should come from production.env.
	assert.Contains(t, env, "DB_HOST")
	assert.Contains(t, env, "API_KEY")
	assert.Contains(t, env, "API_URL")
}

// TestLoadWithMultipleEnvFiles tests multiple environment files.
func TestLoadWithMultipleEnvFiles(t *testing.T) {
	t.Parallel()

	basePath := fixturePath("with-env")
	prodEnv := filepath.Join(basePath, "production.env")
	stagingEnv := filepath.Join(basePath, "staging.env")

	proj, err := New().Load(
		t.Context(),
		LoadWorkingDir(basePath),
		LoadEnvFiles([]string{prodEnv, stagingEnv}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
}

// TestLoadWithoutProfiles tests profiles - no profiles (default services only).
func TestLoadWithoutProfiles(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("with-profiles")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Only services without profiles should be loaded.
	assert.Len(t, proj.Services, 1)
	_, exists := proj.Services["web"]
	assert.True(t, exists, "web service should exist")
}

// TestLoadWithSingleProfile tests profiles - single profile.
func TestLoadWithSingleProfile(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("with-profiles")),
		LoadProfiles([]string{"dev"}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Should have base service + dev profile services.
	assert.GreaterOrEqual(t, len(proj.Services), 3)

	_, exists := proj.Services["web"]
	assert.True(t, exists, "Should have web service")

	_, exists = proj.Services["webpack"]
	assert.True(t, exists, "Should have webpack from dev profile")

	_, exists = proj.Services["hot-reload"]
	assert.True(t, exists, "Should have hot-reload from dev profile")
}

// TestLoadWithMultipleProfiles tests profiles - multiple profiles.
func TestLoadWithMultipleProfiles(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("with-profiles")),
		LoadProfiles([]string{"dev", "monitoring"}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Base service.
	_, exists := proj.Services["web"]
	assert.True(t, exists, "web service should exist")

	// Dev profile services.
	_, exists = proj.Services["webpack"]
	assert.True(t, exists, "webpack service should exist")

	_, exists = proj.Services["hot-reload"]
	assert.True(t, exists, "hot-reload service should exist")

	// Monitoring profile service.
	_, exists = proj.Services["prometheus"]
	assert.True(t, exists, "prometheus service should exist")

	// Adminer is in both dev and debug, so it should appear with dev profile.
	_, exists = proj.Services["adminer"]
	assert.True(t, exists, "adminer service should exist")
}

// TestLoadDevEnvironment tests development environment with profiles.
func TestLoadDevEnvironment(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("dev-environment")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Without profiles, should only have core services.
	assert.Len(t, proj.Services, 2) // app and db
}

// TestLoadDevEnvironmentWithDebugProfile tests development environment with debug profile.
func TestLoadDevEnvironmentWithDebugProfile(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("dev-environment")),
		LoadProfiles([]string{"debug"}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Should have core + debug services.
	assert.GreaterOrEqual(t, len(proj.Services), 4)

	_, exists := proj.Services["app"]
	assert.True(t, exists, "app service should exist")

	_, exists = proj.Services["db"]
	assert.True(t, exists, "db service should exist")

	_, exists = proj.Services["pgadmin"]
	assert.True(t, exists, "pgadmin service should exist")

	_, exists = proj.Services["mailhog"]
	assert.True(t, exists, "mailhog service should exist")
}

// TestLoadMultipleComposeFiles tests multiple compose files (base + override).
func TestLoadMultipleComposeFiles(t *testing.T) {
	t.Parallel()

	basePath := fixturePath("multiple-files")
	baseFile := filepath.Join(basePath, "compose.yaml")
	overrideFile := filepath.Join(basePath, "compose.override.yaml")

	proj, err := New().Load(
		t.Context(),
		LoadFiles([]string{baseFile, overrideFile}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Should have merged services from both files.
	assert.Len(t, proj.Services, 3) // app, db, adminer

	// Check that override values are applied.
	app, exists := proj.Services["app"]
	assert.True(t, exists, "app service should exist")
	env := app.Environment
	// NODE_ENV should be overridden to development.
	assert.Contains(t, env, "NODE_ENV")
	assert.Contains(t, env, "HOT_RELOAD")
}

// TestLoadMultipleComposeFilesCustomOrder tests multiple compose files with custom order.
func TestLoadMultipleComposeFilesCustomOrder(t *testing.T) {
	t.Parallel()

	basePath := fixturePath("multiple-files")
	baseFile := filepath.Join(basePath, "compose.yaml")
	testFile := filepath.Join(basePath, "compose.test.yaml")

	proj, err := New().Load(
		t.Context(),
		LoadFiles([]string{baseFile, testFile}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)

	// Should have test-runner service from compose.test.yaml.
	_, exists := proj.Services["test-runner"]
	assert.True(t, exists, "test-runner service should exist")
}

// TestLoadWithResourceLimits tests loading a compose file with deploy resource limits.
func TestLoadWithResourceLimits(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-resources")),
	)
	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Len(t, proj.Services, 2)

	limited := proj.Services["limited"]
	require.NotNil(t, limited.Deploy)
	require.NotNil(t, limited.Deploy.Resources.Limits)
	assert.Equal(t, float32(0.5), limited.Deploy.Resources.Limits.NanoCPUs.Value())
	assert.Equal(t, int64(512<<20), int64(limited.Deploy.Resources.Limits.MemoryBytes))

	pinned := proj.Services["pinned"]
	require.NotNil(t, pinned.Deploy)
	require.NotNil(t, pinned.Deploy.Resources.Limits)
	assert.Equal(t, float32(2), pinned.Deploy.Resources.Limits.NanoCPUs.Value())
	assert.Equal(t, int64(1<<30), int64(pinned.Deploy.Resources.Limits.MemoryBytes))
}

// TestLoadInvalidComposeFile tests invalid compose file.
func TestLoadInvalidComposeFile(t *testing.T) {
	t.Parallel()

	_, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("invalid")),
	)

	// Should return an error for invalid compose file.
	assert.Error(t, err)
}

// TestLoadMissingComposeFile tests missing compose file.
func TestLoadMissingComposeFile(t *testing.T) {
	t.Parallel()

	_, err := New().Load(
		t.Context(),
		LoadWorkingDir(fixturePath("nonexistent")),
	)

	assert.Error(t, err)
}

// TestLoadWithAllOptions tests all options combined.
func TestLoadWithAllOptions(t *testing.T) {
	t.Parallel()

	basePath := fixturePath("postgres-redis")
	composePath := filepath.Join(basePath, "compose.yaml")
	envPath := filepath.Join(basePath, ".env")

	proj, err := New().Load(
		t.Context(),
		LoadName("my-combined-project"),
		LoadFiles([]string{composePath}),
		LoadWorkingDir(basePath),
		LoadEnvFiles([]string{envPath}),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "my-combined-project", proj.Name)
	assert.Len(t, proj.Services, 3)
}

// TestLoadWithSecrets tests loading a compose file with secrets.
func TestLoadWithSecrets(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-secrets")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "with-secrets", proj.Name)

	// Check secrets are defined.
	assert.Len(t, proj.Secrets, 3)
	assert.Contains(t, proj.Secrets, "db_password")
	assert.Contains(t, proj.Secrets, "api_key")

	// Check db_password secret (file-based).
	dbSecret := proj.Secrets["db_password"]
	assert.NotEmpty(t, dbSecret.File)

	// Check api_key secret (file-based).
	apiSecret := proj.Secrets["api_key"]
	assert.NotEmpty(t, apiSecret.File)

	// Check service has secrets configured.
	app, exists := proj.Services["app"]
	assert.True(t, exists, "app service should exist")
	assert.Len(t, app.Secrets, 3)

	// Check first secret (simple reference).
	assert.Equal(t, "demo_secret", app.Secrets[0].Source)

	// Check second secret (with custom target).
	assert.Equal(t, "api_key", app.Secrets[2].Source)
	assert.Equal(t, "/app/secrets/api.key", app.Secrets[2].Target)
	assert.Equal(t, "1000", app.Secrets[2].UID)
	assert.Equal(t, "1000", app.Secrets[2].GID)
	assert.NotNil(t, app.Secrets[2].Mode)
}

// TestLoadWithConfigs tests loading a compose file with configs.
func TestLoadWithConfigs(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-configs")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "with-configs", proj.Name)

	// Check configs are defined.
	assert.Len(t, proj.Configs, 4)
	assert.Contains(t, proj.Configs, "app_config")
	assert.Contains(t, proj.Configs, "db_config")
	assert.Contains(t, proj.Configs, "nginx_config")
	assert.Contains(t, proj.Configs, "image_override")

	// Check app_config config (file-based).
	appConfig := proj.Configs["app_config"]
	assert.NotEmpty(t, appConfig.File)

	// Check db_config config (content-based).
	dbConfig := proj.Configs["db_config"]
	assert.NotEmpty(t, dbConfig.Content)

	// Check service has configs configured.
	app, exists := proj.Services["app"]
	assert.True(t, exists, "app service should exist")
	assert.Len(t, app.Configs, 4)

	// Check first config (simple reference).
	assert.Equal(t, "app_config", app.Configs[0].Source)

	// Check third config (with custom target).
	assert.Equal(t, "nginx_config", app.Configs[2].Source)
	assert.Equal(t, "/etc/nginx/nginx.conf", app.Configs[2].Target)
	assert.Equal(t, "101", app.Configs[2].UID)
	assert.Equal(t, "101", app.Configs[2].GID)
	assert.NotNil(t, app.Configs[2].Mode)
}

// TestLoadWithRestartPolicies tests loading a compose file with restart policies.
func TestLoadWithRestartPolicies(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-restart")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "with-restart", proj.Name)
	assert.Len(t, proj.Services, 5)

	// Check restart policies are parsed.
	always := proj.Services["always-restart"]
	assert.Equal(t, "always", always.Restart)

	onFailure := proj.Services["on-failure-restart"]
	assert.Equal(t, "on-failure", onFailure.Restart)

	unlessStopped := proj.Services["unless-stopped-restart"]
	assert.Equal(t, "unless-stopped", unlessStopped.Restart)

	noRestart := proj.Services["no-restart"]
	assert.Equal(t, "no", noRestart.Restart)

	defaultRestart := proj.Services["default-restart"]
	assert.Equal(t, "", defaultRestart.Restart)
}

// TestLoadWithXIncusOptions tests loading a compose file with x-incus extensions.
func TestLoadWithXIncusOptions(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(
		t.Context(), LoadWorkingDir(fixturePath("with-incus-options")),
	)

	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, "with-incus-options", proj.Name)
	assert.Len(t, proj.Services, 2)

	// Verify services loaded
	web, exists := proj.Services["web"]
	assert.True(t, exists, "web service should exist")
	assert.Equal(t, "docker.io/alpine:edge", web.Image)

	database, exists := proj.Services["database"]
	assert.True(t, exists, "database service should exist")
	assert.Equal(t, "docker.io/alpine:edge", database.Image)
}

// TestNewLoadOptionsAppliesOptions verifies the functional options set fields.
func TestNewLoadOptionsAppliesOptions(t *testing.T) {
	t.Parallel()

	options := NewLoadOptions(
		LoadName("custom"),
		LoadFiles([]string{"compose.yaml", "compose.override.yaml"}),
		LoadWorkingDir("/tmp/project"),
		LoadEnvFiles([]string{".env", "prod.env"}),
		LoadProfiles([]string{"dev"}),
		LoadOsEnv(),
	)

	assert.Equal(t, "custom", options.Name)
	assert.Equal(t, []string{"compose.yaml", "compose.override.yaml"}, options.Files)
	assert.Equal(t, "/tmp/project", options.WorkingDir)
	assert.Equal(t, []string{".env", "prod.env"}, options.EnvFiles)
	assert.True(t, options.OsEnv)
}

func TestServiceGraphOrdersDependencies(t *testing.T) {
	t.Parallel()

	services := types.Services{
		"db":  {Name: "db"},
		"api": {Name: "api", DependsOn: types.DependsOnConfig{"db": types.ServiceDependency{}}},
		"web": {Name: "web", DependsOn: types.DependsOnConfig{"api": types.ServiceDependency{}}},
	}

	order, err := ServiceGraph(services, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"db", "api", "web"}, order)

	reverse, err := ServiceGraph(services, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"web", "api", "db"}, reverse)
}

func TestServiceGraphReturnsEdgeErrors(t *testing.T) {
	t.Parallel()

	// Cycles are still an error.
	_, err := ServiceGraph(types.Services{
		"api": {Name: "api", DependsOn: types.DependsOnConfig{"web": types.ServiceDependency{}}},
		"web": {Name: "web", DependsOn: types.DependsOnConfig{"api": types.ServiceDependency{}}},
	}, false)
	assert.Error(t, err)
}

func TestServiceGraphSkipsMissingDependency(t *testing.T) {
	t.Parallel()

	// A dependency not present in the service set is silently skipped (filtered subset case).
	order, err := ServiceGraph(types.Services{
		"api": {Name: "api", DependsOn: types.DependsOnConfig{"db": types.ServiceDependency{}}},
	}, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"api"}, order)
}

func TestProjectConfigExtractsXIncus(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(t.Context(),
		LoadWorkingDir(fixturePath("with-project-options")),
	)
	require.NoError(t, err)

	config := proj.ClientConfig

	assert.Equal(t, map[string]string{
		"limits.cpu":              "4",
		"limits.memory":           "2049MiB",
		"limits.virtual-machines": "0",
	}, config.XIncus)

	assert.True(t, config.Healthd.External)
}

func TestHealthdConfigExtractsXIncusCompose(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(t.Context(), LoadWorkingDir(fixturePath("with-healthd-config")))
	require.NoError(t, err)

	config := proj.ClientConfig.Healthd
	assert.Equal(t, "https://10.0.0.1:8443", config.Incus)
	assert.Equal(t, "healthd:default", config.Network)
	assert.Equal(t, shared.HealthScopeProject, config.Scope)
	assert.Equal(t, 64, config.Workers)
	assert.Equal(t, 8, config.RestartWorkers)
	assert.Equal(t, map[string]string{
		"limits.cpu":    "4",
		"limits.memory": "512MB",
	}, config.XIncus)
}

func TestHealthdConfigRejectsABadScope(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	compose := "x-incus-compose:\n  healthd:\n    scope: worldwide\nservices:\n  web:\n    image: docker.io/alpine:edge\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600))

	_, err := New().Load(t.Context(), LoadWorkingDir(dir))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "x-incus-compose.healthd.scope")
}

func TestHealthdConfigEmptyWithoutExtension(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(t.Context(), LoadWorkingDir(fixturePath("simple")))
	require.NoError(t, err)

	config := proj.ClientConfig.Healthd
	assert.Empty(t, config.Incus)
	assert.Empty(t, config.Network)
	assert.False(t, config.External)
	assert.Empty(t, config.Scope)
	assert.Zero(t, config.Workers)
	assert.Zero(t, config.RestartWorkers)
	assert.Empty(t, config.XIncus)
}

func TestBackupConfigExtractsXIncusCompose(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	compose := "x-incus-compose:\n  backup:\n    pool: mypool\nservices:\n  web:\n    image: docker.io/alpine:edge\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600))

	proj, err := New().Load(t.Context(), LoadWorkingDir(dir))
	require.NoError(t, err)

	assert.Equal(t, "mypool", proj.ClientConfig.Backup.Pool)
}
