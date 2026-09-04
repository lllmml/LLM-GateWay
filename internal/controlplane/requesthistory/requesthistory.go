// Package requesthistory exposes ownership-scoped gateway request history.
//
// Semantics (Week 7, ADR-016):
//   - Every read is scoped to the authenticated owner's projects. A request
//     that does not exist and a request that belongs to another owner are
//     indistinguishable (both return ErrNotFound).
//   - Ordering is fixed: started_at DESC, id DESC. Pages are fetched with a
//     keyset cursor over (started_at, id). Within a static dataset this is
//     deterministic with no duplicates or gaps; concurrent inserts keep
//     reasonable keyset behavior but no database snapshot isolation is
//     promised (no snapshot mechanism is introduced).
//   - next_cursor is an opaque server-generated token; decoding it back is
//     only possible through the API.
package requesthistory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("request not found")
	ErrInvalidParams = errors.New("invalid request parameters")
)

const (
	// DefaultPageSize is applied when limit is omitted.
	DefaultPageSize = 50
	// MaxPageSize bounds a single page.
	MaxPageSize = 200
	// MaxRangeDays bounds a from/to window when both bounds are supplied so
	// scans stay bounded; it is the same cap the usage endpoints enforce.
	MaxRangeDays = 90
	// MaxCursorTokenBytes bounds the encoded cursor to a sane size.
	MaxCursorTokenBytes = 2048
)

type Request struct {
	// The durable row also stores provider_credential_id (the FK to the
	// credential used upstream). It is intentionally NOT part of the public
	// request-history contract: it is an internal attribution FK, and the
	// virtual key + project identity already provide safe display attribution.
	// No other analytics/UI field references it, so the SQL select omits it.
	ID                   string     `json:"id"`
	ProjectID            string     `json:"project_id"`
	ProjectName          string     `json:"project_name"`
	VirtualKeyID         string     `json:"virtual_key_id"`
	VirtualKeyPrefix     string     `json:"virtual_key_prefix"`
	Provider             string     `json:"provider"`
	Model                string     `json:"model"`
	IsStream             bool       `json:"is_stream"`
	Status               string     `json:"status"`
	StartedAt            time.Time  `json:"started_at"`
	FirstChunkAt         *time.Time `json:"first_chunk_at"`
	CompletedAt          *time.Time `json:"completed_at"`
	LatencyMS            *int64     `json:"latency_ms"`
	TTFTMS               *int64     `json:"ttft_ms"`
	UpstreamHTTPStatus   *int32     `json:"upstream_http_status"`
	ErrorCategory        *string    `json:"error_category"`
	RetryCount           int16      `json:"retry_count"`
	PromptTokens         *int64     `json:"prompt_tokens"`
	CompletionTokens     *int64     `json:"completion_tokens"`
	TotalTokens          *int64     `json:"total_tokens"`
	UsageSource          *string    `json:"usage_source"`
	PricingID            *string    `json:"pricing_id"`
	EstimatedCostNanoUSD *int64     `json:"estimated_cost_nano_usd"`
	UpstreamRequestID    *string    `json:"upstream_request_id"`
	TraceID              *string    `json:"trace_id"`
	CreatedAt            time.Time  `json:"created_at"`
}

type Cursor struct {
	StartedAt time.Time
	ID        string
}

// ListParams carries validated, ownership-agnostic filters. ProjectID is a
// filter selector; ownership is enforced by the store through owner_user_id.
type ListParams struct {
	ProjectID string
	Provider  string
	Model     string
	Status    string
	Stream    *bool
	From      *time.Time
	To        *time.Time
	Cursor    *Cursor
	Limit     int // page size to return; <=0 means DefaultPageSize
}

type Page struct {
	Items      []Request `json:"items"`
	NextCursor *string   `json:"next_cursor"`
	HasMore    bool      `json:"has_more"`
}

