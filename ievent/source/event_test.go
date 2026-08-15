package source

import (
	"encoding/json"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
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
			// The decoder reports what Incus said and judges nothing: an action
			// nobody has implemented comes through untouched, because what an
			// action means belongs to the plugin that asked for it.
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
			// The rule that costs an afternoon to rediscover:
			// EventLifecycle.Project exists but incusd leaves it empty, so only
			// the envelope says which project an event belongs to.
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
			// InstanceAction is the only one of incusd's lifecycle builders
			// that fills Name, so a network event names nothing in its metadata
			// and carries it in Source alone. Decoded without that, every
			// network event names nothing and the enricher patches no wire.
			name:    "a network event is named from its source URL",
			project: "default",
			lc: incusapi.EventLifecycle{
				Action: incusapi.EventLifecycleNetworkCreated,
				// api.URL.Project omits the query for the default project, so a
				// iutil bridge's source carries the name and nothing else.
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
			assert.Equal(t, tc.wantProject, ev.Project())
			assert.Equal(t, tc.wantName, ev.Name())
			assert.Equal(t, tc.wantOldName, ev.OldName())

			// Decoded events start clean and dated. Nothing has finished with
			// them, and At is what log and trace measure the walk from.
			assert.Equal(t, iutil.StateOk, ev.State())
			assert.False(t, ev.At().IsZero())
		})
	}
}

// TestDecodeLifecycleIgnores covers what there is nowhere to send. None of it
// is malformed, so none of it is an error worth reporting as one.
func TestDecodeLifecycleIgnores(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		project string
		lc      incusapi.EventLifecycle
	}{
		{
			// Server-scoped events - certificates, storage pools, warnings -
			// carry no project, and everything downstream is keyed by one.
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
// as an error, and it has to be told apart from the ignores: route logs this
// one and stays quiet about those.
func TestDecodeLifecycleRejectsBadMetadata(t *testing.T) {
	t.Parallel()

	ev, err := decodeLifecycle(incusapi.Event{
		Type: incusapi.EventTypeLifecycle, Project: "shop", Metadata: []byte("{"),
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, errIgnored)
	assert.Nil(t, ev)
}
