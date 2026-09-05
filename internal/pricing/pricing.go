// Package pricing computes base-rate estimated costs for gateway requests.
//
// Semantics (Week 7, ADR-016):
//   - Prices are integer nano-USD per 1,000,000 tokens, stored in model_prices.
//   - Cost = prompt_tokens*input_nano_per_million/1_000_000
//   - completion_tokens*output_nano_per_million/1_000_000,
//     computed entirely in non-negative int64 with floor (truncation toward
//     zero) division. Each direction loses less than 1 nano-USD per request.
//   - The result is a BASE-RATE ESTIMATE, not an invoice or billing source of
//     truth. Providers may additionally bill cached-input, cache-write,
//     long-context, batch/fast/regional, or time-tier dimensions that this
//     package does not model.
//   - Multiplications are overflow-checked. An overflow (or a negative input,
//     which must never occur from validated adapters) is a calculation
//     failure: it returns ok=false and the caller records the matched
//     pricing_id with a NULL estimated cost rather than fabricating a value.
package pricing

import "math"

// Estimate returns the base-rate estimated cost in integer nano-USD for a
// request with the given non-negative token counts against a price version
// expressed in nano-USD per million tokens. ok=false means the inputs are
// invalid (negative) or the intermediate multiplication overflows int64; in
// that case the caller must not fabricate a cost.
func Estimate(promptTokens, completionTokens, inputNanoPerMillion, outputNanoPerMillion int64) (nanoUSD int64, ok bool) {
	if promptTokens < 0 || completionTokens < 0 || inputNanoPerMillion < 0 || outputNanoPerMillion < 0 {
		return 0, false
	}
	inputCost, ok := scaled(promptTokens, inputNanoPerMillion)
	if !ok {
		return 0, false
	}
	outputCost, ok := scaled(completionTokens, outputNanoPerMillion)
	if !ok {
		return 0, false
	}
	// Correct bound: the overflow guard ensures tokens*nanoPerMillion <= MaxInt64
	// before division, so each direction is <= MaxInt64/1_000_000 (~9.2e12).
	// Two such values sum to ~1.8e13, far below MaxInt64 (~9.2e18), so the
	// final addition cannot overflow int64.
	return inputCost + outputCost, true
}

// scaled computes (tokens * nanoPerMillion) / 1_000_000 with floor division
// and an explicit overflow guard. All inputs must be non-negative.
func scaled(tokens, nanoPerMillion int64) (int64, bool) {
	if nanoPerMillion != 0 && tokens > math.MaxInt64/nanoPerMillion {
		return 0, false
	}
	return tokens * nanoPerMillion / 1_000_000, true
}
