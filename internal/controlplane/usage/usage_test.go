package usage

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubUsageStore struct {
	summaryRow  UsageSummaryRow
	points      []Point
	groups      []Group
	timeseriesN int
	breakdownN  int
}

func (s *stubUsageStore) UsageSummary(context.Context, string, string, time.Time, time.Time) (UsageSummaryRow, error) {
	return s.summaryRow, nil
}

func (s *stubUsageStore) UsageTimeseries(context.Context, string, string, string, time.Time, time.Time) ([]Point, error) {
	s.timeseriesN++
	return s.points, nil
}

func (s *stubUsageStore) UsageBreakdown(context.Context, string, string, string, time.Time, time.Time, int) ([]Group, error) {
	s.breakdownN++
	return s.groups, nil
}

func TestEffectiveWindowDefaultsTo30Days(t *testing.T) {
	t.Parallel()
	from, to, err := EffectiveWindow(nil, nil)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if to.Sub(from) != DefaultWindow {
		t.Fatalf("default window = %v, want %v", to.Sub(from), DefaultWindow)
	}
	if !from.Before(to) {
		t.Fatalf("default window not ordered")
	}
}

func TestEffectiveWindowCapsAt90Days(t *testing.T) {
	t.Parallel()
	from := time.Now().UTC().Add(-MaxWindow - time.Hour)
	to := time.Now().UTC()
	if _, _, err := EffectiveWindow(&from, &to); err == nil {
		t.Fatal("window wider than 90 days accepted")
	}
	reversedFrom := to
	reversedTo := from
	if _, _, err := EffectiveWindow(&reversedFrom, &reversedTo); err == nil {
		t.Fatal("reversed window accepted")
	}
}

func TestSummaryDerivesErrorRateAndCoverage(t *testing.T) {
	t.Parallel()
	store := &stubUsageStore{summaryRow: UsageSummaryRow{
		RequestsTotal:        100,
		RequestsSucceeded:    90,
		RequestsFailed:       10,
		PricedRequests:       80,
		PromptTokens:         1000,
		CompletionTokens:     2000,
		TotalTokens:          3000,
		EstimatedCostNanoUSD: 5_000_000,
		LatencyMSSum:         1000,
		LatencyMSCount:       10,
		TTFTMSSum:            500,
		TTFTMSCount:          10,
	}}
	service := NewService(store)
	summary, err := service.Summary(context.Background(), "owner", "", nil, nil)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.ErrorRate == nil || *summary.ErrorRate != 0.1 {
		t.Fatalf("error rate = %v, want 0.1", summary.ErrorRate)
	}
	if summary.UnpricedRequests != 10 { // 90 succeeded - 80 priced
		t.Fatalf("unpriced = %d, want 10", summary.UnpricedRequests)
	}
	if summary.AvgLatencyMS == nil || *summary.AvgLatencyMS != 100 {
		t.Fatalf("avg latency = %v, want 100", summary.AvgLatencyMS)
	}
	if summary.AvgTTFTMS == nil || *summary.AvgTTFTMS != 50 {
		t.Fatalf("avg ttft = %v, want 50", summary.AvgTTFTMS)
	}
}

func TestSummaryEmptyWindowHasNullDerivedFields(t *testing.T) {
	t.Parallel()
	service := NewService(&stubUsageStore{})
	summary, err := service.Summary(context.Background(), "owner", "", nil, nil)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.ErrorRate != nil || summary.AvgLatencyMS != nil || summary.AvgTTFTMS != nil {
		t.Fatalf("empty window derived fields = %+v, want null", summary)
	}
	if summary.UnpricedRequests != 0 {
		t.Fatalf("unpriced = %d, want 0", summary.UnpricedRequests)
	}
}

func TestTimeseriesValidatesBucketAndPassesThrough(t *testing.T) {
	t.Parallel()
	store := &stubUsageStore{}
	service := NewService(store)
	from := time.Date(2026, 9, 1, 13, 37, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	if _, err := service.Timeseries(context.Background(), "owner", "", "week", &from, &to); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("bucket week error = %v, want ErrInvalidParams", err)
	}
	points, err := service.Timeseries(context.Background(), "owner", "", "hour", &from, &to)
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}
	if points == nil || store.timeseriesN != 1 {
		t.Fatalf("points = %v, calls = %d", points, store.timeseriesN)
	}
}

func TestBreakdownValidatesDimensionAndLimit(t *testing.T) {
	t.Parallel()
	service := NewService(&stubUsageStore{})
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC()
	if _, err := service.Breakdown(context.Background(), "owner", "", "status", &from, &to, 10); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("dimension error = %v, want ErrInvalidParams", err)
	}
	if _, err := service.Breakdown(context.Background(), "owner", "", "provider", &from, &to, MaxBreakdownLimit+1); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("limit error = %v, want ErrInvalidParams", err)
	}
	groups, err := service.Breakdown(context.Background(), "owner", "", "model", &from, &to, 0)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if groups == nil {
		t.Fatalf("breakdown groups nil, want empty slice")
	}
}
