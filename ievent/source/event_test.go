package source

import (
	"encoding/json"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawEvent builds a lifecycle event the way incusd sends one: the project on
// the envelope, everything else in the metadata.
func rawEvent(t *testing.T, project string, lc incusapi.EventLifecycle) incusapi.Event {
	t.Helper()

	meta, err := json.Marshal(lc)
	require.NoError(t, err)

	return incusapi.Event{Type: incusapi.EventTypeLifecycle, Project: project, Metadata: meta}
}

func TestDecodeLifecycle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		project string
		lc      incusapi.EventLifecycle

		wantAction  string
		wantProject string
		wantName    string
		wantOldName string
	}{
		{
			// What an action means is the plugin's call, not the decoder's.
			name:    "an action nobody implemented comes through untouched",
			project: "shop",
			lc: incusapi.EventLifecycle{
				Action: "something-nobody-implemented",
				Name:   "web",
			},
			wantAction:  "something-nobody-implemented",
			wantProject: "shop",
			wantName:    "web",
		},
		{
			// EventLifecycle.Project exists but incusd never fills it.
			name:    "the project comes off the envelope, not the payload",
			project: "shop",
			lc: incusapi.EventLifecycle{
				Action:  incusapi.EventLifecycleInstanceStarted,
				Name:    "web",
				Project: "",
			},
			wantAction:  incusapi.EventLifecycleInstanceStarted,
			wantProject: "shop",
			wantName:    "web",
		},
		{
			// Only InstanceAction fills Name; a network event carries its
			// name in Source instead.
			name:    "a network event is named from its source URL",
			project: "default",
			lc: incusapi.EventLifecycle{
				Action: incusapi.EventLifecycleNetworkCreated,
				// The default project's URL has no query string, so Source
				// is just the path.
				Source: "/1.0/networks/ic-q2mjfn37xz",
			},
			wantAction:  incusapi.EventLifecycleNetworkCreated,
			wantProject: "default",
			wantName:    "ic-q2mjfn37xz",
		},
		{
			name:    "a rename carries the old name out of the context",
			project: "shop",
			lc: incusapi.EventLifecycle{
				Action:  incusapi.EventLifecycleInstanceRenamed,
				Name:    "web2",
				Context: map[string]any{"old_name": "web"},
			},
			wantAction:  incusapi.EventLifecycleInstanceRenamed,
			wantProject: "shop",
			wantName:    "web2",
			wantOldName: "web",
		},
		{
			name:    "an old name of the wrong type reads as absent",
			project: "shop",
			lc: incusapi.EventLifecycle{
				Action:  incusapi.EventLifecycleInstanceRenamed,
				Name:    "web2",
				Context: map[string]any{"old_name": 7},
			},
			wantAction:  incusapi.EventLifecycleInstanceRenamed,
			wantProject: "shop",
			wantName:    "web2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ev, err := decodeLifecycle(rawEvent(t, tc.project, tc.lc))
			require.NoError(t, err)

			assert.Equal(t, tc.wantAction, ev.Action())
			assert.Equal(t, tc.wantProject, ev.ProjectName())
			assert.Equal(t, tc.wantName, ev.Name())
			assert.Equal(t, tc.wantOldName, ev.OldName())

			// Decoded events start clean and dated; At is what log/trace
			// measure the walk from.
			assert.Nil(t, ev.Err())
			assert.False(t, ev.At().IsZero())
		})
	}
}

// TestDecodeLifecycleIgnores covers events with nowhere to send; none of them
// are malformed enough to be an error.
func TestDecodeLifecycleIgnores(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		project string
		lc      incusapi.EventLifecycle
	}{
		{
			// Server-scoped events - certificates, storage pools, warnings - carry no project.
			name:    "an event naming no project",
			project: "",
			lc: incusapi.EventLifecycle{
				Action: incusapi.EventLifecycleInstanceStarted,
				Name:   "web",
			},
		},
		{
			name:    "an event naming no action",
			project: "shop",
			lc:      incusapi.EventLifecycle{Name: "web"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ev, err := decodeLifecycle(rawEvent(t, tc.project, tc.lc))
			require.ErrorIs(t, err, errIgnored)
			assert.Nil(t, ev)
		})
	}
}

// TestDecodeLifecycleRejectsBadMetadata is the one failure the decoder reports
// as an error; route logs this one and stays quiet about the ignores.
func TestDecodeLifecycleRejectsBadMetadata(t *testing.T) {
	t.Parallel()

	ev, err := decodeLifecycle(incusapi.Event{
		Type: incusapi.EventTypeLifecycle, Project: "shop", Metadata: []byte("{"),
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, errIgnored)
	assert.Nil(t, ev)
}
