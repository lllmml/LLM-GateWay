package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type handlerUsageStore struct {
	summary UsageSummaryRow
	points  []Point
	groups  []Group
}

func (s *handlerUsageStore) UsageSummary(context.Context, string, string, time.Time, time.Time) (UsageSummaryRow, error) {
	return s.summary, nil
}

func (s *handlerUsageStore) UsageTimeseries(context.Context, string, string, string, time.Time, time.Time) ([]Point, error) {
	return s.points, nil
}

func (s *handlerUsageStore) UsageBreakdown(context.Context, string, string, string, time.Time, time.Time, int) ([]Group, error) {
	return s.groups, nil
}

func signedIn(_ *http.Request) (string, bool)  { return "owner-user", true }
func signedOut(_ *http.Request) (string, bool) { return "", false }

func newUsageHandler(t *testing.T, store Store, currentUser CurrentUserID) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	NewHandler(NewService(store), currentUser).Register(mux)
	return mux
}

func TestUsageEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()
	mux := newUsageHandler(t, &handlerUsageStore{}, signedOut)
	for _, path := range []string{"/api/v1/usage/summary", "/api/v1/usage/timeseries", "/api/v1/usage/breakdown"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, response.Code)
		}
	}
}

func TestUsageEndpointsRejectBadParams(t *testing.T) {
	t.Parallel()
	mux := newUsageHandler(t, &handlerUsageStore{}, signedIn)
	cases := []string{
		"/api/v1/usage/timeseries?bucket=week",
		"/api/v1/usage/timeseries?project_id=not-a-uuid",
		"/api/v1/usage/timeseries?from=oops",
		"/api/v1/usage/breakdown?dimension=region",
		"/api/v1/usage/breakdown?dimension=provider&project_id=not-a-uuid",
		"/api/v1/usage/summary?project_id=not-a-uuid",
		"/api/v1/usage/summary?from=2026-09-02T00:00:00Z&to=2026-09-01T00:00:00Z",
		"/api/v1/usage/summary?from=2020-01-01T00:00:00Z&to=2026-09-01T00:00:00Z", // >90 days
	}
	for _, path := range cases {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, response.Code)
		}
	}
}

func TestUsageSummaryRespondsWithDerivedFields(t *testing.T) {
	t.Parallel()
	store := &handlerUsageStore{summary: UsageSummaryRow{
		RequestsTotal: 10, RequestsSucceeded: 8, RequestsFailed: 2, PricedRequests: 6,
		PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, EstimatedCostNanoUSD: 9,
	}}
	mux := newUsageHandler(t, store, signedIn)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/usage/summary", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var summary Summary
	if err := json.NewDecoder(response.Body).Decode(&summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.UnpricedRequests != 2 || summary.ErrorRate == nil || *summary.ErrorRate != 0.2 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.From.IsZero() || !summary.From.Before(summary.To) {
		t.Fatalf("window not populated: %+v", summary)
	}
}
