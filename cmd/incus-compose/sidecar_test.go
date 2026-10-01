package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSidecarQueryEndpoints_Empty(t *testing.T) {
	t.Parallel()

	resps, err := sidecarQueryEndpoints(t.Context(), "inst", "proj", 9153, nil)
	assert.NoError(t, err)
	assert.Nil(t, resps)
}

func TestRenderSidecarStatus_Text(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	err := renderSidecarStatus(&buf, "text", "ready", "10.0.0.1", "fd00::1", "metric_a 10\n")
	assert.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "Status: ready\n")
	assert.Contains(t, out, "IPv4: 10.0.0.1\n")
	assert.Contains(t, out, "IPv6: fd00::1\n")
	assert.Contains(t, out, "Metrics:\nmetric_a 10\n")

	var bufNoMetrics strings.Builder
	err = renderSidecarStatus(&bufNoMetrics, "text", "stopped", "", "", "")
	assert.NoError(t, err)
	outNoMetrics := bufNoMetrics.String()
	assert.Contains(t, outNoMetrics, "Status: stopped\n")
	assert.Contains(t, outNoMetrics, "IPv4:\n")
	assert.Contains(t, outNoMetrics, "IPv6:\n")
	assert.NotContains(t, outNoMetrics, "Metrics:")
}

func TestRenderSidecarStatus_JSON(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	err := renderSidecarStatus(&buf, "json", "ready", "10.0.0.1", "fd00::1", "metric_a 10\n")
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), `"status": "ready"`)
	assert.Contains(t, buf.String(), `"ipv4": "10.0.0.1"`)
	assert.Contains(t, buf.String(), `"ipv6": "fd00::1"`)
	assert.Contains(t, buf.String(), `"metrics": "metric_a 10\n"`)

	var bufNoMetrics strings.Builder
	err = renderSidecarStatus(&bufNoMetrics, "json", "stopped", "", "", "")
	assert.NoError(t, err)
	assert.Contains(t, bufNoMetrics.String(), `"status": "stopped"`)
	assert.NotContains(t, bufNoMetrics.String(), `"metrics"`)
}

func TestRenderSidecarStatus_InvalidFormat(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	err := renderSidecarStatus(&buf, "yaml", "ready", "10.0.0.1", "fd00::1", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid format: yaml")
}
