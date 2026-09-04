package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/requesthistory"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/usage"
)

// ListRequests implements requesthistory.Store. Ownership is enforced in SQL
// through owner_user_id joined on projects. The params.Limit already includes
// the +1 row the service needs to detect the next page.
func (s *Store) ListRequests(ctx context.Context, ownerUserID string, params requesthistory.ListParams) ([]requesthistory.Request, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return nil, requesthistory.ErrNotFound
	}
	var projectID pgtype.UUID
	if params.ProjectID != "" {
		projectID, err = parseUUID(params.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("parse project filter: %w", err)
		}
	}
	var cursorStartedAt pgtype.Timestamptz
	var cursorID pgtype.UUID
	if params.Cursor != nil {
		cursorStartedAt = timestamptz(params.Cursor.StartedAt)
		cursorID, err = parseUUID(params.Cursor.ID)
		if err != nil {
			return nil, requesthistory.ErrInvalidParams
		}
	}
	rows, err := s.queries.ListRequestsForOwner(ctx, ListRequestsForOwnerParams{
		OwnerUserID:     ownerID,
		ProjectID:       projectID,
		Provider:        optionalText(params.Provider),
		Model:           optionalText(params.Model),
		Status:          optionalText(params.Status),
		IsStream:        optionalBool(params.Stream),
		From:            optionalTimestamptz(params.From),
		To:              optionalTimestamptz(params.To),
		CursorStartedAt: cursorStartedAt,
		CursorID:        cursorID,
		Limit:           int32(params.Limit),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return []requesthistory.Request{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]requesthistory.Request, 0, len(rows))
	for _, row := range rows {
		items = append(items, requestRowFromListRow(row))
	}
	return items, nil
}

// GetRequest implements requesthistory.Store. Another owner's request and a
// nonexistent request are indistinguishable (both ErrNotFound).
func (s *Store) GetRequest(ctx context.Context, ownerUserID, requestID string) (requesthistory.Request, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return requesthistory.Request{}, requesthistory.ErrNotFound
	}
	id, err := parseUUID(requestID)
	if err != nil {
		return requesthistory.Request{}, requesthistory.ErrNotFound
	}
	row, err := s.queries.GetRequestForOwner(ctx, GetRequestForOwnerParams{OwnerUserID: ownerID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return requesthistory.Request{}, requesthistory.ErrNotFound
	}
	if err != nil {
		return requesthistory.Request{}, err
	}
	return requestRowFromListRow(ListRequestsForOwnerRow(row)), nil
}

func requestRowFromListRow(row ListRequestsForOwnerRow) requesthistory.Request {
	return requesthistory.Request{
		ID:                   formatUUID(row.ID),
		ProjectID:            formatUUID(row.ProjectID),
		ProjectName:          row.ProjectName,
		VirtualKeyID:         formatUUID(row.VirtualKeyID),
		VirtualKeyPrefix:     row.VirtualKeyPrefix,
		Provider:             row.Provider,
		Model:                row.Model,
		IsStream:             row.IsStream,
		Status:               row.Status,
		StartedAt:            row.StartedAt.Time.UTC(),
		FirstChunkAt:         optionalTimePtr(row.FirstChunkAt),
		CompletedAt:          optionalTimePtr(row.CompletedAt),
		LatencyMS:            int8Ptr(row.LatencyMs),
		TTFTMS:               int8Ptr(row.TtftMs),
		UpstreamHTTPStatus:   int4Ptr(row.UpstreamHttpStatus),
		ErrorCategory:        textPtr(row.ErrorCategory),
		RetryCount:           row.RetryCount,
		PromptTokens:         int8Ptr(row.PromptTokens),
		CompletionTokens:     int8Ptr(row.CompletionTokens),
		TotalTokens:          int8Ptr(row.TotalTokens),
		UsageSource:          textPtr(row.UsageSource),
		PricingID:            uuidPtr(row.PricingID),
		EstimatedCostNanoUSD: int8Ptr(row.EstimatedCostNanoUsd),
		UpstreamRequestID:    textPtr(row.UpstreamRequestID),
		TraceID:              textPtr(row.TraceID),
		CreatedAt:            row.CreatedAt.Time.UTC(),
	}
}

