package enricher

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	projectsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Subsystem: name,
		Name:      "projects",
		Help:      "Number of watched projects currently held in state.",
	})

	instancesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Subsystem: name,
		Name:      "instances",
		Help:      "Number of instances currently held in state.",
	})

	sweepsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: name,
		Name:      "sweeps_total",
		Help:      "Number of discovery sweeps completed, by result.",
	}, []string{"result"})
)
