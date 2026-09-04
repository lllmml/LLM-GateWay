//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"time"

	sharedapikey "github.com/lllmml/production-go-llm-gateway/internal/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/credential"
	projectdomain "github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/providerconfig"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/requesthistory"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/usage"
	"github.com/lllmml/production-go-llm-gateway/internal/dataplane"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

// migratedAnalyticsStore migrates through the Week 7 seed migration so
// analytics fixtures can reference real model_prices rows by FK.
func migratedAnalyticsStore(t *testing.T, ctx context.Context) (*Store, func()) {
	t.Helper()
	store, cleanup := newMigratedStore(t, ctx)
	applyMigration(t, ctx, store.pool, "000005_seed_model_prices.up.sql")
	return store, cleanup
}

// analyticsFixture seeds one owner with a project/key/credential/config and
// returns a function that inserts gateway request rows with the given times.
type analyticsFixture struct {
	store        *Store
	ownerID      string
	projectID    string
	keyID        string
	credentialID string
	otherOwnerID string
}

func newAnalyticsFixture(t *testing.T, ctx context.Context, store *Store) *analyticsFixture {
	t.Helper()
	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{GitHubID: 8101, GitHubLogin: "analytics-owner"})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	other, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{GitHubID: 8102, GitHubLogin: "analytics-other"})
	if err != nil {
		t.Fatalf("upsert other owner: %v", err)
	}
	project, err := store.CreateProject(ctx, projectdomain.CreateParams{OwnerUserID: owner.ID, Name: "Analytics", Slug: "analytics"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := store.CreateProject(ctx, projectdomain.CreateParams{OwnerUserID: other.ID, Name: "Other", Slug: "analytics-other"}); err != nil {
		t.Fatalf("create other project: %v", err)
	}
	keyService, err := apikey.NewService(store, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("key service: %v", err)
	}
	keyResult, err := keyService.Create(ctx, owner.ID, project.ID, "analytics-client")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	cipher, credentialService := newCredentialServiceForIntegration(t, store)
	_ = cipher
	cred, err := credentialService.Create(ctx, owner.ID, project.ID, "openai", "primary", "sk-live-not-logged")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if _, err := store.UpsertProviderConfig(ctx, providerconfig.UpsertParams{
		OwnerUserID: owner.ID, ProjectID: project.ID, Provider: "openai", CredentialID: cred.ID, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert config: %v", err)
	}
	return &analyticsFixture{
		store: store, ownerID: owner.ID, projectID: project.ID, keyID: keyResult.Key.ID,
		credentialID: cred.ID, otherOwnerID: other.ID,
	}
}

// insertRequest finalizes a gateway request row through the data plane store
// adapters so the durable lifecycle semantics are exercised end to end.
func (f *analyticsFixture) insertRequest(t *testing.T, ctx context.Context, startedAt time.Time, status string, pricing *dataplane.ModelPrice, tokens *provider.Usage, retries int16, latencyMS *int64) string {
	t.Helper()
	return f.insertProviderRequest(t, ctx, startedAt, provider.OpenAI, "gpt-5.6-terra", status, pricing, tokens, retries, latencyMS)
}

// insertProviderRequest finalizes a gateway request row for an arbitrary
// provider/model through the data plane store adapters.
func (f *analyticsFixture) insertProviderRequest(t *testing.T, ctx context.Context, startedAt time.Time, name provider.Name, model, status string, pricing *dataplane.ModelPrice, tokens *provider.Usage, retries int16, latencyMS *int64) string {
	t.Helper()
	record, err := f.store.CreateGatewayRequest(ctx, dataplane.CreateRequestParams{
		ProjectID: f.projectID, VirtualKeyID: f.keyID, ProviderCredentialID: f.credentialID,
		Provider: name, Model: model, IsStream: false, StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("create gateway request: %v", err)
	}
	var pricingID *string
	var cost *int64
	if pricing != nil {
		id := pricing.ID
		pricingID = &id
		if tokens != nil {
			computed, ok := costForTest(tokens.PromptTokens, tokens.CompletionTokens, pricing.InputNanoUSDPerMillion, pricing.OutputNanoUSDPerMillion)
			if ok {
				cost = &computed
			}
		}
	}
	var prompt, completion, total *int64
	if tokens != nil {
		prompt = ptrInt64(tokens.PromptTokens)
		completion = ptrInt64(tokens.CompletionTokens)
		total = ptrInt64(tokens.TotalTokens)
	}
	var category *provider.ErrorCategory
	if status == "failed" {
		failed := provider.ProviderTimeout
		category = &failed
	}
	completedAt := startedAt.Add(time.Second)
	if err := f.store.FinalizeGatewayRequest(ctx, dataplane.FinalizeParams{
		ID: record.ID, Status: status, CompletedAt: completedAt, LatencyMS: latencyMS,
		RetryCount: retries, ErrorCategory: category, PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: total, PricingID: pricingID, EstimatedCostNanoUSD: cost,
	}); err != nil {
		t.Fatalf("finalize gateway request: %v", err)
	}
	return record.ID
}

func costForTest(prompt, completion, input, output int64) (int64, bool) {
	if prompt < 0 || completion < 0 || input < 0 || output < 0 {
		return 0, false
	}
	if (input != 0 && prompt > math.MaxInt64/input) || (output != 0 && completion > math.MaxInt64/output) {
		return 0, false
	}
	return prompt*input/1_000_000 + completion*output/1_000_000, true
}

var analyticsTerraPrice = dataplane.ModelPrice{
	ID:                     "bf814983-0ae3-43bb-8b5f-5c45063d4874",
	InputNanoUSDPerMillion: 2_000_000_000, OutputNanoUSDPerMillion: 12_000_000_000,
}

func TestAnalyticsRequestHistoryOwnershipPaginationAndWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, cleanup := migratedAnalyticsStore(t, ctx)
	defer cleanup()
	fixture := newAnalyticsFixture(t, ctx, store)

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Eight rows with deterministic times, including two with the same
	// started_at (tie broken by id DESC) and one exactly at the window edge.
	ids := make([]string, 0, 8)
	times := []time.Time{
		base.Add(7 * time.Hour), base.Add(6 * time.Hour), base.Add(6 * time.Hour),
		base.Add(5 * time.Hour), base.Add(4 * time.Hour), base.Add(3 * time.Hour),
		base.Add(2 * time.Hour), base,
	}
	for _, at := range times {
		ids = append(ids, fixture.insertRequest(t, ctx, at, "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, 0, nil))
	}

	// Window [base, base+8h): exactly at from is included; exactly at to is not.
	from := base
	to := base.Add(8 * time.Hour)
	page, err := requesthistory.NewService(fixture.store).List(ctx, fixture.ownerID, requesthistory.ListParams{From: &from, To: &to, Limit: 3})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 3 || !page.HasMore {
		t.Fatalf("first page len=%d hasMore=%v", len(page.Items), page.HasMore)
	}
	if !page.Items[0].StartedAt.Equal(base.Add(7 * time.Hour)) {
		t.Fatalf("first item = %v, want newest first", page.Items[0].StartedAt)
	}
	cursor, err := requesthistory.DecodeCursor(*page.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	second, err := requesthistory.NewService(fixture.store).List(ctx, fixture.ownerID, requesthistory.ListParams{From: &from, To: &to, Cursor: &cursor, Limit: 3})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	third, err := requesthistory.NewService(fixture.store).List(ctx, fixture.ownerID, requesthistory.ListParams{From: &from, To: &to, Limit: 100})
	if err != nil {
		t.Fatalf("full list: %v", err)
	}
	if len(third.Items) != 8 {
		t.Fatalf("window rows = %d, want 8 (from inclusive, to exclusive)", len(third.Items))
	}
	// Page 3 fetches the remainder after the second page's cursor; combined
	// coverage must be exactly the 8 window rows with no duplicates or gaps.
	lastCursor, err := requesthistory.DecodeCursor(*second.NextCursor)
	if err != nil {
		t.Fatalf("decode second cursor: %v", err)
	}
	thirdPage, err := requesthistory.NewService(fixture.store).List(ctx, fixture.ownerID, requesthistory.ListParams{From: &from, To: &to, Cursor: &lastCursor, Limit: 100})
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	if thirdPage.HasMore || len(thirdPage.Items) != 2 {
		t.Fatalf("third page = %d items hasMore=%v, want 2 terminal items", len(thirdPage.Items), thirdPage.HasMore)
	}
	seen := map[string]bool{}
	for _, items := range [][]requesthistory.Request{page.Items, second.Items, thirdPage.Items} {
		for _, item := range items {
			if seen[item.ID] {
				t.Fatalf("duplicate id across pages: %s", item.ID)
			}
			seen[item.ID] = true
		}
	}
	if len(seen) != 8 {
		t.Fatalf("pages covered %d unique rows, want 8 without gaps", len(seen))
	}

	// Ownership: the other owner sees nothing; their direct get is 404-shaped.
	otherPage, err := requesthistory.NewService(fixture.store).List(ctx, fixture.otherOwnerID, requesthistory.ListParams{Limit: 100})
	if err != nil {
		t.Fatalf("other owner list: %v", err)
	}
	if len(otherPage.Items) != 0 {
		t.Fatalf("other owner saw %d rows, want 0", len(otherPage.Items))
	}
	if _, err := requesthistory.NewService(fixture.store).Get(ctx, fixture.otherOwnerID, ids[0]); !errors.Is(err, requesthistory.ErrNotFound) {
		t.Fatalf("cross-owner get error = %v, want ErrNotFound", err)
	}
	if _, err := requesthistory.NewService(fixture.store).Get(ctx, fixture.ownerID, "00000000-0000-4000-8000-00000000ffff"); !errors.Is(err, requesthistory.ErrNotFound) {
		t.Fatalf("missing get error = %v, want ErrNotFound", err)
	}
	got, err := requesthistory.NewService(fixture.store).Get(ctx, fixture.ownerID, ids[0])
	if err != nil {
		t.Fatalf("owner get: %v", err)
	}
	if got.ID != ids[0] || got.ProjectName != "Analytics" || got.VirtualKeyPrefix == "" {
		t.Fatalf("get row = %+v", got)
	}
}

func TestAnalyticsSummaryCoverageAndIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, cleanup := migratedAnalyticsStore(t, ctx)
	defer cleanup()
	fixture := newAnalyticsFixture(t, ctx, store)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// 10 succeeded priced, 3 succeeded unpriced (no pricing), 2 failed.
	for i := 0; i < 10; i++ {
		fixture.insertRequest(t, ctx, base.Add(time.Duration(i)*time.Minute), "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, 0, ptrInt64(120))
	}
	for i := 0; i < 3; i++ {
		fixture.insertRequest(t, ctx, base.Add(time.Duration(10+i)*time.Minute), "succeeded", nil, &provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 0, ptrInt64(80))
	}
	for i := 0; i < 2; i++ {
		fixture.insertRequest(t, ctx, base.Add(time.Duration(13+i)*time.Minute), "failed", nil, nil, 2, nil)
	}
	from := base.Add(-time.Hour)
	to := base.Add(24 * time.Hour)
	service := usage.NewService(fixture.store)
	summary, err := service.Summary(ctx, fixture.ownerID, "", &from, &to)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.RequestsTotal != 15 || summary.RequestsSucceeded != 13 || summary.RequestsFailed != 2 {
		t.Fatalf("counts = %+v", summary)
	}
	if summary.PricedRequests != 10 || summary.UnpricedRequests != 3 {
		t.Fatalf("coverage priced=%d unpriced=%d, want 10/3", summary.PricedRequests, summary.UnpricedRequests)
	}
	if summary.ErrorRate == nil || math.Abs(*summary.ErrorRate-2.0/15.0) > 1e-9 {
		t.Fatalf("error rate = %v", summary.ErrorRate)
	}
	// cost only aggregates priced rows: 10 * (1000*2e9/1e6 + 500*12e9/1e6)
	// = 10 * (2_000_000 + 6_000_000) = 80_000_000 nano-USD.
	if summary.EstimatedCostNanoUSD != 80_000_000 {
		t.Fatalf("estimated cost = %d, want 80000000", summary.EstimatedCostNanoUSD)
	}
	if summary.AvgLatencyMS == nil || math.Abs(*summary.AvgLatencyMS-(10*120+3*80)/13.0) > 1 {
		t.Fatalf("avg latency = %v", summary.AvgLatencyMS)
	}
	other, err := service.Summary(ctx, fixture.otherOwnerID, "", &from, &to)
	if err != nil {
		t.Fatalf("other summary: %v", err)
	}
	if other.RequestsTotal != 0 || other.PricedRequests != 0 || other.UnpricedRequests != 0 {
		t.Fatalf("other owner summary = %+v, want empty", other)
	}
}

func TestAnalyticsTimeseriesAlignmentZeroFillAndBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, cleanup := migratedAnalyticsStore(t, ctx)
	defer cleanup()
	fixture := newAnalyticsFixture(t, ctx, store)

	// Non-aligned from exercises the date_trunc(bucket, from) alignment.
	from := time.Date(2026, 9, 1, 13, 37, 0, 0, time.UTC)
	// One request at the exact from (must count), one mid-window, one at the
	// exact to boundary (must NOT count into the window but may occupy its own
	// aligned bucket only if before to - handled by the aggregate filter).
	fixture.insertRequest(t, ctx, from, "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 0, nil)
	fixture.insertRequest(t, ctx, from.Add(2*time.Hour), "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 0, nil)
	fixture.insertRequest(t, ctx, from.Add(47*time.Hour), "failed", nil, nil, 1, nil)
	to := from.Add(48 * time.Hour) // 2026-09-03 13:37 UTC

	service := usage.NewService(fixture.store)

	dayPoints, err := service.Timeseries(ctx, fixture.ownerID, "", "day", &from, &to)
	if err != nil {
		t.Fatalf("day timeseries: %v", err)
	}
	// Aligned buckets: Sep 1, Sep 2, Sep 3 00:00 UTC (partial first/last).
	if len(dayPoints) != 3 {
		t.Fatalf("day points = %d, want 3 (partial first and last buckets)", len(dayPoints))
	}
	if !dayPoints[0].TS.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first bucket = %v, want Sep 1 00:00 UTC", dayPoints[0].TS)
	}
	// 13:37 bucket on Sep 1 counts the from-boundary request and the +2h one;
	// Sep 2 is zero-filled; Sep 3 carries the failed request at 12:37 UTC.
	if dayPoints[0].RequestsTotal != 2 || dayPoints[0].RequestsSucceeded != 2 {
		t.Fatalf("day first bucket = %+v", dayPoints[0])
	}
	if dayPoints[1].RequestsTotal != 0 {
		t.Fatalf("Sep 2 should be zero-filled, got %+v", dayPoints[1])
	}
	if dayPoints[2].RequestsTotal != 1 || dayPoints[2].RequestsFailed != 1 {
		t.Fatalf("Sep 3 bucket = %+v", dayPoints[2])
	}

	hourPoints, err := service.Timeseries(ctx, fixture.ownerID, "", "hour", &from, &to)
	if err != nil {
		t.Fatalf("hour timeseries: %v", err)
	}
	// Hours: 2026-09-01 13:00 .. 2026-09-03 13:00 UTC inclusive = 49 buckets;
	// the final bucket is partial (13:00-13:37) and zero-filled.
	if len(hourPoints) != 49 {
		t.Fatalf("hour points = %d, want 49", len(hourPoints))
	}
	if !hourPoints[0].TS.Equal(time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("first hour bucket = %v, want 13:00 UTC", hourPoints[0].TS)
	}
	if hourPoints[0].RequestsTotal != 1 {
		t.Fatalf("hour 13 bucket total = %d, want 1 (request at 13:37)", hourPoints[0].RequestsTotal)
	}
	// 14:00 bucket must be zero-filled.
	if hourPoints[1].RequestsTotal != 0 {
		t.Fatalf("hour 14 bucket = %+v, want zero-filled", hourPoints[1])
	}
	// 15:00 bucket has the +2h request (15:37 -> hour 15).
	if hourPoints[2].RequestsTotal != 1 {
		t.Fatalf("hour 15 bucket total = %d, want 1", hourPoints[2].RequestsTotal)
	}
	// Failed request at Sep 3 12:37 counts in its hour-12 bucket.
	if hourPoints[47].RequestsTotal != 1 || hourPoints[47].RequestsFailed != 1 {
		t.Fatalf("hour 12 bucket = %+v", hourPoints[47])
	}
	if hourPoints[48].RequestsTotal != 0 {
		t.Fatalf("final partial hour bucket = %+v, want zero-filled", hourPoints[48])
	}
}

func TestAnalyticsBreakdownDimensions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, cleanup := migratedAnalyticsStore(t, ctx)
	defer cleanup()
	fixture := newAnalyticsFixture(t, ctx, store)

	base := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	from := base.Add(-time.Hour)
	to := base.Add(24 * time.Hour)
	fixture.insertRequest(t, ctx, base, "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, 0, nil)
	fixture.insertRequest(t, ctx, base.Add(time.Minute), "failed", nil, nil, 1, nil)
	service := usage.NewService(fixture.store)

	providerGroups, err := service.Breakdown(ctx, fixture.ownerID, "", "provider", &from, &to, 10)
	if err != nil {
		t.Fatalf("provider breakdown: %v", err)
	}
	if len(providerGroups) != 1 || providerGroups[0].Key != "openai" || providerGroups[0].RequestsTotal != 2 || providerGroups[0].RequestsFailed != 1 {
		t.Fatalf("provider groups = %+v", providerGroups)
	}
	modelGroups, err := service.Breakdown(ctx, fixture.ownerID, "", "model", &from, &to, 10)
	if err != nil {
		t.Fatalf("model breakdown: %v", err)
	}
	if len(modelGroups) != 1 || modelGroups[0].Key != "gpt-5.6-terra" || modelGroups[0].EstimatedCostNanoUSD != 8_000_000 {
		t.Fatalf("model groups = %+v", modelGroups)
	}
	keyGroups, err := service.Breakdown(ctx, fixture.ownerID, "", "key", &from, &to, 10)
	if err != nil {
		t.Fatalf("key breakdown: %v", err)
	}
	if len(keyGroups) != 1 || keyGroups[0].KeyID == nil || *keyGroups[0].KeyID != fixture.keyID || keyGroups[0].KeyPrefix == nil || *keyGroups[0].KeyPrefix == "" {
		t.Fatalf("key groups = %+v", keyGroups)
	}
}

func TestSeedMigrationIsDeterministicReversibleAndValid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, _, cleanupPool := newIsolatedPool(t, ctx)
	defer cleanupPool()
	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000002_virtual_api_keys.up.sql")
	applyMigration(t, ctx, pool, "000003_provider_credentials.up.sql")
	applyMigration(t, ctx, pool, "000004_data_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000005_seed_model_prices.up.sql")

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM model_prices").Scan(&count); err != nil {
		t.Fatalf("count prices: %v", err)
	}
	if count != 7 {
		t.Fatalf("seeded prices = %d, want 7", count)
	}

	// No overlapping windows in the catalog.
	var overlaps int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM model_prices AS a
		JOIN model_prices AS b
		  ON b.provider = a.provider AND b.model = a.model AND b.id <> a.id
		 AND b.effective_from < COALESCE(a.effective_to, 'infinity'::timestamptz)
		 AND a.effective_from < COALESCE(b.effective_to, 'infinity'::timestamptz)`).Scan(&overlaps)
	if err != nil {
		t.Fatalf("overlap check: %v", err)
	}
	if overlaps != 0 {
		t.Fatalf("catalog has %d overlapping price windows", overlaps)
	}

	// Effective dates match the official announcements recorded in the seed.
	rows, err := pool.Query(ctx, `
		SELECT model, effective_from FROM model_prices
		WHERE (provider, effective_from) IN (
			('openai', '2026-09-03T00:00:00Z'), ('openai', '2026-07-30T00:00:00Z'),
			('anthropic', '2026-09-01T00:00:00Z'), ('anthropic', '2026-07-24T00:00:00Z'),
			('anthropic', '2026-06-30T00:00:00Z'), ('anthropic', '2025-10-15T00:00:00Z'))
		ORDER BY model`)
	if err != nil {
		t.Fatalf("query seed rows: %v", err)
	}
	defer rows.Close()
	want := map[string]string{
		"gpt-6-astra": "2026-09-03T00:00:00Z", "gpt-5.6-terra": "2026-07-30T00:00:00Z",
		"gpt-5.6-luna": "2026-07-30T00:00:00Z", "claude-fable-5-1": "2026-09-01T00:00:00Z",
		"claude-opus-5": "2026-07-24T00:00:00Z", "claude-sonnet-5": "2026-06-30T00:00:00Z",
		"claude-haiku-4-5-20251001": "2025-10-15T00:00:00Z",
	}
	got := map[string]string{}
	for rows.Next() {
		var model string
		var effective time.Time
		if err := rows.Scan(&model, &effective); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[model] = effective.UTC().Format(time.RFC3339)
	}
	if len(got) != 7 {
		t.Fatalf("matched seed rows = %d, want 7 (all models present)", len(got))
	}
	for model, wantTime := range want {
		if got[model] != wantTime {
			t.Fatalf("model %s effective_from = %s, want %s", model, got[model], wantTime)
		}
	}

	// Reversible: down removes exactly the seed, up restores it.
	applyMigration(t, ctx, pool, "000005_seed_model_prices.down.sql")
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM model_prices").Scan(&count); err != nil {
		t.Fatalf("count after down: %v", err)
	}
	if count != 0 {
		t.Fatalf("prices after down = %d, want 0", count)
	}
	applyMigration(t, ctx, pool, "000005_seed_model_prices.up.sql")
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM model_prices").Scan(&count); err != nil {
		t.Fatalf("count after re-up: %v", err)
	}
	if count != 7 {
		t.Fatalf("prices after re-up = %d, want 7", count)
	}
}

func TestFindModelPriceAtWindowBoundariesAndOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	insertPrice := func(id, providerName, model string, input int64, effectiveFrom, effectiveTo string) {
		t.Helper()
		_, err := store.pool.Exec(ctx, `
			INSERT INTO model_prices (id, provider, model, input_nano_usd_per_million, output_nano_usd_per_million, effective_from, effective_to, source_note)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::timestamptz, 'integration fixture')`,
			id, providerName, model, input, 1, effectiveFrom, effectiveTo)
		if err != nil {
			t.Fatalf("insert price %s: %v", id, err)
		}
	}
	insertPrice("aaaaaaaa-0000-4000-8000-000000000001", "openai", "model-a", 1_000_000_000, "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z")
	insertPrice("aaaaaaaa-0000-4000-8000-000000000002", "openai", "model-a", 2_000_000_000, "2026-05-01T00:00:00Z", "")
	insertPrice("aaaaaaaa-0000-4000-8000-000000000003", "openai", "model-b", 3_000_000_000, "2026-01-01T00:00:00Z", "")
	insertPrice("aaaaaaaa-0000-4000-8000-000000000004", "openai", "model-c", 4_000_000_000, "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z")

	// Window semantics: at == effective_from is included; at == effective_to
	// is excluded. Two overlapping rows for model-a resolve deterministically
	// to the most recently effective (2e9, id ...02).
	atBoundary := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	price, err := store.FindModelPrice(ctx, provider.OpenAI, "model-b", atBoundary)
	if err != nil {
		t.Fatalf("find at effective_from boundary: %v", err)
	}
	if price.ID != "aaaaaaaa-0000-4000-8000-000000000003" {
		t.Fatalf("price = %+v", price)
	}
	atExactEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.FindModelPrice(ctx, provider.OpenAI, "model-c", atExactEnd); !errors.Is(err, dataplane.ErrNotFound) {
		t.Fatalf("find at effective_to boundary error = %v, want ErrNotFound", err)
	}
	justBeforeEnd := time.Date(2026, 5, 31, 23, 59, 59, 0, time.UTC)
	price, err = store.FindModelPrice(ctx, provider.OpenAI, "model-c", justBeforeEnd)
	if err != nil || price.ID != "aaaaaaaa-0000-4000-8000-000000000004" {
		t.Fatalf("find just before effective_to = %+v, err=%v", price, err)
	}
	atOverlap := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	price, err = store.FindModelPrice(ctx, provider.OpenAI, "model-a", atOverlap)
	if err != nil {
		t.Fatalf("find during overlap: %v", err)
	}
	if price.ID != "aaaaaaaa-0000-4000-8000-000000000002" || price.InputNanoUSDPerMillion != 2_000_000_000 {
		t.Fatalf("overlap resolution = %+v, want newest effective (2e9)", price)
	}
	atNullEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	price, err = store.FindModelPrice(ctx, provider.OpenAI, "model-a", atNullEnd)
	if err != nil {
		t.Fatalf("find with NULL effective_to: %v", err)
	}
	if price.ID != "aaaaaaaa-0000-4000-8000-000000000002" {
		t.Fatalf("null-end resolution = %+v", price)
	}
}

var _ = fmt.Sprintf // keep fmt available for future diagnostics
var _ = sharedapikey.ParseRawKey
var _ = credential.SealedSecret{}
var _ = dataplane.ErrNotFound

func insertModelPriceRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, providerName, model string, inputNano int64, effectiveFrom, effectiveTo string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO model_prices (id, provider, model, input_nano_usd_per_million, output_nano_usd_per_million, effective_from, effective_to, source_note)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::timestamptz, 'integration fixture')`,
		id, providerName, model, inputNano, 1, effectiveFrom, effectiveTo)
	if err != nil {
		t.Fatalf("insert price %s: %v", id, err)
	}
}

func countOverlaps(t *testing.T, ctx context.Context, pool *pgxpool.Pool, providerName, model string) int {
	t.Helper()
	var overlaps int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM model_prices AS a
		JOIN model_prices AS b
		  ON b.provider = a.provider AND b.model = a.model AND b.id <> a.id
		 AND b.effective_from < COALESCE(a.effective_to, 'infinity'::timestamptz)
		 AND a.effective_from < COALESCE(b.effective_to, 'infinity'::timestamptz)
		WHERE a.provider = $1 AND a.model = $2`, providerName, model).Scan(&overlaps)
	if err != nil {
		t.Fatalf("overlap count: %v", err)
	}
	return overlaps
}

func TestModelPricesAdjacentWindowsAreNotOverlapping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, _, cleanup := newIsolatedPool(t, ctx)
	defer cleanup()
	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000002_virtual_api_keys.up.sql")
	applyMigration(t, ctx, pool, "000003_provider_credentials.up.sql")
	applyMigration(t, ctx, pool, "000004_data_plane_foundation.up.sql")

	// Adjacent half-open windows [t0,t1) + [t1,t2) must NOT be an overlap.
	insertModelPriceRow(t, ctx, pool, "bbbbbbbb-0000-4000-8000-000000000001", "openai", "model-adjacent", 1_000_000_000, "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z")
	insertModelPriceRow(t, ctx, pool, "bbbbbbbb-0000-4000-8000-000000000002", "openai", "model-adjacent", 2_000_000_000, "2026-06-01T00:00:00Z", "")
	if overlaps := countOverlaps(t, ctx, pool, "openai", "model-adjacent"); overlaps != 0 {
		t.Fatalf("adjacent windows reported as overlapping (%d); half-open [t0,t1)+[t1,t2) must be valid", overlaps)
	}

	// Truly overlapping windows [t0,t2) + [t1,t3) MUST be detected.
	insertModelPriceRow(t, ctx, pool, "bbbbbbbb-0000-4000-8000-000000000003", "openai", "model-overlap", 1_000_000_000, "2026-01-01T00:00:00Z", "")
	insertModelPriceRow(t, ctx, pool, "bbbbbbbb-0000-4000-8000-000000000004", "openai", "model-overlap", 2_000_000_000, "2026-03-01T00:00:00Z", "")
	if overlaps := countOverlaps(t, ctx, pool, "openai", "model-overlap"); overlaps == 0 {
		t.Fatal("overlapping windows were not detected")
	}
}

func TestTimeseriesBucketsAreUTCIndependentOfSessionTimezone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, _, cleanupPool := newIsolatedPool(t, ctx)
	defer cleanupPool()
	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000002_virtual_api_keys.up.sql")
	applyMigration(t, ctx, pool, "000003_provider_credentials.up.sql")
	applyMigration(t, ctx, pool, "000004_data_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000005_seed_model_prices.up.sql")

	// Bind every store call in this test to ONE connection whose session is
	// deliberately non-UTC, so the query itself must do the UTC alignment.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET TIME ZONE 'America/Los_Angeles'"); err != nil {
		t.Fatalf("set session timezone: %v", err)
	}
	var zone string
	if err := conn.QueryRow(ctx, "SHOW TIME ZONE").Scan(&zone); err != nil {
		t.Fatalf("read session timezone: %v", err)
	}
	if zone != "America/Los_Angeles" {
		t.Fatalf("session timezone = %q, want America/Los_Angeles", zone)
	}
	store := &Store{queries: New(conn)}
	fixture := newAnalyticsFixture(t, ctx, store)

	// Day buckets: a request at 01:00Z on Sep 1 sits in UTC day Sep 1 even
	// though a Los_Angeles session would place it on the previous local day.
	fromDay := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fixture.insertRequest(t, ctx, fromDay.Add(time.Hour), "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, 0, nil)
	fixture.insertRequest(t, ctx, fromDay.Add(26*time.Hour), "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, 0, nil)
	toDay := fromDay.Add(72 * time.Hour)

	service := usage.NewService(store)
	dayPoints, err := service.Timeseries(ctx, fixture.ownerID, "", "day", &fromDay, &toDay)
	if err != nil {
		t.Fatalf("day timeseries under LA session: %v", err)
	}
	if len(dayPoints) != 3 {
		t.Fatalf("day points = %d, want 3", len(dayPoints))
	}
	wantUTC := []time.Time{
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	}
	for i, want := range wantUTC {
		if !dayPoints[i].TS.Equal(want) {
			t.Fatalf("day bucket %d = %v, want UTC %v", i, dayPoints[i].TS, want)
		}
	}
	if dayPoints[0].RequestsTotal != 1 || dayPoints[1].RequestsTotal != 1 || dayPoints[2].RequestsTotal != 0 {
		t.Fatalf("day totals = %+v", dayPoints)
	}

	// Hour buckets similarly land on UTC :00 boundaries under the LA session.
	fromHour := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	fixture.insertRequest(t, ctx, fromHour.Add(30*time.Minute), "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, 0, nil)
	fixture.insertRequest(t, ctx, fromHour.Add(105*time.Minute), "succeeded", nil, &provider.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}, 0, nil)
	toHour := fromHour.Add(3 * time.Hour)
	hourPoints, err := service.Timeseries(ctx, fixture.ownerID, "", "hour", &fromHour, &toHour)
	if err != nil {
		t.Fatalf("hour timeseries under LA session: %v", err)
	}
	if len(hourPoints) != 3 {
		t.Fatalf("hour points = %d, want 3", len(hourPoints))
	}
	if !hourPoints[0].TS.Equal(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)) || hourPoints[0].RequestsTotal != 1 {
		t.Fatalf("hour bucket 0 = %+v", hourPoints[0])
	}
	if !hourPoints[1].TS.Equal(time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)) || hourPoints[1].RequestsTotal != 1 {
		t.Fatalf("hour bucket 1 = %+v", hourPoints[1])
	}
	if !hourPoints[2].TS.Equal(time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)) || hourPoints[2].RequestsTotal != 0 {
		t.Fatalf("hour bucket 2 = %+v", hourPoints[2])
	}
}

func TestUnpricedRequestsKeepTokensButNoCostAndCoverage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, cleanup := migratedAnalyticsStore(t, ctx)
	defer cleanup()
	fixture := newAnalyticsFixture(t, ctx, store)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// One priced OpenAI request and one deliberately unpriced DeepSeek request
	// (Week 7 has no DeepSeek price versions). Both report usage.
	pricedID := fixture.insertRequest(t, ctx, base, "succeeded", &analyticsTerraPrice, &provider.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, 0, nil)
	unpricedID := fixture.insertProviderRequest(t, ctx, base.Add(time.Minute), provider.DeepSeek, "deepseek-chat", "succeeded", nil, &provider.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300}, 0, nil)
	_ = pricedID
	_ = unpricedID

	from := base.Add(-time.Hour)
	to := base.Add(24 * time.Hour)
	service := usage.NewService(fixture.store)

	summary, err := service.Summary(ctx, fixture.ownerID, "", &from, &to)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// Tokens include EVERY reported usage, priced or not.
	if summary.PromptTokens != 1200 || summary.CompletionTokens != 600 || summary.TotalTokens != 1800 {
		t.Fatalf("token sums = %+v, want 1200/600/1800 (unpriced usage included)", summary)
	}
	// Cost only includes the priced request; coverage says one is unpriced.
	if summary.EstimatedCostNanoUSD != 8_000_000 {
		t.Fatalf("cost = %d, want 8000000 (only the priced request)", summary.EstimatedCostNanoUSD)
	}
	if summary.PricedRequests != 1 || summary.UnpricedRequests != 1 {
		t.Fatalf("coverage priced=%d unpriced=%d, want 1/1", summary.PricedRequests, summary.UnpricedRequests)
	}

	// Breakdown: the DeepSeek group is entirely unpriced yet still reports its
	// tokens; the aggregate cost value is zero but priced_requests==0 exposes
	// that nothing was attributed (the UI renders it as unpriced, not $0.00).
	groups, err := service.Breakdown(ctx, fixture.ownerID, "", "provider", &from, &to, 10)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("provider groups = %+v", groups)
	}
	var openAIGroup, deepSeekGroup *usage.Group
	for i := range groups {
		if groups[i].Key == "openai" {
			openAIGroup = &groups[i]
		}
		if groups[i].Key == "deepseek" {
			deepSeekGroup = &groups[i]
		}
	}
	if openAIGroup == nil || deepSeekGroup == nil {
		t.Fatalf("missing groups: %+v", groups)
	}
	if openAIGroup.PricedRequests != 1 || openAIGroup.UnpricedRequests != 0 || openAIGroup.EstimatedCostNanoUSD != 8_000_000 {
		t.Fatalf("openai group = %+v", openAIGroup)
	}
	if deepSeekGroup.PricedRequests != 0 || deepSeekGroup.UnpricedRequests != 1 {
		t.Fatalf("deepseek group coverage = %+v", deepSeekGroup)
	}
	if deepSeekGroup.EstimatedCostNanoUSD != 0 {
		t.Fatalf("deepseek cost = %d; aggregate sum is 0 but coverage must mark it unpriced", deepSeekGroup.EstimatedCostNanoUSD)
	}
	if deepSeekGroup.PromptTokens != 200 || deepSeekGroup.CompletionTokens != 100 || deepSeekGroup.TotalTokens != 300 {
		t.Fatalf("deepseek group tokens = %+v, want reported usage aggregated", deepSeekGroup)
	}
}
