package telemetry

import (
	"context"
	"strings"
	"testing"
)

func TestSanitizeClientRequestID(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{name: "plain", input: "req-123", want: "req-123", wantOK: true},
		{name: "trimmed", input: "  req-123  ", want: "req-123", wantOK: true},
		{name: "exactly bound", input: strings.Repeat("a", maxClientRequestIDBytes), want: strings.Repeat("a", maxClientRequestIDBytes), wantOK: true},
		{name: "over bound", input: strings.Repeat("a", maxClientRequestIDBytes+1), want: "", wantOK: false},
		{name: "control character", input: "req-\x01-abc", want: "", wantOK: false},
		{name: "newline", input: "req-\n-abc", want: "", wantOK: false},
		{name: "del", input: "req-\x7f-abc", want: "", wantOK: false},
		{name: "blank", input: "   ", want: "", wantOK: false},
		{name: "unicode length counts bytes", input: strings.Repeat("é", 101), want: "", wantOK: false},
	}
	for _, current := range cases {
		got, ok := SanitizeClientRequestID(current.input)
		if got != current.want || ok != current.wantOK {
			t.Errorf("%s: SanitizeClientRequestID(%q) = (%q, %v), want (%q, %v)",
				current.name, current.input, got, ok, current.want, current.wantOK)
		}
	}
}

func TestClientRequestIDContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := ClientRequestIDFromContext(ctx); ok {
		t.Fatal("background context unexpectedly carries a client request id")
	}
	ctx = WithClientRequestID(ctx, "req-1")
	if got, ok := ClientRequestIDFromContext(ctx); !ok || got != "req-1" {
		t.Fatalf("round trip = (%q, %v), want (req-1, true)", got, ok)
	}
}
