# ADR-016: Pricing versions, estimated cost, and analytics (Week 7)

- Status: Accepted
- Date: 2026-09-05
- Related: Tech Design §18-19; Week 7 roadmap entry; migration `000005_seed_model_prices`

## Context

The gateway attributes durable request lifecycle rows to project, virtual key,
credential, provider, and model. To answer "where does spend come from" the
MVP needs price-versioned estimated cost plus ownership-scoped request history
and usage aggregation, surfaced in Overview / Usage / Requests screens.

Week 7 constraints: keep the closed loop small, use integer money, never let
pricing problems affect gateway traffic, and stay honest about what an
"estimated cost" can mean while the persisted billing dimensions are coarse.

## Options considered

1. Hard-code per-model prices in Go.
2. Seed a `model_prices` catalog with `effective_from = snapshot date`.
3. Treat `estimated_cost` as authoritative cost for all providers, including
   DeepSeek, using an upper-bound price.
4. This design: versioned seed data with real effective dates, base-rate
   estimate semantics, and no DeepSeek cost in Week 7.

Option 1 couples request handling to volatile pricing. Option 2 fabricates
effective dates. Option 3 would mix a deliberate upper bound into the same
dashboard metric as other providers' standard prices.

## Decision

### Pricing data

- Prices live in `model_prices` (schema from 000004) as integer nano-USD per
  1,000,000 tokens with an effective window. Catalog is data, not code.
- Window semantics are fixed: `effective_from <= request.started_at <
  effective_to`; `effective_to IS NULL` means still in force. The pricing
  lookup time is `gateway_requests.started_at`, so a mid-request price change
  never changes a request's attribution.
- `effective_from` is the date the provider's price actually took effect,
  derived from official provider announcements, not the date this project
  wrote the migration. Providers do not publish intraday price-version start
  times, so a date-only announcement is recorded at 00:00 UTC and that
  convention is noted in each row's `source_note`. Where a row's real
  effective date could not be determined, the row was not included.
- Overlapping windows are a catalog data-quality error, never silently picked:
  the query deterministically resolves to the most recently effective version
  (unique `(provider, model, effective_from)` makes it total), and a catalog
  validator test asserts the seed has no overlaps. The only catalog write path
  today is migrations; a future pricing admin must close the previous window
  (set `effective_to`) when inserting a replacement and reuse the validator.

### Estimated cost semantics

- `estimated_cost_nano_usd` is a BASE-RATE ESTIMATE, not an invoice and not
  the full bill. Providers may bill additional dimensions (cached input,
  cache writes, long context, batch/fast processing, regional uplift, time
  tiers) that the Week 7 schema does not persist. API responses, UI copy, and
  this ADR all say "estimated base-rate cost".
- Integer arithmetic: `prompt_tokens * in_nano_per_million / 1_000_000 +
  completion_tokens * out_nano_per_million / 1_000_000`, floor division per
  direction (error < 1 nano-USD per request per direction). Multiplication is
  overflow-checked; overflow or negative inputs are a calculation failure.
- `pricing_id` is provenance, `estimated_cost` is a derived result, and they
  are intentionally NOT bound by an iff invariant:

  | Scenario (succeeded request) | pricing_id | estimated_cost |
  |---|---|---|
  | No effective price version | NULL | NULL |
  | Price found, usage complete, calculation ok | matched | calculated |
  | Price found, usage missing or calculation failed | matched | NULL |
  | Failed request | NULL | NULL (never priced) |

- Pricing lookup or calculation problems degrade to NULL plus a bounded log
  line and never fail a gateway request; only the final durable row write can
  surface the existing `usage_persistence_failed` semantics.

### DeepSeek (deliberate limitation)

DeepSeek 2026 pricing splits cache hit/miss and peak/off-peak, and the Week 7
schema persists only prompt/completion/total tokens. Week 7 therefore does not
compute DeepSeek estimated cost and does not seed DeepSeek price versions: the
lookup misses, cost stays NULL, usage is still recorded. Using a peak/cache-miss
upper bound would pollute the shared cost metric with a number that is not
comparable to the other providers' standard prices. Cache-aware / time-tier-aware
exact pricing is a later iteration (schema + adapter changes to persist the
missing billing dimensions); no schema is changed for it this week.

### Analytics contract

- Time is UTC. Windows are half-open `[from, to)`. Usage endpoints default to a
  30-day window and enforce a 90-day maximum span.
- Timeseries buckets align to `date_trunc(bucket, from)` via `generate_series`
  and are computed in explicit UTC (`AT TIME ZONE 'UTC'` on both the series and
  the aggregated `started_at`), so results never depend on the PostgreSQL
  session TimeZone. First and last buckets may be partial; empty buckets are
  zero-filled server-side. `from` need not sit on a bucket boundary.
- Request history is ordered `started_at DESC, id DESC` with an opaque keyset
  cursor. On a static dataset pagination is deterministic with no duplicates or
  gaps; concurrent inserts keep reasonable keyset behavior but no database
  snapshot isolation is promised (no snapshot mechanism is introduced).
- Aggregation semantics: request counts include every matching row; token sums
  include every reported non-null usage regardless of pricing; estimated cost
  sums only succeed rows with an attributed cost. `priced_requests` /
  `unpriced_requests` coverage (summary, timeseries, and breakdown) explains
  cost completeness so an unpriced bucket/group is never shown as $0.00.
- Breakdown dimensions are an explicit allowlist: `provider`, `model`, `key`.
- Every analytics query is ownership-scoped in SQL through
  `projects.owner_user_id`. Another owner's request and a nonexistent request
  are indistinguishable (404).
- The seeded catalog covers only models this gateway routes today with prices
  verified 2026-09-05 from official sources: OpenAI `gpt-6-astra` (2026-09-03),
  `gpt-5.6-terra`/`gpt-5.6-luna` (2026-07-30 price cut), Anthropic
  `claude-fable-5-1` (2026-09-01), `claude-opus-5` (2026-07-24),
  `claude-sonnet-5` (2026-06-30; $2/$10 made permanent 2026-08-10 per the
  official article changelog), `claude-haiku-4-5-20251001` (2025-10-15). Test
  fixtures (`gpt-test`, `deepseek-chat`, `claude-sonnet-x`) are not in the
  seed.

## Consequences

- Cost numbers in the console are clearly framed as base-rate estimates.
- DeepSeek requests show no cost until a later schema iteration lands.
- Catalog price changes require a new migration that closes the old window and
  re-runs the overlap validator.
- Sonnet 5's $2/$10 price is permanent: the official article changelog
  (2026-08-10) cancelled the previously scheduled $3/$15 change, so no window
  close is needed. A genuine future price change still requires a new row that
  closes this window.
- Requests whose model has no seed row still succeed; they are simply
  unpriced.

## How to revisit

- Revisit the DeepSeek deferral when cache/time-tier billing dimensions are
  persisted and priced.
- Revisit percentile latency and richer breakdown composition only when
  measured need and index evidence justify them (Observability Week scope).
- Revisit catalog coverage before public launch: re-verify each provider price
  page, model ID, and effective date.
