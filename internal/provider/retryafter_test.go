package provider

import (
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		header  string
		present bool
		want    time.Duration
	}{
		{name: "missing header", header: "", present: false},
		{name: "blank header", header: "   ", present: false},
		{name: "delta seconds", header: "120", present: true, want: 120 * time.Second},
		{name: "zero delta", header: "0", present: true, want: 0},
		{name: "future http date", header: "Tue, 08 Sep 2026 12:02:00 GMT", present: true, want: 2 * time.Minute},
		{name: "past http date", header: "Tue, 08 Sep 2026 11:00:00 GMT", present: true, want: 0},
		{name: "exactly now http date", header: "Tue, 08 Sep 2026 12:00:00 GMT", present: true, want: 0},
		{name: "malformed delta", header: "-5", present: false},
		{name: "malformed delta plus sign", header: "+120", present: false},
		{name: "malformed text", header: "soon", present: false},
		{name: "fractional delta", header: "1.5", present: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseRetryAfter(test.header, now)
			if !test.present {
				if got != nil {
					t.Fatalf("ParseRetryAfter(%q) = %v, want nil", test.header, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseRetryAfter(%q) = nil, want present", test.header)
			}
			if *got != test.want {
				t.Fatalf("ParseRetryAfter(%q) = %v, want %v", test.header, *got, test.want)
			}
		})
	}
}

func TestParseRetryAfterDeterministicAgainstClock(t *testing.T) {
	// The same header must yield a stable result for a fixed now and never
	// depend on the wall clock.
	header := "Tue, 08 Sep 2026 12:05:00 GMT"
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	first := ParseRetryAfter(header, now)
	second := ParseRetryAfter(header, now)
	if first == nil || second == nil || *first != *second {
		t.Fatalf("ParseRetryAfter is not deterministic: %v vs %v", first, second)
	}
}
