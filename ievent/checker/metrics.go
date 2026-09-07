package checker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	instancesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: name,
		Name:      "instances",
		Help:      "Number of watched instances by health status.",
	}, []string{"status"})

	checksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: name,
		Name:      "checks_total",
		Help:      "Number of health check executions, by result.",
	}, []string{"result"})

	restartsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: name,
		Name:      "restarts_total",
		Help:      "Number of instance restarts attempted, by result.",
	}, []string{"result"})

	poolRefusals = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: name,
		Name:      "pool_refusals_total",
		Help:      "Number of actions deferred because worker pools were saturated, by pool.",
	}, []string{"pool"})
)
