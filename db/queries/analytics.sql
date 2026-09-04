-- Control Plane analytics queries (Week 7).
--
-- Every query is ownership-scoped: the caller supplies owner_user_id and rows
-- are joined through projects.owner_user_id. A browser-supplied project_id is
-- a filter selector, never proof of access.
--
-- Time semantics: [from, to) half-open intervals, UTC. Timeseries buckets use
-- generate_series aligned to date_trunc(bucket, from); the first and last
-- buckets may be partial. Zero-fill is server-side via LEFT JOIN + COALESCE.
--
-- Token sums include EVERY reported non-null usage, independent of whether a
-- price version exists. estimated_cost_nano_usd aggregates only base-rate
-- attributed costs (succeeded rows with pricing_id and a recorded cost);
-- priced/unpriced request counts expose cost completeness separately.
--
-- The durable gateway_requests row carries provider_credential_id, an
-- internal attribution FK. It is intentionally NOT selected here: the public
-- request-history contract exposes virtual-key and project identity for safe
-- display attribution and no analytics path references the credential FK.

-- name: ListRequestsForOwner :many
SELECT
    r.id,
    r.project_id,
    r.virtual_key_id,
    r.provider,
    r.model,
    r.is_stream,
    r.status,
    r.started_at,
    r.first_chunk_at,
    r.completed_at,
    r.latency_ms,
    r.ttft_ms,
    r.upstream_http_status,
    r.error_category,
    r.retry_count,
    r.prompt_tokens,
    r.completion_tokens,
    r.total_tokens,
    r.usage_source,
    r.pricing_id,
    r.estimated_cost_nano_usd,
    r.upstream_request_id,
    r.trace_id,
    r.created_at,
    p.name AS project_name,
    k.key_prefix AS virtual_key_prefix
FROM gateway_requests AS r
JOIN projects AS p
  ON p.id = r.project_id
 AND p.owner_user_id = sqlc.arg('owner_user_id')
JOIN virtual_api_keys AS k
  ON k.id = r.virtual_key_id
WHERE (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid)
  AND (sqlc.narg('provider')::text IS NULL OR r.provider = sqlc.narg('provider')::text)
  AND (sqlc.narg('model')::text IS NULL OR r.model = sqlc.narg('model')::text)
  AND (sqlc.narg('status')::text IS NULL OR r.status = sqlc.narg('status')::text)
  AND (sqlc.narg('is_stream')::boolean IS NULL OR r.is_stream = sqlc.narg('is_stream')::boolean)
  AND (sqlc.narg('from')::timestamptz IS NULL OR r.started_at >= sqlc.narg('from')::timestamptz)
  AND (sqlc.narg('to')::timestamptz IS NULL OR r.started_at < sqlc.narg('to')::timestamptz)
  AND (
      sqlc.narg('cursor_started_at')::timestamptz IS NULL
      OR r.started_at < sqlc.narg('cursor_started_at')::timestamptz
      OR (r.started_at = sqlc.narg('cursor_started_at')::timestamptz AND r.id < sqlc.narg('cursor_id')::uuid)
  )
ORDER BY r.started_at DESC, r.id DESC
LIMIT sqlc.arg('limit');

-- name: GetRequestForOwner :one
SELECT
    r.id,
    r.project_id,
    r.virtual_key_id,
    r.provider,
    r.model,
    r.is_stream,
    r.status,
    r.started_at,
    r.first_chunk_at,
    r.completed_at,
    r.latency_ms,
    r.ttft_ms,
    r.upstream_http_status,
    r.error_category,
    r.retry_count,
    r.prompt_tokens,
    r.completion_tokens,
    r.total_tokens,
    r.usage_source,
    r.pricing_id,
    r.estimated_cost_nano_usd,
    r.upstream_request_id,
    r.trace_id,
    r.created_at,
    p.name AS project_name,
    k.key_prefix AS virtual_key_prefix
FROM gateway_requests AS r
JOIN projects AS p
  ON p.id = r.project_id
 AND p.owner_user_id = sqlc.arg('owner_user_id')
JOIN virtual_api_keys AS k
  ON k.id = r.virtual_key_id
WHERE r.id = sqlc.arg('id');

-- name: UsageSummaryForOwner :one
SELECT
    count(*)::bigint AS requests_total,
    count(*) FILTER (WHERE r.status = 'succeeded')::bigint AS requests_succeeded,
    count(*) FILTER (WHERE r.status = 'failed')::bigint AS requests_failed,
    count(*) FILTER (
        WHERE r.status = 'succeeded'
          AND r.pricing_id IS NOT NULL
          AND r.estimated_cost_nano_usd IS NOT NULL
    )::bigint AS priced_requests,
    COALESCE(SUM(r.prompt_tokens), 0)::bigint AS prompt_tokens,
    COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
    COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
    COALESCE(SUM(r.estimated_cost_nano_usd), 0)::bigint AS estimated_cost_nano_usd,
    COALESCE(SUM(r.latency_ms), 0)::bigint AS latency_ms_sum,
    count(r.latency_ms)::bigint AS latency_ms_count,
    COALESCE(SUM(r.ttft_ms), 0)::bigint AS ttft_ms_sum,
    count(r.ttft_ms)::bigint AS ttft_ms_count
