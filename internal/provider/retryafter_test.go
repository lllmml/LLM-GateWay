package provider

import (
	"math"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	maxDuration := time.Duration(math.MaxInt64/int64(time.Second)) * time.Second
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
		{name: "future http date", header: "Sat, 05 Sep 2026 12:02:00 GMT", present: true, want: 2 * time.Minute},
		{name: "past http date", header: "Sat, 05 Sep 2026 11:00:00 GMT", present: true, want: 0},
		{name: "exactly now http date", header: "Sat, 05 Sep 2026 12:00:00 GMT", present: true, want: 0},
		{name: "malformed delta", header: "-5", present: false},
		{name: "malformed delta plus sign", header: "+120", present: false},
		{name: "malformed text", header: "soon", present: false},
		{name: "fractional delta", header: "1.5", present: false},
		{name: "largest representable delta", header: "9223372036", present: true, want: maxDuration},
		{name: "one beyond duration boundary clamps", header: "9223372037", present: true, want: maxDuration},
		{name: "far beyond duration boundary clamps", header: "18446744073709551615", present: true, want: maxDuration},
		{name: "extremely long numeric string clamps", header: "999999999999999999999999999999999999", present: true, want: maxDuration},
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
			if *got < 0 {
				t.Fatalf("ParseRetryAfter(%q) produced a negative duration %v", test.header, *got)
			}
		})
	}
}

func TestParseRetryAfterDeterministicAgainstClock(t *testing.T) {
	// The same header must yield a stable result for a fixed now and never
	// depend on the wall clock.
	header := "Sat, 05 Sep 2026 12:05:00 GMT"
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	first := ParseRetryAfter(header, now)
	second := ParseRetryAfter(header, now)
	if first == nil || second == nil || *first != *second {
		t.Fatalf("ParseRetryAfter is not deterministic: %v vs %v", first, second)
	}
}
