package telemetry

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

// Metric and label names follow ADR-019 D3/D5: exactly four metrics in the A1
// foundation, bounded label domains only, no request/trace/project/user/
// virtual-key identifiers anywhere.
const (
	metricRequestsTotal   = "gateway_requests_total"
	metricRequestDuration = "gateway_request_duration_seconds"
	metricActiveRequests  = "gateway_active_requests"
	metricActiveStreams   = "gateway_active_streams"

	labelProvider    = "provider"
	labelModelFamily = "model_family"
	labelStatus      = "status"
	labelStream      = "stream"

	// statusSucceeded/statusFailed mirror the durable terminal row status and
	// are the only two allowed values of the status label (ADR-019 D3/D5).
	statusSucceeded = "succeeded"
	statusFailed    = "failed"

	// modelFamilyOther is the bounded fallback for any provider/model that is
	// not covered by the explicit family mapping (ADR-019 D5); a raw model
	// string is never a label value.
	modelFamilyOther = "other"

	streamTrue  = "true"
	streamFalse = "false"

	helpRequestsTotal   = "Gateway requests whose durable terminal state was determined (succeeded or failed). Durable lifecycle metric: pre-auth, malformed, unsupported, and admission-rejected requests are never counted; a finalize persistence failure does not change the count (ADR-019 D3)."
	helpRequestDuration = "End-to-end gateway request duration in seconds from handler ingress to the durable terminal state determination, including the full stream lifecycle for streaming requests (ADR-019 D3/D5)."
	helpActiveRequests  = "Chat operations (stream and non-stream) that passed all admission controls and are still executing (ADR-019 D4)."
	helpActiveStreams   = "Streaming chat operations that passed all admission controls and are still executing, by provider (ADR-019 D4)."
)

// requestDurationBuckets span the practical gateway range (sub-millisecond to
// the stream phase budget order of magnitude). A deliberate, documented choice
// for the foundation; buckets are revisited when the complete §24.1 metric set
// and Week 11 benchmarks land. Values above the largest bucket fall into +Inf.
var requestDurationBuckets = []float64{
	0.001, 0.004, 0.016, 0.064, 0.256,
	1.024, 4.096, 16.384, 65.536, 262.144,
}

// Metrics owns the four A1 gateway metrics on an app-owned Prometheus
// registry (ADR-019 D2/D3). There is no package-global state: every Metrics
// value creates its own *prometheus.Registry, so tests and multiple Service
// instances never collide on registration.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	activeRequests  prometheus.Gauge
	activeStreams   *prometheus.GaugeVec
}

// NewMetrics constructs a Metrics value with its own registry. It returns an
// error only if one of the four collectors fails to register on the fresh
// registry (a programming error; the names are fixed constants).
func NewMetrics() (*Metrics, error) {
	registry := prometheus.NewRegistry()
	metrics := &Metrics{
		registry: registry,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricRequestsTotal,
			Help: helpRequestsTotal,
		}, []string{labelProvider, labelModelFamily, labelStatus, labelStream}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricRequestDuration,
			Help:    helpRequestDuration,
			Buckets: requestDurationBuckets,
		}, []string{labelProvider, labelStream}),
		activeRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: metricActiveRequests,
			Help: helpActiveRequests,
		}),
		activeStreams: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricActiveStreams,
			Help: helpActiveStreams,
		}, []string{labelProvider}),
	}
	collectors := []prometheus.Collector{
		metrics.requestsTotal,
		metrics.requestDuration,
		metrics.activeRequests,
		metrics.activeStreams,
	}
	for _, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}
	return metrics, nil
}

// Registry exposes the app-owned registry. The Operations Plane serves it via
// Handler; tests gather from it directly.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// Handler returns the Prometheus scrape handler for the Operations Plane
// (ADR-019 D2). Data and Control Plane muxes never mount it.
func (m *Metrics) Handler() http.Handler {
	if m == nil || m.registry == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveRequest records the durable lifecycle counter and the request
// duration histogram once a request's business terminal state is determined
// (ADR-019 D3 counting semantics): the call happens before the durable
// finalize write, so a persistence failure never changes the request count.
// status must be "succeeded" or "failed"; any other value is a programmer
// error and is ignored rather than inventing a label value.
func (m *Metrics) ObserveRequest(providerName, model string, stream bool, status string, duration time.Duration) {
	if m == nil || !validProvider(providerName) {
		return
	}
	if status != statusSucceeded && status != statusFailed {
		return
	}
	streamValue := streamLabel(stream)
	// WithLabelValues is positional and must match the declared order
	// provider, model_family, status, stream.
	m.requestsTotal.WithLabelValues(providerName, modelFamily(providerName, model), status, streamValue).Inc()
	if duration < 0 {
		duration = 0
	}
	m.requestDuration.WithLabelValues(providerName, streamValue).Observe(duration.Seconds())
}

// TrackInFlight increments the active request gauge (and, for streaming
// requests, the active streams gauge labeled by provider) and returns a
// release function that decrements both exactly once. Callers defer the
// release in the same request lifecycle as the admission slot release, after
// the durable terminal state has been determined (ADR-019 D4).
func (m *Metrics) TrackInFlight(providerName string, stream bool) func() {
	if m == nil || !validProvider(providerName) {
		return func() {}
	}
	m.activeRequests.Inc()
	if stream {
		m.activeStreams.WithLabelValues(providerName).Inc()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.activeRequests.Dec()
			if stream {
				m.activeStreams.WithLabelValues(providerName).Dec()
			}
		})
	}
}

func validProvider(name string) bool {
	switch name {
	case string(provider.OpenAI), string(provider.Anthropic), string(provider.DeepSeek):
		return true
	default:
		return false
	}
}

// modelFamily maps a model string to a bounded product family per provider
// (ADR-019 D5). The explicit table below is derived from the seed-catalog
// families; anything unmapped resolves to the bounded "other" fallback, so an
// arbitrary or adversarial model string can never become a high-cardinality
// label value.
func modelFamily(providerName, model string) string {
	model = strings.TrimSpace(model)
	switch providerName {
	case string(provider.OpenAI):
		if strings.HasPrefix(model, "gpt") {
			return "gpt"
		}
	case string(provider.Anthropic):
		if strings.HasPrefix(model, "claude") {
			return "claude"
		}
	case string(provider.DeepSeek):
		if strings.HasPrefix(model, "deepseek") {
			return "deepseek"
		}
	}
	return modelFamilyOther
}

func streamLabel(stream bool) string {
	if stream {
		return streamTrue
	}
	return streamFalse
}
