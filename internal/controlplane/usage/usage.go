// Package usage exposes ownership-scoped usage and cost aggregation.
//
// Semantics (Week 7, ADR-016):
//   - Time is UTC. Windows are half-open [from, to). When the caller omits the
//     bounds the service applies the default 30-day window; the maximum span is
//     90 days.
//   - Timeseries buckets use generate_series aligned to date_trunc(bucket,
//     from) computed in explicit UTC (independent of the PostgreSQL session
//     TimeZone), so the first/last buckets may be partial and empty buckets
//     are zero-filled server-side. from does not need to be on a bucket
//     boundary.
//   - estimated_cost_nano_usd is the BASE-RATE estimate aggregated over the
//     requests that recorded a cost (usage + price version available). It is
//     not an invoice and not the full bill. Token sums include every reported
//     non-null usage regardless of pricing; priced_requests /
//     unpriced_requests expose cost completeness so an unpriced group or
//     bucket is never presented as zero cost.
//   - Breakdown dimensions are allowlisted: provider, model, key.
package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidParams = errors.New("invalid usage parameters")
)

const (
	// DefaultWindow is applied when from/to are both omitted.
	DefaultWindow = 30 * 24 * time.Hour
	// MaxWindow caps a from/to span.
	MaxWindow = 90 * 24 * time.Hour
	// DefaultBreakdownLimit and MaxBreakdownLimit bound breakdown groups.
	DefaultBreakdownLimit = 20
	MaxBreakdownLimit     = 100
)

type Summary struct {
	From                 time.Time `json:"from"`
	To                   time.Time `json:"to"`
	RequestsTotal        int64     `json:"requests_total"`
	RequestsSucceeded    int64     `json:"requests_succeeded"`
	RequestsFailed       int64     `json:"requests_failed"`
	PricedRequests       int64     `json:"priced_requests"`
	UnpricedRequests     int64     `json:"unpriced_requests"`
	ErrorRate            *float64  `json:"error_rate"`
	PromptTokens         int64     `json:"prompt_tokens"`
	CompletionTokens     int64     `json:"completion_tokens"`
	TotalTokens          int64     `json:"total_tokens"`
	EstimatedCostNanoUSD int64     `json:"estimated_cost_nano_usd"`
	AvgLatencyMS         *float64  `json:"avg_latency_ms"`
	AvgTTFTMS            *float64  `json:"avg_ttft_ms"`
	GeneratedAt          time.Time `json:"generated_at"`
}

type Point struct {
	TS                   time.Time `json:"ts"`
	RequestsTotal        int64     `json:"requests_total"`
	RequestsSucceeded    int64     `json:"requests_succeeded"`
	RequestsFailed       int64     `json:"requests_failed"`
	PricedRequests       int64     `json:"priced_requests"`
	UnpricedRequests     int64     `json:"unpriced_requests"`
	PromptTokens         int64     `json:"prompt_tokens"`
	CompletionTokens     int64     `json:"completion_tokens"`
	TotalTokens          int64     `json:"total_tokens"`
	EstimatedCostNanoUSD int64     `json:"estimated_cost_nano_usd"`
}

type Group struct {
	Key                  string  `json:"key"`
	KeyID                *string `json:"key_id,omitempty"`
	KeyPrefix            *string `json:"key_prefix,omitempty"`
	RequestsTotal        int64   `json:"requests_total"`
	RequestsFailed       int64   `json:"requests_failed"`
	PricedRequests       int64   `json:"priced_requests"`
	UnpricedRequests     int64   `json:"unpriced_requests"`
	PromptTokens         int64   `json:"prompt_tokens"`
	CompletionTokens     int64   `json:"completion_tokens"`
	TotalTokens          int64   `json:"total_tokens"`
	EstimatedCostNanoUSD int64   `json:"estimated_cost_nano_usd"`
}

// Store is implemented by the postgres store; ownership is enforced through
// owner_user_id. from/to are always supplied by the service (validated).
type Store interface {
	UsageSummary(context.Context, string, string, time.Time, time.Time) (UsageSummaryRow, error)
	UsageTimeseries(context.Context, string, string, string, time.Time, time.Time) ([]Point, error)
	UsageBreakdown(context.Context, string, string, string, time.Time, time.Time, int) ([]Group, error)
}

