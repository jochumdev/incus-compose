package source

import (
	"errors"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// errIgnored marks an event there is nowhere to send. Not a failure: the stream
// carries plenty that belongs to no project at all.
var errIgnored = errors.New("lifecycle event ignored")

// decodeLifecycle turns one raw Incus event into ours. It parses and judges
// nothing, which is why Action stays Incus's own string.
func decodeLifecycle(raw incusapi.Event) (*iutil.Event, error) {
	// Through iclient, because incusd fills Name on instance events alone and
	// leaves every other kind carrying it in Source.
	lc, err := iclient.LifecycleEvent(raw)
	if err != nil {
		return nil, err
	}

	if lc.Action == "" {
		return nil, errIgnored
	}

	// The project comes off the envelope rather than the payload, which incusd
	// leaves empty on project and profile events. Everything downstream is keyed
	// by project, so an event naming none has nowhere to go.
	if raw.Project == "" {
		return nil, errIgnored
	}

	// Incus carries the pre-rename name in the context, and only on a rename. A
	// value of the wrong type reads as absent, which the consumer already handles.
	old, _ := lc.Context["old_name"].(string)

	// time.Now rather than raw.Timestamp, so what log measures is the time spent
	// here rather than the clock difference to whichever member sent it.
	return iutil.NewEvent(time.Now(), lc.Action, raw.Project, lc.Name, old), nil
}
