package source

import (
	"errors"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// errIgnored marks an event with nowhere to send it - not a failure.
var errIgnored = errors.New("lifecycle event ignored")

// decodeLifecycle turns one raw Incus event into ours, judging nothing:
// Action stays Incus's own string.
func decodeLifecycle(raw incusapi.Event) (*iutil.Event, error) {
	// Through iclient: incusd fills Name only on instance events, elsewhere
	// it's carried in Source.
	lc, err := iclient.LifecycleEvent(raw)
	if err != nil {
		return nil, err
	}

	if lc.Action == "" {
		return nil, errIgnored
	}

	// raw.Project, not the payload: incusd leaves the payload's Project
	// empty on project and profile events.
	if raw.Project == "" {
		return nil, errIgnored
	}

	// old_name is only present on a rename; a value of the wrong type reads as absent.
	old, _ := lc.Context["old_name"].(string)

	// time.Now, not raw.Timestamp: downstream measures time spent in the
	// chain, not clock skew against the cluster member that sent it.
	return iutil.NewEvent(time.Now(), lc.Action, raw.Project, lc.Name, old), nil
}