// UsageSummaryRow is the raw aggregate before derived fields (error_rate,
// unpriced_requests) are computed by the service.
type UsageSummaryRow struct {
	RequestsTotal        int64
	RequestsSucceeded    int64
	RequestsFailed       int64
	PricedRequests       int64
	PromptTokens         int64
	CompletionTokens     int64
	TotalTokens          int64
	EstimatedCostNanoUSD int64
	LatencyMSSum         int64
	LatencyMSCount       int64
	TTFTMSSum            int64
	TTFTMSCount          int64
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// Summary returns aggregates over [from, to). Empty windows return zero
// aggregates with error_rate and averages null.
func (s *Service) Summary(ctx context.Context, ownerUserID, projectID string, from, to *time.Time) (Summary, error) {
	if projectID != "" && !isUUID(projectID) {
		return Summary{}, fmt.Errorf("%w: project_id must be a UUID", ErrInvalidParams)
	}
	fromTime, toTime, err := window(from, to)
	if err != nil {
		return Summary{}, err
	}
	row, err := s.store.UsageSummary(ctx, ownerUserID, projectID, fromTime, toTime)
	if err != nil {
		return Summary{}, err
	}
	var errorRate *float64
	if row.RequestsTotal > 0 {
		rate := float64(row.RequestsFailed) / float64(row.RequestsTotal)
		errorRate = &rate
	}
	return Summary{
		From:                 fromTime,
		To:                   toTime,
		RequestsTotal:        row.RequestsTotal,
		RequestsSucceeded:    row.RequestsSucceeded,
		RequestsFailed:       row.RequestsFailed,
		PricedRequests:       row.PricedRequests,
		UnpricedRequests:     row.RequestsSucceeded - row.PricedRequests,
		ErrorRate:            errorRate,
		PromptTokens:         row.PromptTokens,
		CompletionTokens:     row.CompletionTokens,
		TotalTokens:          row.TotalTokens,
		EstimatedCostNanoUSD: row.EstimatedCostNanoUSD,
		AvgLatencyMS:         average(row.LatencyMSSum, row.LatencyMSCount),
		AvgTTFTMS:            average(row.TTFTMSSum, row.TTFTMSCount),
		GeneratedAt:          time.Now().UTC(),
	}, nil
}

func average(sum, count int64) *float64 {
	if count <= 0 {
		return nil
	}
	avg := float64(sum) / float64(count)
	return &avg
}

// Timeseries returns zero-filled buckets aligned to date_trunc(bucket, from).
// The number of points equals the number of whole-or-partial buckets covering
// [from, to); an empty range yields no points.
func (s *Service) Timeseries(ctx context.Context, ownerUserID, projectID string, bucket string, from, to *time.Time) ([]Point, error) {
	if projectID != "" && !isUUID(projectID) {
		return nil, fmt.Errorf("%w: project_id must be a UUID", ErrInvalidParams)
	}
	if bucket != "hour" && bucket != "day" {
		return nil, fmt.Errorf("%w: bucket must be hour or day", ErrInvalidParams)
	}
	fromTime, toTime, err := window(from, to)
	if err != nil {
		return nil, err
	}
	if !fromTime.Before(toTime) {
		return []Point{}, nil
	}
	points, err := s.store.UsageTimeseries(ctx, ownerUserID, projectID, bucket, fromTime, toTime)
	if err != nil {
		return nil, err
	}
	if points == nil {
		points = []Point{}
	}
	return points, nil
}

// Breakdown returns groups for one allowlisted dimension ordered by estimated
// cost descending.
func (s *Service) Breakdown(ctx context.Context, ownerUserID, projectID, dimension string, from, to *time.Time, limit int) ([]Group, error) {
	if projectID != "" && !isUUID(projectID) {
		return nil, fmt.Errorf("%w: project_id must be a UUID", ErrInvalidParams)
	}
	if dimension != "provider" && dimension != "model" && dimension != "key" {
		return nil, fmt.Errorf("%w: dimension must be provider, model, or key", ErrInvalidParams)
	}
	if limit <= 0 {
		limit = DefaultBreakdownLimit
	}
	if limit > MaxBreakdownLimit {
		return nil, fmt.Errorf("%w: limit exceeds %d", ErrInvalidParams, MaxBreakdownLimit)
	}
	fromTime, toTime, err := window(from, to)
	if err != nil {
		return nil, err
	}
	groups, err := s.store.UsageBreakdown(ctx, ownerUserID, projectID, dimension, fromTime, toTime, limit)
	if err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, nil
}

// EffectiveWindow normalizes an optional half-open UTC window: when from/to
// are omitted it applies the 30-day default, and it enforces from < to and the
// 90-day maximum span. Handlers and the service share this so the reported
// window in a response always matches the window used for the query.
func EffectiveWindow(from, to *time.Time) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	fromTime := now.Add(-DefaultWindow)
	toTime := now
	if from != nil {
		fromTime = from.UTC()
	}
	if to != nil {
		toTime = to.UTC()
	}
	if !fromTime.Before(toTime) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: from must be before to", ErrInvalidParams)
	}
	if toTime.Sub(fromTime) > MaxWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: window exceeds 90 days", ErrInvalidParams)
	}
	return fromTime, toTime, nil
}

func window(from, to *time.Time) (time.Time, time.Time, error) {
	return EffectiveWindow(from, to)
}

// isUUID reports whether value is a canonical hyphenated UUID, validated at
// the domain boundary so a malformed project_id filter is a 400 invalid_request
// regardless of the store implementation.
func isUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for _, char := range value {
		if char == '-' {
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}