FROM gateway_requests AS r
JOIN projects AS p
  ON p.id = r.project_id
 AND p.owner_user_id = sqlc.arg('owner_user_id')
WHERE r.started_at >= sqlc.arg('from')::timestamptz
  AND r.started_at < sqlc.arg('to')::timestamptz
  AND (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid);

-- name: UsageTimeseriesForOwnerDay :many
SELECT
    buckets.ts::timestamptz AS ts,
    COALESCE(a.requests_total, 0)::bigint AS requests_total,
    COALESCE(a.requests_succeeded, 0)::bigint AS requests_succeeded,
    COALESCE(a.requests_failed, 0)::bigint AS requests_failed,
    COALESCE(a.requests_priced, 0)::bigint AS requests_priced,
    COALESCE(a.requests_unpriced, 0)::bigint AS requests_unpriced,
    COALESCE(a.prompt_tokens, 0)::bigint AS prompt_tokens,
    COALESCE(a.completion_tokens, 0)::bigint AS completion_tokens,
    COALESCE(a.total_tokens, 0)::bigint AS total_tokens,
    COALESCE(a.estimated_cost_nano_usd, 0)::bigint AS estimated_cost_nano_usd
FROM generate_series(
    date_trunc('day', (sqlc.arg('from')::timestamptz) AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    date_trunc('day', ((sqlc.arg('to')::timestamptz) - interval '1 microsecond') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    interval '1 day'
) AS buckets(ts)
LEFT JOIN (
    SELECT
        date_trunc('day', (r.started_at AT TIME ZONE 'UTC')) AT TIME ZONE 'UTC' AS ts,
        count(*)::bigint AS requests_total,
        count(*) FILTER (WHERE r.status = 'succeeded')::bigint AS requests_succeeded,
        count(*) FILTER (WHERE r.status = 'failed')::bigint AS requests_failed,
        count(*) FILTER (WHERE r.status = 'succeeded' AND r.pricing_id IS NOT NULL AND r.estimated_cost_nano_usd IS NOT NULL)::bigint AS requests_priced,
        count(*) FILTER (WHERE r.status = 'succeeded' AND (r.pricing_id IS NULL OR r.estimated_cost_nano_usd IS NULL))::bigint AS requests_unpriced,
        COALESCE(SUM(r.prompt_tokens), 0)::bigint AS prompt_tokens,
        COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
        COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
        COALESCE(SUM(r.estimated_cost_nano_usd), 0)::bigint AS estimated_cost_nano_usd
    FROM gateway_requests AS r
    JOIN projects AS p
      ON p.id = r.project_id
     AND p.owner_user_id = sqlc.arg('owner_user_id')
    WHERE r.started_at >= sqlc.arg('from')::timestamptz
      AND r.started_at < sqlc.arg('to')::timestamptz
      AND (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid)
    GROUP BY 1
) AS a ON a.ts = buckets.ts
ORDER BY buckets.ts ASC;

-- name: UsageTimeseriesForOwnerHour :many
SELECT
    buckets.ts::timestamptz AS ts,
    COALESCE(a.requests_total, 0)::bigint AS requests_total,
    COALESCE(a.requests_succeeded, 0)::bigint AS requests_succeeded,
    COALESCE(a.requests_failed, 0)::bigint AS requests_failed,
    COALESCE(a.requests_priced, 0)::bigint AS requests_priced,
    COALESCE(a.requests_unpriced, 0)::bigint AS requests_unpriced,
    COALESCE(a.prompt_tokens, 0)::bigint AS prompt_tokens,
    COALESCE(a.completion_tokens, 0)::bigint AS completion_tokens,
    COALESCE(a.total_tokens, 0)::bigint AS total_tokens,
    COALESCE(a.estimated_cost_nano_usd, 0)::bigint AS estimated_cost_nano_usd
FROM generate_series(
    date_trunc('hour', (sqlc.arg('from')::timestamptz) AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    date_trunc('hour', ((sqlc.arg('to')::timestamptz) - interval '1 microsecond') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    interval '1 hour'
) AS buckets(ts)
LEFT JOIN (
    SELECT
        date_trunc('hour', (r.started_at AT TIME ZONE 'UTC')) AT TIME ZONE 'UTC' AS ts,
        count(*)::bigint AS requests_total,
        count(*) FILTER (WHERE r.status = 'succeeded')::bigint AS requests_succeeded,
        count(*) FILTER (WHERE r.status = 'failed')::bigint AS requests_failed,
        count(*) FILTER (WHERE r.status = 'succeeded' AND r.pricing_id IS NOT NULL AND r.estimated_cost_nano_usd IS NOT NULL)::bigint AS requests_priced,
        count(*) FILTER (WHERE r.status = 'succeeded' AND (r.pricing_id IS NULL OR r.estimated_cost_nano_usd IS NULL))::bigint AS requests_unpriced,
        COALESCE(SUM(r.prompt_tokens), 0)::bigint AS prompt_tokens,
        COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
        COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
        COALESCE(SUM(r.estimated_cost_nano_usd), 0)::bigint AS estimated_cost_nano_usd
    FROM gateway_requests AS r
    JOIN projects AS p
      ON p.id = r.project_id
     AND p.owner_user_id = sqlc.arg('owner_user_id')
    WHERE r.started_at >= sqlc.arg('from')::timestamptz
      AND r.started_at < sqlc.arg('to')::timestamptz
      AND (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid)
    GROUP BY 1
) AS a ON a.ts = buckets.ts
ORDER BY buckets.ts ASC;

-- name: UsageBreakdownByProviderForOwner :many
SELECT
    r.provider AS key,
    count(*)::bigint AS requests_total,
    count(*) FILTER (WHERE r.status = 'failed')::bigint AS requests_failed,
    count(*) FILTER (WHERE r.status = 'succeeded' AND r.pricing_id IS NOT NULL AND r.estimated_cost_nano_usd IS NOT NULL)::bigint AS requests_priced,
    count(*) FILTER (WHERE r.status = 'succeeded' AND (r.pricing_id IS NULL OR r.estimated_cost_nano_usd IS NULL))::bigint AS requests_unpriced,
    COALESCE(SUM(r.prompt_tokens), 0)::bigint AS prompt_tokens,
    COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
    COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
    COALESCE(SUM(r.estimated_cost_nano_usd), 0)::bigint AS estimated_cost_nano_usd
FROM gateway_requests AS r
JOIN projects AS p
  ON p.id = r.project_id
 AND p.owner_user_id = sqlc.arg('owner_user_id')
WHERE r.started_at >= sqlc.arg('from')::timestamptz
  AND r.started_at < sqlc.arg('to')::timestamptz
  AND (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid)
GROUP BY r.provider
ORDER BY COALESCE(SUM(r.estimated_cost_nano_usd), 0) DESC
LIMIT sqlc.arg('limit');

-- name: UsageBreakdownByModelForOwner :many
SELECT
    r.model AS key,
    count(*)::bigint AS requests_total,
    count(*) FILTER (WHERE r.status = 'failed')::bigint AS requests_failed,
    count(*) FILTER (WHERE r.status = 'succeeded' AND r.pricing_id IS NOT NULL AND r.estimated_cost_nano_usd IS NOT NULL)::bigint AS requests_priced,
    count(*) FILTER (WHERE r.status = 'succeeded' AND (r.pricing_id IS NULL OR r.estimated_cost_nano_usd IS NULL))::bigint AS requests_unpriced,
    COALESCE(SUM(r.prompt_tokens), 0)::bigint AS prompt_tokens,
    COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
    COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
    COALESCE(SUM(r.estimated_cost_nano_usd), 0)::bigint AS estimated_cost_nano_usd
FROM gateway_requests AS r
JOIN projects AS p
  ON p.id = r.project_id
 AND p.owner_user_id = sqlc.arg('owner_user_id')
WHERE r.started_at >= sqlc.arg('from')::timestamptz
  AND r.started_at < sqlc.arg('to')::timestamptz
  AND (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid)
GROUP BY r.model
ORDER BY COALESCE(SUM(r.estimated_cost_nano_usd), 0) DESC
LIMIT sqlc.arg('limit');

-- name: UsageBreakdownByKeyForOwner :many
SELECT
    r.virtual_key_id AS key_id,
    k.key_prefix AS key_prefix,
    count(*)::bigint AS requests_total,
    count(*) FILTER (WHERE r.status = 'failed')::bigint AS requests_failed,
    count(*) FILTER (WHERE r.status = 'succeeded' AND r.pricing_id IS NOT NULL AND r.estimated_cost_nano_usd IS NOT NULL)::bigint AS requests_priced,
    count(*) FILTER (WHERE r.status = 'succeeded' AND (r.pricing_id IS NULL OR r.estimated_cost_nano_usd IS NULL))::bigint AS requests_unpriced,
    COALESCE(SUM(r.prompt_tokens), 0)::bigint AS prompt_tokens,
    COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
    COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
    COALESCE(SUM(r.estimated_cost_nano_usd), 0)::bigint AS estimated_cost_nano_usd
FROM gateway_requests AS r
JOIN projects AS p
  ON p.id = r.project_id
 AND p.owner_user_id = sqlc.arg('owner_user_id')
JOIN virtual_api_keys AS k
  ON k.id = r.virtual_key_id
WHERE r.started_at >= sqlc.arg('from')::timestamptz
  AND r.started_at < sqlc.arg('to')::timestamptz
  AND (sqlc.narg('project_id')::uuid IS NULL OR r.project_id = sqlc.narg('project_id')::uuid)
GROUP BY r.virtual_key_id, k.key_prefix
ORDER BY COALESCE(SUM(r.estimated_cost_nano_usd), 0) DESC
LIMIT sqlc.arg('limit');
