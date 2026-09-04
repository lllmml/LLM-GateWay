-- Remove the Week 7 seed price catalog. Reversible: deletes exactly the rows
-- inserted by 000005_seed_model_prices.up.sql, matched on the unique
-- (provider, model, effective_from) triple that the up migration used.
DELETE FROM model_prices
WHERE (provider, model, effective_from) IN (
    ('openai', 'gpt-6-astra', '2026-09-03T00:00:00Z'),
    ('openai', 'gpt-5.6-terra', '2026-07-30T00:00:00Z'),
    ('openai', 'gpt-5.6-luna', '2026-07-30T00:00:00Z'),
    ('anthropic', 'claude-fable-5-1', '2026-09-01T00:00:00Z'),
    ('anthropic', 'claude-opus-5', '2026-07-24T00:00:00Z'),
    ('anthropic', 'claude-sonnet-5', '2026-06-30T00:00:00Z'),
    ('anthropic', 'claude-haiku-4-5-20251001', '2025-10-15T00:00:00Z')
);