// UsageSummary implements usage.Store.
func (s *Store) UsageSummary(ctx context.Context, ownerUserID, projectID string, from, to time.Time) (usage.UsageSummaryRow, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return usage.UsageSummaryRow{}, fmt.Errorf("parse owner: %w", err)
	}
	filterID, err := optionalFilterUUID(projectID)
	if err != nil {
		return usage.UsageSummaryRow{}, err
	}
	row, err := s.queries.UsageSummaryForOwner(ctx, UsageSummaryForOwnerParams{
		OwnerUserID: ownerID,
		ProjectID:   filterID,
		From:        timestamptz(from),
		To:          timestamptz(to),
	})
	if err != nil {
		return usage.UsageSummaryRow{}, err
	}
	return usage.UsageSummaryRow{
		RequestsTotal:        row.RequestsTotal,
		RequestsSucceeded:    row.RequestsSucceeded,
		RequestsFailed:       row.RequestsFailed,
		PricedRequests:       row.PricedRequests,
		PromptTokens:         row.PromptTokens,
		CompletionTokens:     row.CompletionTokens,
		TotalTokens:          row.TotalTokens,
		EstimatedCostNanoUSD: row.EstimatedCostNanoUsd,
		LatencyMSSum:         row.LatencyMsSum,
		LatencyMSCount:       row.LatencyMsCount,
		TTFTMSSum:            row.TtftMsSum,
		TTFTMSCount:          row.TtftMsCount,
	}, nil
}

// UsageTimeseries implements usage.Store for the allowlisted day/hour buckets.
func (s *Store) UsageTimeseries(ctx context.Context, ownerUserID, projectID, bucket string, from, to time.Time) ([]usage.Point, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("parse owner: %w", err)
	}
	filterID, err := optionalFilterUUID(projectID)
	if err != nil {
		return nil, err
	}
	var points []usage.Point
	switch bucket {
	case "day":
		rows, err := s.queries.UsageTimeseriesForOwnerDay(ctx, UsageTimeseriesForOwnerDayParams{
			OwnerUserID: ownerID, ProjectID: filterID, From: timestamptz(from), To: timestamptz(to),
		})
		if err != nil {
			return nil, err
		}
		points = make([]usage.Point, 0, len(rows))
		for _, row := range rows {
			points = append(points, usage.Point{
				TS: row.Ts.Time.UTC(), RequestsTotal: row.RequestsTotal, RequestsSucceeded: row.RequestsSucceeded,
				RequestsFailed: row.RequestsFailed, PricedRequests: row.RequestsPriced, UnpricedRequests: row.RequestsUnpriced,
				PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
				TotalTokens: row.TotalTokens, EstimatedCostNanoUSD: row.EstimatedCostNanoUsd,
			})
		}
	case "hour":
		rows, err := s.queries.UsageTimeseriesForOwnerHour(ctx, UsageTimeseriesForOwnerHourParams{
			OwnerUserID: ownerID, ProjectID: filterID, From: timestamptz(from), To: timestamptz(to),
		})
		if err != nil {
			return nil, err
		}
		points = make([]usage.Point, 0, len(rows))
		for _, row := range rows {
			points = append(points, usage.Point{
				TS: row.Ts.Time.UTC(), RequestsTotal: row.RequestsTotal, RequestsSucceeded: row.RequestsSucceeded,
				RequestsFailed: row.RequestsFailed, PricedRequests: row.RequestsPriced, UnpricedRequests: row.RequestsUnpriced,
				PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
				TotalTokens: row.TotalTokens, EstimatedCostNanoUSD: row.EstimatedCostNanoUsd,
			})
		}
	default:
		return nil, usage.ErrInvalidParams
	}
	return points, nil
}

