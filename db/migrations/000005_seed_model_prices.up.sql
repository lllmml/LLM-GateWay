-- Seed the initial model price catalog (Week 7).
--
-- Data-only migration: no schema changes. Pricing is data, not code. Each row
-- is the CURRENT base-rate list price for a real catalog model that the
-- gateway routes; the effective_from column is the date the provider's price
-- actually took effect (from official provider announcements), NOT the date
-- this migration was written.
--
-- Semantics (ADR-016):
--   * effective_from <= request.started_at < effective_to; NULL effective_to
--     means the price is still in force.
--   * Prices are integer nano-USD per 1,000,000 tokens.
--   * These are BASE-RATE ESTIMATES. Providers may bill additional dimensions
--     (cached input, cache writes, long context, batch/fast, regional, time
--     tiers) that this catalog does not model.
--   * DeepSeek is intentionally absent: its 2026 pricing splits cache
--     hit/miss and peak/off-peak, and the Week 7 schema does not persist those
--     billing dimensions. Cache/time-tier-aware DeepSeek pricing is deferred.
--   * Providers do not publish intraday price-version start times; where an
--     announcement gives a date only, effective_from is that date at 00:00 UTC.
--
-- Verified 2026-09-05 against official provider pricing pages and
-- release/announcement notes (URLs per row in source_note).

INSERT INTO model_prices (id, provider, model, input_nano_usd_per_million, output_nano_usd_per_million, effective_from, effective_to, source_note) VALUES
-- OpenAI gpt-6-astra: $10.00 / $50.00 per MTok. Launched and priced at this
-- level per OpenAI changelog "Sep 3 ... Released GPT-6 Astra" (2026-09-03).
(
    'c0b4fd8e-5e6a-4a0a-8dc8-c946a039089e',
    'openai', 'gpt-6-astra',
    10000000000, 50000000000,
    '2026-09-03T00:00:00Z', NULL,
    'Official: platform.openai.com/docs/pricing (Standard, short context; snapshot 2026-09-05) and platform.openai.com/docs/changelog "Sep 3 Released GPT-6 Astra". Price effective from 2026-09-03 (announcement date, 00:00 UTC). Base-rate estimate only.'
),
-- OpenAI gpt-5.6-terra: current $2.00 / $12.00 per MTok after the 2026-07-30
-- price cut ("GPT-5.6 Terra costs 20% less") per OpenAI changelog.
(
    'bf814983-0ae3-43bb-8b5f-5c45063d4874',
    'openai', 'gpt-5.6-terra',
    2000000000, 12000000000,
    '2026-07-30T00:00:00Z', NULL,
    'Official: platform.openai.com/docs/pricing (Standard, short context; snapshot 2026-09-05) and changelog "Starting July 30 ... GPT-5.6 Terra costs 20% less". Price version effective from 2026-07-30 (00:00 UTC). Base-rate estimate only.'
),
-- OpenAI gpt-5.6-luna: current $0.20 / $1.20 per MTok after the 2026-07-30
-- price cut ("GPT-5.6 Luna costs 80% less") per OpenAI changelog.
(
    'f6294ec7-c98a-4d17-b63a-d1548b2d8e67',
    'openai', 'gpt-5.6-luna',
    200000000, 1200000000,
    '2026-07-30T00:00:00Z', NULL,
    'Official: platform.openai.com/docs/pricing (Standard, short context; snapshot 2026-09-05) and changelog "Starting July 30 ... GPT-5.6 Luna costs 80% less". Price version effective from 2026-07-30 (00:00 UTC). Base-rate estimate only.'
),
-- Anthropic claude-fable-5-1: $10.00 / $50.00 per MTok. Announced with
-- "Introducing Claude Fable 5.1 and Claude Mythos 5.1" (anthropic.com/news,
-- 2026-09-01). API ID from docs.anthropic.com/en/docs/about-claude/models/overview.
(
    '634f4c0f-a75b-47e2-8a06-d0194fb51054',
    'anthropic', 'claude-fable-5-1',
    10000000000, 50000000000,
    '2026-09-01T00:00:00Z', NULL,
    'Official: anthropic.com/news "Introducing Claude Fable 5.1 and Claude Mythos 5.1" (2026-09-01) and anthropic.com/pricing (snapshot 2026-09-05). Price effective from 2026-09-01 (00:00 UTC). Base-rate estimate only.'
),
-- Anthropic claude-opus-5: $5.00 / $25.00 per MTok. "Introducing Claude Opus 5"
-- (anthropic.com/news, 2026-07-24) states: priced at $5 per million input
-- tokens and $25 per million output tokens.
(
    'dd063952-1598-4911-b6d8-f1c2cb1ab1df',
    'anthropic', 'claude-opus-5',
    5000000000, 25000000000,
    '2026-07-24T00:00:00Z', NULL,
    'Official: anthropic.com/news/claude-opus-5 (2026-07-24, "priced at $5 per million input tokens and $25 per million output tokens") and anthropic.com/pricing (snapshot 2026-09-05). Price effective from 2026-07-24 (00:00 UTC). Base-rate estimate only.'
),
-- Anthropic claude-sonnet-5: $2.00 / $10.00 per MTok. Priced at this level at
-- launch (anthropic.com/news/claude-sonnet-5, 2026-06-30) and made PERMANENT on
-- 2026-08-10 per the article changelog ("Sonnet 5's introductory pricing of $2
-- per million input tokens and $10 per million output tokens is now permanent;
-- the standard pricing of $3 input / $15 output previously set to take effect
-- September 1 no longer applies"). No separate price version exists: the
-- numeric price never changed, so this single row is still in force.
-- API ID from docs.anthropic.com.
(
    'd1b2e13c-0d50-4ff7-af55-d12de65b2a13',
    'anthropic', 'claude-sonnet-5',
    2000000000, 10000000000,
    '2026-06-30T00:00:00Z', NULL,
    'Official: anthropic.com/news/claude-sonnet-5 (2026-06-30 launch price; article changelog 2026-08-10: $2/$10 made permanent, previously scheduled $3/$15 change cancelled) and anthropic.com/pricing (snapshot 2026-09-05). Price effective from 2026-06-30 (00:00 UTC). Base-rate estimate only.'
),
-- Anthropic claude-haiku-4-5 (API ID claude-haiku-4-5-20251001): $1.00 / $5.00
-- per MTok per anthropic.com/news/claude-haiku-4-5 (2025-10-15, "Pricing is
-- now $1/$5 per million input and output tokens"). API ID carries the
-- 2025-10-01 snapshot suffix per docs.anthropic.com models overview.
(
    'fed6e8b4-8f30-42eb-8e12-ae8526510b7b',
    'anthropic', 'claude-haiku-4-5-20251001',
    1000000000, 5000000000,
    '2025-10-15T00:00:00Z', NULL,
    'Official: anthropic.com/news/claude-haiku-4-5 (2025-10-15, "Pricing is now $1/$5 per million input and output tokens") and docs.anthropic.com/en/docs/about-claude/models/overview (API ID, snapshot 2026-09-05). Price effective from 2025-10-15 (00:00 UTC). Base-rate estimate only.'
);
