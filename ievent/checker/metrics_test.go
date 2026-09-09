package checker

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/lxc/incus-compose/shared"
)

func TestMetricsRecordState(t *testing.T) {
	instances := map[string]*instance{
		"p/web-1": {status: shared.HealthStatusHealthy},
		"p/web-2": {status: shared.HealthStatusUnhealthy},
		"p/web-3": {status: shared.HealthStatusStarting},
		"p/web-4": {status: shared.HealthStatusStopped},
		"p/web-5": {status: ""},
	}

	updateInstanceMetrics(instances)

	assert.Equal(t, float64(1), testutil.ToFloat64(instancesGauge.WithLabelValues(shared.HealthStatusHealthy)))
	assert.Equal(t, float64(1), testutil.ToFloat64(instancesGauge.WithLabelValues(shared.HealthStatusUnhealthy)))
	assert.Equal(t, float64(1), testutil.ToFloat64(instancesGauge.WithLabelValues(shared.HealthStatusStarting)))
	assert.Equal(t, float64(1), testutil.ToFloat64(instancesGauge.WithLabelValues(shared.HealthStatusStopped)))
	assert.Equal(t, float64(1), testutil.ToFloat64(instancesGauge.WithLabelValues(shared.HealthStatusUnknown)))

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	checksBefore := testutil.ToFloat64(checksTotal.WithLabelValues("passed"))

	// When metrics are disabled:
	s.metrics = false
	res := s.checked(t, inst, nil)
	s.result(res)
	assert.Equal(t, checksBefore, testutil.ToFloat64(checksTotal.WithLabelValues("passed")))

	// When metrics are enabled:
	s.metrics = true
	res = s.checked(t, inst, nil)
	s.result(res)
	assert.Equal(t, checksBefore+1, testutil.ToFloat64(checksTotal.WithLabelValues("passed")))
}
