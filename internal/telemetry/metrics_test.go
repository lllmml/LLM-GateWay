package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func gatherFamilies(t *testing.T, metrics *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	return byName
}

func labelValue(t *testing.T, metric *dto.Metric, name string) string {
	t.Helper()
	for _, pair := range metric.Label {
		if pair.GetName() == name {
			return pair.GetValue()
		}
	}
	return ""
}

func findCounter(t *testing.T, families map[string]*dto.MetricFamily, labels map[string]string) *dto.Metric {
	t.Helper()
	family, ok := families[metricRequestsTotal]
	if !ok {
		t.Fatalf("metric family %q not present", metricRequestsTotal)
	}
	for _, metric := range family.Metric {
		match := true
		for name, want := range labels {
			if labelValue(t, metric, name) != want {
				match = false
				break
			}
		}
		if match {
			return metric
		}
	}
	return nil
}

func activeGaugeValue(t *testing.T, families map[string]*dto.MetricFamily) float64 {
	t.Helper()
	family, ok := families[metricActiveRequests]
	if !ok {
		t.Fatalf("metric family %q not present", metricActiveRequests)
	}
	if len(family.Metric) != 1 {
		t.Fatalf("active requests family has %d series, want 1", len(family.Metric))
	}
	return family.Metric[0].GetGauge().GetValue()
}

func TestNewMetricsRegistersAndRecordsAllFourMetrics(t *testing.T) {
	metrics, err := NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	metrics.ObserveRequest("openai", "gpt-6-astra", false, "succeeded", 250*time.Millisecond)
	metrics.ObserveRequest("openai", "gpt-6-astra", false, "failed", 100*time.Millisecond)
	metrics.ObserveRequest("anthropic", "claude-opus-5", true, "failed", 5*time.Second)
	metrics.ObserveRequest("deepseek", "deepseek-chat", false, "succeeded", 10*time.Second)
	endStream := metrics.TrackInFlight("anthropic", true)
	endStream()

	families := gatherFamilies(t, metrics)
	for _, name := range []string{
		metricRequestsTotal,
		metricRequestDuration,
		metricActiveRequests,
		metricActiveStreams,
	} {
		if _, ok := families[name]; !ok {
			t.Fatalf("metric family %q missing after recording", name)
		}
	}

	success := findCounter(t, families, map[string]string{
		labelProvider:    "openai",
		labelModelFamily: "gpt",
		labelStatus:      "succeeded",
		labelStream:      "false",
	})
	if success == nil || success.GetCounter().GetValue() != 1 {
		t.Fatalf("openai/gpt/succeeded counter = %v, want 1 series", success)
	}
	failure := findCounter(t, families, map[string]string{
		labelProvider:    "openai",
		labelModelFamily: "gpt",
		labelStatus:      "failed",
		labelStream:      "false",
	})
	if failure == nil || failure.GetCounter().GetValue() != 1 {
		t.Fatalf("openai/gpt/failed counter = %v, want 1 series", failure)
	}
	streamFailed := findCounter(t, families, map[string]string{
		labelProvider:    "anthropic",
		labelModelFamily: "claude",
		labelStatus:      "failed",
		labelStream:      "true",
	})
	if streamFailed == nil || streamFailed.GetCounter().GetValue() != 1 {
		t.Fatalf("anthropic/claude stream failed counter = %v, want 1 series", streamFailed)
	}

	duration, ok := families[metricRequestDuration]
	if !ok {
		t.Fatal("request duration family missing")
	}
	sawOpenAI := false
	sawAnthropic := false
	sawDeepSeek := false
	for _, metric := range duration.Metric {
		sampleCount := metric.GetHistogram().GetSampleCount()
		switch labelValue(t, metric, labelProvider) {
		case "openai":
			if labelValue(t, metric, labelStream) == "false" && sampleCount != 2 {
				t.Fatalf("openai non-stream samples = %d, want 2", sampleCount)
			}
			sawOpenAI = true
		case "anthropic":
			if labelValue(t, metric, labelStream) == "true" && sampleCount != 1 {
				t.Fatalf("anthropic stream samples = %d, want 1", sampleCount)
			}
			sawAnthropic = true
		case "deepseek":
			if labelValue(t, metric, labelStream) == "false" && sampleCount != 1 {
				t.Fatalf("deepseek non-stream samples = %d, want 1", sampleCount)
			}
			sawDeepSeek = true
		}
	}
	if !sawOpenAI || !sawAnthropic || !sawDeepSeek {
		t.Fatalf("duration histogram missing provider series: openai=%v anthropic=%v deepseek=%v", sawOpenAI, sawAnthropic, sawDeepSeek)
	}

	if activeGaugeValue(t, families) != 0 {
		t.Fatalf("active requests gauge = %v after release, want 0", activeGaugeValue(t, families))
	}
	streams, ok := families[metricActiveStreams]
	if !ok {
		t.Fatal("active streams family missing")
	}
	for _, metric := range streams.Metric {
		if metric.GetGauge().GetValue() != 0 {
			t.Fatalf("active streams gauge not zero after release")
		}
	}
}

