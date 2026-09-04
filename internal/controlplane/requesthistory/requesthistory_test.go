package requesthistory

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type stubRequestStore struct {
	requests []Request
}

func (s *stubRequestStore) ListRequests(_ context.Context, _ string, params ListParams) ([]Request, error) {
	if params.Cursor == nil {
		limit := params.Limit
		if limit > len(s.requests) {
			limit = len(s.requests)
		}
		return s.requests[:limit], nil
	}
	// Emulate the keyset predicate: return rows strictly after (started_at, id)
	// in DESC order, capped at the requested limit.
	out := []Request{}
	for _, item := range s.requests {
		if item.StartedAt.Before(params.Cursor.StartedAt) ||
			(item.StartedAt.Equal(params.Cursor.StartedAt) && item.ID < params.Cursor.ID) {
			out = append(out, item)
			if len(out) == params.Limit {
				break
			}
		}
	}
	return out, nil
}

func (s *stubRequestStore) GetRequest(context.Context, string, string) (Request, error) {
	return Request{}, ErrNotFound
}

func sampleRequest(startedAt time.Time, id string) Request {
	return Request{ID: id, Status: "succeeded", Provider: "openai", Model: "gpt-5.6-terra", StartedAt: startedAt, CreatedAt: startedAt}
}

func sortedRequests(n int) []Request {
	// Simulates the store contract: started_at DESC, id DESC.
	items := make([]Request, n)
	for i := 0; i < n; i++ {
		started := time.Date(2026, 9, 1, 0, 0, n-i, 0, time.UTC)
		items[i] = sampleRequest(started, "00000000-0000-4000-8000-00000000"+pad(n-i))
	}
	return items
}

func pad(value int) string {
	digits := fmt.Sprintf("%d", value)
	return strings.Repeat("0", 8-len(digits)) + digits
}

func TestListTrimsToPageSizeAndChainsCursorWithoutDupOrGap(t *testing.T) {
	t.Parallel()
	store := &stubRequestStore{requests: sortedRequests(5)}
	service := NewService(store)

	first, err := service.List(context.Background(), "owner", ListParams{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if !first.HasMore || first.NextCursor == nil || len(first.Items) != 2 {
		t.Fatalf("first page = %+v", first)
	}
	cursor, err := DecodeCursor(*first.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	second, err := service.List(context.Background(), "owner", ListParams{Limit: 2, Cursor: &cursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if !second.HasMore || len(second.Items) != 2 {
		t.Fatalf("second page = %+v", second)
	}
	cursor, err = DecodeCursor(*second.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor 2: %v", err)
	}
	third, err := service.List(context.Background(), "owner", ListParams{Limit: 2, Cursor: &cursor})
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	if third.HasMore || third.NextCursor != nil || len(third.Items) != 1 {
		t.Fatalf("third page = %+v", third)
	}

	seen := map[string]bool{}
	for _, page := range []Page{first, second, third} {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate id %s across pages", item.ID)
			}
			seen[item.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("covered %d unique ids, want 5", len(seen))
	}
}

func TestListDefaultsPageSizeAndReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	store := &stubRequestStore{requests: []Request{}}
	page, err := NewService(store).List(context.Background(), "owner", ListParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.HasMore || page.NextCursor != nil {
		t.Fatalf("empty page = %+v", page)
	}
}

func TestDecodeCursorRejectsMalformedTokens(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"%%%not-base64%%%",
		base64.RawURLEncoding.EncodeToString([]byte(`{"id":"x"}`)),                            // missing started_at
		base64.RawURLEncoding.EncodeToString([]byte(`{"started_at":"2026-09-01T00:00:00Z"}`)), // missing id
		base64.RawURLEncoding.EncodeToString([]byte(`not json`)),
	}
	for _, token := range cases {
		if _, err := DecodeCursor(token); err == nil {
			t.Fatalf("DecodeCursor(%q) succeeded, want error", token)
		}
	}
	if _, err := DecodeCursor(strings.Repeat("A", MaxCursorTokenBytes+1)); err == nil {
		t.Fatal("oversized cursor accepted")
	}
}

func TestDecodeCursorRoundTripsMicrosecondPrecision(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 1, 13, 37, 42, 123456000, time.UTC)
	token, err := encodeCursor(Cursor{StartedAt: started, ID: "00000000-0000-4000-8000-000000000009"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeCursor(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.StartedAt.Equal(started) || decoded.ID != "00000000-0000-4000-8000-000000000009" {
		t.Fatalf("round trip = %+v, want %+v", decoded, started)
	}
}

func TestValidateListParamsRejectsBadInput(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cases := []struct {
		name   string
		params ListParams
	}{
		{name: "provider", params: ListParams{Provider: "gemini"}},
		{name: "status", params: ListParams{Status: "cancelled"}},
		{name: "limit over max", params: ListParams{Limit: MaxPageSize + 1}},
		{name: "limit negative", params: ListParams{Limit: -1}},
		{name: "from after to", params: ListParams{From: &now, To: &now}},
		{name: "window over max", params: ListParams{From: timePtr(now.Add(-(MaxRangeDays + 1) * 24 * time.Hour)), To: &now}},
		{name: "long model", params: ListParams{Model: strings.Repeat("m", 201)}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := validateListParams(&tc.params); err == nil {
				t.Fatal("validateListParams succeeded, want error")
			}
		})
	}
	if err := validateListParams(&ListParams{Provider: "openai", Status: "succeeded"}); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
}

func timePtr(value time.Time) *time.Time { return &value }

func TestStoreErrorPassesThrough(t *testing.T) {
	t.Parallel()
	store := &stubRequestStore{}
	service := NewService(store)
	if _, err := service.Get(context.Background(), "owner", "00000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get error = %v, want ErrNotFound", err)
	}
}
