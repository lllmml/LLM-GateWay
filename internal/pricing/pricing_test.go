package pricing

import (
	"math"
	"testing"
)

func TestEstimateHappyPath(t *testing.T) {
	t.Parallel()

	// openai/gpt-5.6-terra: 2,000,000,000 nano-USD per million input,
	// 12,000,000,000 per million output.
	cases := []struct {
		name               string
		prompt, completion int64
		inNano, outNano    int64
		wantCost           int64
	}{
		{
			name:       "zero tokens",
			prompt:     0,
			completion: 0,
			inNano:     2_000_000_000,
			outNano:    12_000_000_000,
			wantCost:   0,
		},
		{
			name:       "mixed directions round cleanly",
			prompt:     1_000,
			completion: 2_000,
			inNano:     2_000_000_000,
			outNano:    12_000_000_000,
			// 1000*2e9/1e6 = 2_000_000; 2000*12e9/1e6 = 24_000_000
			wantCost: 26_000_000,
		},
		{
			name:       "single luna token is exact nano",
			prompt:     1,
			completion: 0,
			inNano:     200_000_000, // gpt-5.6-luna input: 1*2e8/1e6 = 200
			wantCost:   200,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Estimate(tc.prompt, tc.completion, tc.inNano, tc.outNano)
			if !ok {
				t.Fatalf("Estimate() ok=false, want true")
			}
			if got != tc.wantCost {
				t.Fatalf("Estimate() = %d, want %d", got, tc.wantCost)
			}
		})
	}
}

func TestEstimateTruncationFloorSemantics(t *testing.T) {
	t.Parallel()

	// price 1 nano-USD per million => a single token costs 1/1e6 nano-USD,
	// which floors to 0; 999_999 tokens floor to 0; 1_000_000 tokens => 1.
	in := int64(1)
	got, ok := Estimate(1, 0, in, 0)
	if !ok || got != 0 {
		t.Fatalf("1 token at 1 nano/MTok = %d, ok=%v; want 0,true", got, ok)
	}
	got, ok = Estimate(999_999, 0, in, 0)
	if !ok || got != 0 {
		t.Fatalf("999999 tokens at 1 nano/MTok = %d, ok=%v; want 0,true", got, ok)
	}
	got, ok = Estimate(1_000_000, 0, in, 0)
	if !ok || got != 1 {
		t.Fatalf("1e6 tokens at 1 nano/MTok = %d, ok=%v; want 1,true", got, ok)
	}
}

func TestEstimateOverflowFails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name               string
		prompt, completion int64
		inNano, outNano    int64
	}{
		{
			name:       "input overflow",
			prompt:     math.MaxInt64,
			completion: 0,
			inNano:     10_000_000_000,
			outNano:    0,
		},
		{
			name:       "output overflow",
			prompt:     0,
			completion: math.MaxInt64,
			inNano:     0,
			outNano:    50_000_000_000,
		},
		{
			name:       "large price overflows with modest tokens",
			prompt:     1_000_000_000_000,
			completion: 0,
			inNano:     50_000_000_000,
			outNano:    0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := Estimate(tc.prompt, tc.completion, tc.inNano, tc.outNano); ok {
				t.Fatalf("Estimate() ok=true, want false (overflow must be a calculation failure)")
			}
		})
	}
}

func TestEstimateRejectsNegativeInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name               string
		prompt, completion int64
		inNano, outNano    int64
	}{
		{name: "negative prompt", prompt: -1, completion: 0, inNano: 1, outNano: 1},
		{name: "negative completion", prompt: 0, completion: -5, inNano: 1, outNano: 1},
		{name: "negative input price", prompt: 1, completion: 0, inNano: -1, outNano: 1},
		{name: "negative output price", prompt: 0, completion: 1, inNano: 1, outNano: -1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := Estimate(tc.prompt, tc.completion, tc.inNano, tc.outNano); ok {
				t.Fatalf("Estimate() ok=true, want false for negative input")
			}
		})
	}
}

func TestEstimateMaxInt64WithoutOverflow(t *testing.T) {
	t.Parallel()

	// Boundary just below overflow: tokens * price must stay <= MaxInt64.
	tokens := int64(math.MaxInt64 / 1_000_000)
	got, ok := Estimate(tokens, tokens, 1_000_000, 1_000_000)
	if !ok {
		t.Fatalf("boundary Estimate() ok=false, want true")
	}
	want := tokens * 1_000_000 / 1_000_000 * 2
	if got != want {
		t.Fatalf("Estimate() = %d, want %d", got, want)
	}
}