func TestModelFamilyMappingIsBounded(t *testing.T) {
	cases := []struct {
		providerName string
		model        string
		want         string
	}{
		{"openai", "gpt-6-astra", "gpt"},
		{"openai", "gpt-test", "gpt"},
		{"anthropic", "claude-opus-5", "claude"},
		{"anthropic", "claude-haiku-4-5-20251001", "claude"},
		{"deepseek", "deepseek-chat", "deepseek"},
		{"openai", "sk-live-abcdef", modelFamilyOther},
		{"anthropic", "some-unknown-model", modelFamilyOther},
		{"gemini", "claude-opus-5", modelFamilyOther},
		{"openai", "  gpt-6-astra  ", "gpt"},
	}
	for _, current := range cases {
		if got := modelFamily(current.providerName, current.model); got != current.want {
			t.Errorf("modelFamily(%q, %q) = %q, want %q", current.providerName, current.model, got, current.want)
		}
	}
}

func TestMetricLabelDomainsAreBoundedAndSecretsNeverLeak(t *testing.T) {
	const secretMarker = "vk-secret-token-do-not-leak"
	metrics, err := NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	metrics.ObserveRequest("openai", "gpt-6-astra", false, "succeeded", time.Millisecond)
	metrics.ObserveRequest("openai", secretMarker, false, "failed", time.Millisecond)
	metrics.ObserveRequest("anthropic", "claude-opus-5", true, "failed", time.Millisecond)
	metrics.ObserveRequest("deepseek", "deepseek-chat", false, "succeeded", time.Millisecond)
	end := metrics.TrackInFlight("deepseek", false)
	end()

	families := gatherFamilies(t, metrics)
	allowedProviders := map[string]bool{"openai": true, "anthropic": true, "deepseek": true}
	allowedFamilies := map[string]bool{"gpt": true, "claude": true, "deepseek": true, modelFamilyOther: true}
	allowedStatus := map[string]bool{"succeeded": true, "failed": true}
	allowedStream := map[string]bool{"true": true, "false": true}

	counter := families[metricRequestsTotal]
	for _, metric := range counter.Metric {
		for _, pair := range metric.Label {
			switch pair.GetName() {
			case labelProvider:
				if !allowedProviders[pair.GetValue()] {
					t.Errorf("unexpected provider label value %q", pair.GetValue())
				}
			case labelModelFamily:
				if !allowedFamilies[pair.GetValue()] {
					t.Errorf("unexpected model_family label value %q", pair.GetValue())
				}
			case labelStatus:
				if !allowedStatus[pair.GetValue()] {
					t.Errorf("unexpected status label value %q", pair.GetValue())
				}
			case labelStream:
				if !allowedStream[pair.GetValue()] {
					t.Errorf("unexpected stream label value %q", pair.GetValue())
				}
			default:
				t.Errorf("unexpected label name %q", pair.GetName())
			}
		}
	}
	if metric := findCounter(t, families, map[string]string{
		labelProvider:    "openai",
		labelModelFamily: modelFamilyOther,
		labelStatus:      "failed",
		labelStream:      "false",
	}); metric == nil {
		t.Fatal("adversarial model did not resolve to the bounded other family")
	}

	// The raw model string must never appear anywhere in the exposition.
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics content type = %q, want text/plain", recorder.Header().Get("Content-Type"))
	}
	body := recorder.Body.String()
	if strings.Contains(body, secretMarker) {
		t.Fatal("secret-shaped model string leaked into the metrics exposition")
	}
}

