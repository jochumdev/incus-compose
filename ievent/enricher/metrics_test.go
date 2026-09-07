package enricher

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestMetricsRecordState(t *testing.T) {
	p := &Plugin{state: newState("")}
	p.state.project("alpha")
	p.state.project("beta")

	// Disabled by default: nothing is recorded.
	projectsGauge.Set(0)
	p.updateMetrics()
	assert.Equal(t, float64(0), testutil.ToFloat64(projectsGauge))

	// Enabled: state is recorded.
	p.opts.Metrics = true
	p.updateMetrics()
	assert.Equal(t, float64(2), testutil.ToFloat64(projectsGauge))
	assert.Equal(t, float64(0), testutil.ToFloat64(instancesGauge))
}