// Store fetches the rows the service asks for (Limit+1, see Service.List).
type Store interface {
	ListRequests(context.Context, string, ListParams) ([]Request, error)
	GetRequest(context.Context, string, string) (Request, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// List returns one page of requests ordered by started_at DESC, id DESC. The
// store is asked for pageSize+1 rows: the extra row proves HasMore and is
// trimmed so pages never overlap or gap on a static dataset.
func (s *Service) List(ctx context.Context, ownerUserID string, params ListParams) (Page, error) {
	if err := validateListParams(&params); err != nil {
		return Page{}, err
	}
	pageSize := params.Limit
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	fetchParams := params
	fetchParams.Limit = pageSize + 1
	rows, err := s.store.ListRequests(ctx, ownerUserID, fetchParams)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Request{}}
	if len(rows) > pageSize {
		page.HasMore = true
		rows = rows[:pageSize]
	}
	page.Items = rows
	if page.HasMore {
		last := rows[len(rows)-1]
		token, err := encodeCursor(Cursor{StartedAt: last.StartedAt, ID: last.ID})
		if err != nil {
			return Page{}, err
		}
		page.NextCursor = &token
	}
	return page, nil
}

// Get returns one request for the owner; another owner's request and a
// nonexistent request both surface as ErrNotFound.
func (s *Service) Get(ctx context.Context, ownerUserID, requestID string) (Request, error) {
	return s.store.GetRequest(ctx, ownerUserID, requestID)
}

func validateListParams(params *ListParams) error {
	if params.ProjectID != "" && !isUUID(params.ProjectID) {
		return fmt.Errorf("%w: project_id must be a UUID", ErrInvalidParams)
	}
	if params.Provider != "" && !oneOf(params.Provider, "openai", "anthropic", "deepseek") {
		return fmt.Errorf("%w: provider must be openai, anthropic, or deepseek", ErrInvalidParams)
	}
	if params.Status != "" && !oneOf(params.Status, "in_progress", "succeeded", "failed") {
		return fmt.Errorf("%w: status must be in_progress, succeeded, or failed", ErrInvalidParams)
	}
	model := strings.TrimSpace(params.Model)
	if len(model) > 200 {
		return fmt.Errorf("%w: model is too long", ErrInvalidParams)
	}
	params.Model = model
	if params.Limit > MaxPageSize {
		return fmt.Errorf("%w: limit exceeds %d", ErrInvalidParams, MaxPageSize)
	}
	if params.Limit < 0 {
		return fmt.Errorf("%w: limit must be non-negative", ErrInvalidParams)
	}
	if params.From != nil && params.To != nil && !params.From.Before(*params.To) {
		return fmt.Errorf("%w: from must be before to", ErrInvalidParams)
	}
	if params.From != nil && params.To != nil && params.To.Sub(*params.From) > MaxRangeDays*24*time.Hour {
		return fmt.Errorf("%w: window exceeds %d days", ErrInvalidParams, MaxRangeDays)
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// isUUID reports whether value is a canonical lowercase hyphenated UUID, the
// form this API accepts for identifiers and filters. Validating at the domain
// boundary keeps the HTTP contract (400 invalid_request) independent of any
// store-level parse errors.
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

type cursorEnvelope struct {
	StartedAt time.Time `json:"started_at"`
	ID        string    `json:"id"`
}

// DecodeCursor parses an opaque cursor returned by the API. It is exported so
// the HTTP layer can turn the query parameter into a typed Cursor with the
// same validation the API guarantees.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" || len(token) > MaxCursorTokenBytes {
		return Cursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidParams)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidParams)
	}
	var envelope cursorEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Cursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidParams)
	}
	if envelope.StartedAt.IsZero() || strings.TrimSpace(envelope.ID) == "" {
		return Cursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidParams)
	}
	return Cursor{StartedAt: envelope.StartedAt.UTC(), ID: envelope.ID}, nil
}

func encodeCursor(cursor Cursor) (string, error) {
	raw, err := json.Marshal(cursorEnvelope{StartedAt: cursor.StartedAt.UTC(), ID: cursor.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
