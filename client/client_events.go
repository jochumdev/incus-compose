package client

import (
	"context"
	"encoding/json"
	"fmt"

	incusApi "github.com/lxc/incus/v7/shared/api"
)

func addEventHook(c *Client) {
	c.AddHookConnected(func(err error) error {
		if err != nil {
			return err
		}

		// Its own context, so Done ends the listener without touching the client's.
		listenCtx, stop := context.WithCancel(c.ctx)

		events, err := c.incus.ListenEvents(listenCtx, c.incusProject, []string{incusApi.EventTypeLifecycle})
		if err != nil {
			stop()
			return fmt.Errorf("opening an event listener: %w", err)
		}

		go func() {
			for event := range events {
				var lc incusApi.EventLifecycle

				err := json.Unmarshal(event.Metadata, &lc)
				if err != nil {
					c.LogDebug("Decoding lifecycle event", "error", err)
					continue
				}

				// Ignore all lifecycle events except started, stopped and updated.
				switch lc.Action {
				case incusApi.EventLifecycleInstanceStarted:
				case incusApi.EventLifecycleInstanceStopped:
				case incusApi.EventLifecycleInstanceUpdated:
				default:
					continue
				}

				if lc.Name == "" {
					continue
				}

				c.rangeResources(func(r Resource) {
					if r.Kind() != KindInstance || r.IncusName() != lc.Name {
						return
					}

					inst, ok := r.(*Instance)
					if !ok {
						return
					}

					// We ignore errors here as on stop/delete this would log an error.
					err = inst.fetch(listenCtx, nil)
					if err == nil {
						c.LogDebug("New lifecycle event", "resource", inst, "action", lc.Action, "health_status", inst.State().IncusInstance.Config[HealthStatusKey])
					}
				})
			}
		}()

		c.AddHookDone(func(err error) error {
			stop()
			return err
		})

		return nil
	})
}
