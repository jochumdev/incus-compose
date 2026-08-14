package ecs_view

import (
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Name is the name of the plugin, and the subsystem its metrics publish under.
const Name = "ecs_view"

var log = clog.NewWithPlugin(Name)

var (
	requestCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: Name,
		Name:      "requests_total",
		Help:      "Counter of DNS requests handled, by result.",
	}, []string{"server", "result"})

	// deniedCount is the fail-closed path.
	deniedCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: Name,
		Name:      "unidentified_clients_total",
		Help:      "Counter of requests denied because the querier could not be placed on any known network.",
	}, []string{"server"})

	zonesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: Name,
		Name:      "zones",
		Help:      "Number of zones served from the store.",
	})

	addressesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: Name,
		Name:      "addresses",
		Help:      "Number of addresses indexed for client identification.",
	})
)
