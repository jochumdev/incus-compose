package client

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	logger := slog.New(slog.NewTextHandler(
		os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelDebug - 4}),
	)

	slog.SetDefault(logger)

	code := m.Run()
	os.Exit(code)
}

func skipLocal(t *testing.T) {
	t.Helper()
	if os.Getenv("INCUS_COMPOSE_TEST_LOCAL") != "" {
		t.Skip("Skipping: env INCUS_COMPOSE_TEST_LOCAL is set, run `just test` for this test")
	}
}

// newRandomTestClient creates a GlobalClient, a fresh project-scoped Client,
// and registers t.Cleanup to delete the project on teardown.
func newRandomTestClient(_ context.Context, t *testing.T, prefix string) *Client {
	t.Helper()
	gc, err := NewTestClient(t.Context())
	require.NoError(t, err)
	name := prefix + strings.ToLower(RandString(12))

	c, err := createProjectClient(gc, name)
	require.NoError(t, err)

	err = c.Open()
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = c.Done()
		_ = gc.DeleteProject(name, true)
	})
	return c
}

// createProjectClient creates a project-scoped client with logging hooks.
func createProjectClient(gc *GlobalClient, name string) (*Client, error) {
	if name == "" {
		name = "test-" + strings.ToLower(RandString(12))
	}

	_ = gc.DeleteProject(name, true)

	c, err := gc.createProject(name, nil)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// ----------------------------------------------------------------------------
// Unit Tests
// ----------------------------------------------------------------------------

func TestClientDescriptionFormat(t *testing.T) {
	t.Parallel()
	client := NewOfflineClient(t.Context(), "my_project")

	assert.Equal(t, "incus-client: %s", client.globalClient.config.DescriptionFormat)
	assert.Equal(t, "incus-client: my_project:%s", client.Config().DescriptionFormat)
	assert.Equal(t, "incus-client: my_project:web", fmt.Sprintf(client.Config().DescriptionFormat, "web"))
}

func TestClientCustomDescriptionFormat(t *testing.T) {
	t.Parallel()
	gc := New(t.Context(), ClientDescriptionFormat("managed-by-test: %s"))

	config := gc.config
	config.DescriptionFormat = fmt.Sprintf(config.DescriptionFormat, "demo") + ":%s"

	require.Equal(t, "managed-by-test: demo:web", fmt.Sprintf(config.DescriptionFormat, "web"))
}

func TestSanitizeProjectName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple name",
			input:    "myproject",
			expected: "myproject",
		},
		{
			name:     "underscore replacement",
			input:    "my_project",
			expected: "my-project",
		},
		{
			name:     "uppercase to lowercase",
			input:    "MyProject",
			expected: "myproject",
		},
		{
			name:     "special characters",
			input:    "my project!",
			expected: "my-project",
		},
		{
			name:     "quotes removed",
			input:    `my"project"`,
			expected: "myproject",
		},
		{
			name:     "multiple special chars",
			input:    "my__project--name",
			expected: "my--project-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, SanitizeProjectName(tt.input))
		})
	}
}

// ----------------------------------------------------------------------------
// Integration Tests
// ----------------------------------------------------------------------------

func TestClientConnection_IsConnected(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)
	require.True(t, gc.IsConnected())
}

func TestClientProject_GlobalClientKeepsDefaultProfile(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)

	gInfo, err := gc.incus.GetConnectionInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "default", gInfo.Project)

	name := "client-gcdef-" + strings.ToLower(RandString(8))
	t.Cleanup(func() { _ = gc.DeleteProject(name, true) })

	project, err := gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)
	require.NotNil(t, project)

	gConn, err := project.GlobalConnection()
	require.NoError(t, err)

	gInfo, err = gConn.GetConnectionInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "default", gInfo.Project)
}

func TestClientProject_ImageCacheIsInCacheProfile(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)

	if gc.imageCache == nil {
		t.Skipf("Skipping INCUS_COMPOSE_IMAGE_CACHE is empty")
	}

	gInfo, err := gc.imageCache.incus.GetConnectionInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "incus-compose-tests-cache", gInfo.Project)
}

func TestClientProject_EnsureWithCreate(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)
	name := "client-ensure-" + strings.ToLower(RandString(8))
	t.Cleanup(func() { _ = gc.DeleteProject(name, true) })

	project, err := gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)
	require.NotNil(t, project)
}

func TestClientProject_EnsureWithoutCreate_Fails(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)

	_, err = gc.EnsureProject("surely-does-not-exist-12345")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClientProject_NameIsPreserved(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)
	name := "client-name-" + strings.ToLower(RandString(8))
	t.Cleanup(func() { _ = gc.DeleteProject(name, true) })

	project, err := gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)
	require.Equal(t, name, project.Project())
}

func TestClientProject_NameIsSanitized(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)

	name := "Test Project_123"
	project, err := gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)
	t.Cleanup(func() { _ = gc.DeleteProject(name, true) })

	require.Equal(t, name, project.Project())
	require.Equal(t, "test-project-123", project.IncusProject())
}

func TestClientProject_EnsureIdempotent(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)
	name := "client-idem-" + strings.ToLower(RandString(8))
	t.Cleanup(func() { _ = gc.DeleteProject(name, true) })

	project1, err := gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)
	project2, err := gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)
	require.Same(t, project1, project2)
}

func TestClientProject_DeleteSucceeds(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)
	name := "client-del-" + strings.ToLower(RandString(8))

	_, err = gc.EnsureProject(name, EnsureProjectWithCreate())
	require.NoError(t, err)

	require.NoError(t, gc.DeleteProject(name, true))
}

func TestClientProject_DeleteNonExistent_NoError(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	gc, err := NewTestClient(ctx)
	require.NoError(t, err)

	_ = gc.DeleteProject("never-existed-xyz987", true)
}