// UsageBreakdown implements usage.Store for the allowlisted provider/model/key
// dimensions.
func (s *Store) UsageBreakdown(ctx context.Context, ownerUserID, projectID, dimension string, from, to time.Time, limit int) ([]usage.Group, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("parse owner: %w", err)
	}
	filterID, err := optionalFilterUUID(projectID)
	if err != nil {
		return nil, err
	}
	switch dimension {
	case "provider":
		rows, err := s.queries.UsageBreakdownByProviderForOwner(ctx, UsageBreakdownByProviderForOwnerParams{
			OwnerUserID: ownerID, ProjectID: filterID, From: timestamptz(from), To: timestamptz(to), Limit: int32(limit),
		})
		if err != nil {
			return nil, err
		}
		groups := make([]usage.Group, 0, len(rows))
		for _, row := range rows {
			groups = append(groups, groupFromRow(row.Key, row.RequestsTotal, row.RequestsFailed, row.RequestsPriced, row.RequestsUnpriced, row.PromptTokens, row.CompletionTokens, row.TotalTokens, row.EstimatedCostNanoUsd))
		}
		return groups, nil
	case "model":
		rows, err := s.queries.UsageBreakdownByModelForOwner(ctx, UsageBreakdownByModelForOwnerParams{
			OwnerUserID: ownerID, ProjectID: filterID, From: timestamptz(from), To: timestamptz(to), Limit: int32(limit),
		})
		if err != nil {
			return nil, err
		}
		groups := make([]usage.Group, 0, len(rows))
		for _, row := range rows {
			groups = append(groups, groupFromRow(row.Key, row.RequestsTotal, row.RequestsFailed, row.RequestsPriced, row.RequestsUnpriced, row.PromptTokens, row.CompletionTokens, row.TotalTokens, row.EstimatedCostNanoUsd))
		}
		return groups, nil
	case "key":
		rows, err := s.queries.UsageBreakdownByKeyForOwner(ctx, UsageBreakdownByKeyForOwnerParams{
			OwnerUserID: ownerID, ProjectID: filterID, From: timestamptz(from), To: timestamptz(to), Limit: int32(limit),
		})
		if err != nil {
			return nil, err
		}
		groups := make([]usage.Group, 0, len(rows))
		for _, row := range rows {
			group := groupFromRow(row.KeyPrefix, row.RequestsTotal, row.RequestsFailed, row.RequestsPriced, row.RequestsUnpriced, row.PromptTokens, row.CompletionTokens, row.TotalTokens, row.EstimatedCostNanoUsd)
			keyID := formatUUID(row.KeyID)
			group.KeyID = &keyID
			group.KeyPrefix = &row.KeyPrefix
			groups = append(groups, group)
		}
		return groups, nil
	default:
		return nil, usage.ErrInvalidParams
	}
}

func groupFromRow(key string, total, failed, priced, unpriced, prompt, completion, totalTokens, cost int64) usage.Group {
	return usage.Group{
		Key: key, RequestsTotal: total, RequestsFailed: failed,
		PricedRequests: priced, UnpricedRequests: unpriced,
		PromptTokens: prompt, CompletionTokens: completion, TotalTokens: totalTokens,
		EstimatedCostNanoUSD: cost,
	}
}

func optionalFilterUUID(value string) (pgtype.UUID, error) {
	if value == "" {
		return pgtype.UUID{}, nil
	}
	id, err := parseUUID(value)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse project filter: %w", err)
	}
	return id, nil
}

func optionalBool(value *bool) pgtype.Bool {
	if value == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *value, Valid: true}
}

func optionalTimePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	utc := value.Time.UTC()
	return &utc
}

func int8Ptr(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func int4Ptr(value pgtype.Int4) *int32 {
	if !value.Valid {
		return nil
	}
	return &value.Int32
}

func textPtr(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func uuidPtr(value pgtype.UUID) *string {
	if !value.Valid {
		return nil
	}
	formatted := formatUUID(value)
	return &formatted
}