func TestInvalidProviderAndStatusInputsAreIgnored(t *testing.T) {
	metrics, err := NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	metrics.ObserveRequest("gemini", "anything", false, "succeeded", time.Millisecond)
	metrics.ObserveRequest("openai", "gpt-x", false, "not-a-real-status", time.Millisecond)
	metrics.TrackInFlight("gemini", false)
	metrics.TrackInFlight("openai", false)()

	families := gatherFamilies(t, metrics)
	if _, ok := families[metricRequestsTotal]; ok {
		t.Fatal("invalid provider/status inputs created a requests_total series")
	}
	if _, ok := families[metricRequestDuration]; ok {
		t.Fatal("invalid provider/status inputs created a duration series")
	}
	if activeGaugeValue(t, families) != 0 {
		t.Fatal("invalid provider leaked into the active gauge")
	}
}

func TestActiveGaugeReleasesExactlyOnce(t *testing.T) {
	metrics, err := NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	first := metrics.TrackInFlight("openai", false)
	families := gatherFamilies(t, metrics)
	if activeGaugeValue(t, families) != 1 {
		t.Fatalf("active requests after one start = %v, want 1", activeGaugeValue(t, families))
	}

	second := metrics.TrackInFlight("openai", false)
	second() // release one of two
	families = gatherFamilies(t, metrics)
	if activeGaugeValue(t, families) != 1 {
		t.Fatalf("active requests after releasing one of two = %v, want 1", activeGaugeValue(t, families))
	}

	first()
	first() // double release must be a no-op
	families = gatherFamilies(t, metrics)
	if activeGaugeValue(t, families) != 0 {
		t.Fatalf("active requests after release = %v, want 0", activeGaugeValue(t, families))
	}

	stream := metrics.TrackInFlight("deepseek", true)
	families = gatherFamilies(t, metrics)
	if activeGaugeValue(t, families) != 1 {
		t.Fatalf("active requests during stream = %v, want 1", activeGaugeValue(t, families))
	}
	streamFamily := families[metricActiveStreams]
	if len(streamFamily.Metric) != 1 || streamFamily.Metric[0].GetGauge().GetValue() != 1 {
		t.Fatalf("active streams during stream = %v, want 1", len(streamFamily.Metric))
	}
	if labelValue(t, streamFamily.Metric[0], labelProvider) != "deepseek" {
		t.Fatalf("active streams provider = %q, want deepseek", labelValue(t, streamFamily.Metric[0], labelProvider))
	}
	stream()
	families = gatherFamilies(t, metrics)
	if activeGaugeValue(t, families) != 0 {
		t.Fatalf("active requests after stream release = %v, want 0", activeGaugeValue(t, families))
	}
	if family := families[metricActiveStreams]; family.Metric[0].GetGauge().GetValue() != 0 {
		t.Fatal("active streams not zero after stream release")
	}
}

func TestMultipleMetricsInstancesUseIndependentRegistries(t *testing.T) {
	first, err := NewMetrics()
	if err != nil {
		t.Fatalf("first metrics: %v", err)
	}
	second, err := NewMetrics()
	if err != nil {
		t.Fatalf("second metrics: %v", err)
	}
	first.ObserveRequest("openai", "gpt-6-astra", false, "succeeded", time.Millisecond)

	firstFamilies := gatherFamilies(t, first)
	secondFamilies := gatherFamilies(t, second)
	if _, ok := firstFamilies[metricRequestsTotal]; !ok {
		t.Fatal("first instance lost its requests_total family")
	}
	if _, ok := secondFamilies[metricRequestsTotal]; ok {
		t.Fatal("second instance shares the first instance's registry")
	}
}
