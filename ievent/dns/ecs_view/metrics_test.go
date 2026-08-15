package ecs_view

import (
	"context"
	"testing"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetricsAreOptional pins that the counters are the engine's to record or
// not. The collectors are package-level, so the label is this test's own server
// name: another test answering a query must not be able to move these numbers.
func TestMetricsAreOptional(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		metrics bool
		server  string

		want float64
	}{
		{
			name:   "an engine that was not asked for them records nothing",
			server: "metrics-off",
			want:   0,
		},
		{
			name:    "and one that was counts the answer",
			metrics: true,
			server:  "metrics-on",
			want:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Not t.Parallel(): both subtests hit the same package-level
			// zonesGauge, and the sentinel-reset-then-check dance below only
			// holds up if they run one at a time.
			v := engineWith(t)
			v.Metrics = tc.metrics
			v.Server = tc.server

			w := dnstest.NewRecorder(&test.ResponseWriter{})

			handled, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeA, "10.0.1.10"))
			require.NoError(t, err)

			// The answer is the same either way: a metric is never the reason a
			// query was served differently.
			require.True(t, handled)
			require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
			require.Len(t, w.Msg.Answer, 1)

			assert.Equal(t, tc.want, testutil.ToFloat64(requestCount.WithLabelValues(tc.server, "success")))

			// The gauges are the same switch, on the publish path rather than
			// the query one. A sentinel no publish can produce, so the answer is
			// what this Replace did and not what another test left behind.
			zonesGauge.Set(-1)

			snap := testPiece()
			v.Replace(snap)

			want := float64(-1)
			if tc.metrics {
				want = float64(len(snap.ByZone))
			}

			assert.Equal(t, want, testutil.ToFloat64(zonesGauge))
		})
	}
}
