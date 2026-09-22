package main

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthdConfigDrift(t *testing.T) {
	t.Parallel()

	running := map[string]string{
		"environment.INCUS_COMPOSE_HEALTHD_INCUS":              "https://10.0.0.1:8443",
		"environment.INCUS_COMPOSE_HEALTHD_SERVER_FINGERPRINT": "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
		"environment.INCUS_COMPOSE_HEALTHD_WORKERS":            "32",
		"environment.INCUS_COMPOSE_HEALTHD_RESTART_WORKERS":    "12",
		"limits.cpu": "2",
	}

	incus, err := url.Parse("https://10.0.0.1:8443")
	require.NoError(t, err)

	t.Run("no drift when nothing is asked for", func(t *testing.T) {
		t.Parallel()

		assert.Empty(t, healthdConfigDrift(healthdParams{}, running))
	})

	t.Run("no drift when everything matches", func(t *testing.T) {
		t.Parallel()

		params := healthdParams{
			incus:             incus,
			serverFingerprint: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
			workers:           32,
			restartWorkers:    12,
			xIncus:            map[string]string{"limits.cpu": "2"},
		}

		assert.Empty(t, healthdConfigDrift(params, running))
	})

	t.Run("every differing key is named", func(t *testing.T) {
		t.Parallel()

		other, err := url.Parse("https://10.0.0.2:8443")
		require.NoError(t, err)

		params := healthdParams{
			incus:             other,
			serverFingerprint: "different",
			workers:           64,
			restartWorkers:    12,
			xIncus:            map[string]string{"limits.cpu": "4", "limits.memory": "512MB"},
		}

		assert.Equal(t,
			[]string{"incus", "server-fingerprint", "workers", "x-incus.limits.cpu", "x-incus.limits.memory"},
			healthdConfigDrift(params, running))
	})
}
