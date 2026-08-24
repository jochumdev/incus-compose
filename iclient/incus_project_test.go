package iclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIncusDeleteProjectForce pins the one thing that separates a delete from a
// force delete. Losing the parameter turns a teardown into "project not empty";
// sending it by mistake takes the instances and volumes with it.
func TestIncusDeleteProjectForce(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		args *DeleteProjectArgs
		want string
	}{
		{"nil is the zero value", nil, "/1.0/projects/other"},
		{"not forced", &DeleteProjectArgs{}, "/1.0/projects/other"},
		{"forced", &DeleteProjectArgs{Force: true}, "/1.0/projects/other?force=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, seen := recordingServer(t, `{}`)

			require.NoError(t, conn.DeleteProject(t.Context(), "other", tt.args))
			require.Equal(t, []string{tt.want}, seen.uris())
			require.Equal(t, "DELETE", seen.all()[0].method)
		})
	}
}
