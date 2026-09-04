-- name: AuthenticateVirtualAPIKey :one
SELECT
    keys.id,
    keys.project_id,
    keys.key_prefix
FROM virtual_api_keys AS keys
JOIN projects
  ON projects.id = keys.project_id
WHERE keys.key_prefix = sqlc.arg('key_prefix')
  AND keys.key_hash = sqlc.arg('key_hash')
  AND keys.status = 'active'
  AND projects.status = 'active';

-- name: GetActiveProviderCredentialForProjectConfig :one
SELECT
    configs.project_id,
    configs.provider,
    configs.credential_id,
    configs.base_url_override,
    credentials.secret_ciphertext,
    credentials.secret_nonce,
    credentials.key_version
FROM project_provider_configs AS configs
JOIN provider_credentials AS credentials
  ON credentials.id = configs.credential_id
 AND credentials.project_id = configs.project_id
 AND credentials.provider = configs.provider
WHERE configs.project_id = sqlc.arg('project_id')
  AND configs.provider = sqlc.arg('provider')
  AND configs.enabled = true
  AND credentials.status = 'active';

-- name: CreateGatewayRequest :one
INSERT INTO gateway_requests (
    id,
    project_id,
    virtual_key_id,
    provider_credential_id,
    provider,
    model,
    is_stream,
    status,
    started_at,
    trace_id
)
VALUES (
    sqlc.arg('id'),
    sqlc.arg('project_id'),
    sqlc.arg('virtual_key_id'),
    sqlc.arg('provider_credential_id'),
    sqlc.arg('provider'),
    sqlc.arg('model'),
    sqlc.arg('is_stream'),
    'in_progress',
    sqlc.arg('started_at'),
    sqlc.narg('trace_id')
)
RETURNING id, project_id, virtual_key_id, provider_credential_id, provider, model, is_stream, status, started_at, created_at;

-- name: FindModelPriceAt :one
SELECT
    id,
    provider,
    model,
    input_nano_usd_per_million,
    output_nano_usd_per_million,
    effective_from,
    effective_to,
    source_note,
    created_at
FROM model_prices
WHERE provider = sqlc.arg('provider')
  AND model = sqlc.arg('model')
  AND effective_from <= sqlc.arg('at_time')
  AND (effective_to IS NULL OR effective_to > sqlc.arg('at_time'))
ORDER BY effective_from DESC
LIMIT 1;

-- name: FinalizeGatewayRequest :one
UPDATE gateway_requests
SET
    status = sqlc.arg('status'),
    first_chunk_at = sqlc.narg('first_chunk_at'),
    completed_at = sqlc.arg('completed_at'),
    latency_ms = sqlc.narg('latency_ms'),
    ttft_ms = sqlc.narg('ttft_ms'),
    upstream_http_status = sqlc.narg('upstream_http_status'),
    error_category = sqlc.narg('error_category'),
    retry_count = sqlc.arg('retry_count'),
    prompt_tokens = sqlc.narg('prompt_tokens'),
    completion_tokens = sqlc.narg('completion_tokens'),
    total_tokens = sqlc.narg('total_tokens'),
    usage_source = sqlc.narg('usage_source'),
    pricing_id = sqlc.narg('pricing_id'),
    estimated_cost_nano_usd = sqlc.narg('estimated_cost_nano_usd'),
    upstream_request_id = sqlc.narg('upstream_request_id')
WHERE id = sqlc.arg('id')
RETURNING
    id,
    project_id,
    virtual_key_id,
    provider_credential_id,
    provider,
    model,
    is_stream,
    status,
    started_at,
    first_chunk_at,
    completed_at,
    latency_ms,
    ttft_ms,
    upstream_http_status,
    error_category,
    retry_count,
    prompt_tokens,
    completion_tokens,
    total_tokens,
    usage_source,
    pricing_id,
    estimated_cost_nano_usd,
    upstream_request_id,
    trace_id,
    created_at;

-- name: GetGatewayRequest :one
SELECT
    id,
    project_id,
    virtual_key_id,
    provider_credential_id,
    provider,
    model,
    is_stream,
    status,
    started_at,
    first_chunk_at,
    completed_at,
    latency_ms,
    ttft_ms,
    upstream_http_status,
    error_category,
    retry_count,
    prompt_tokens,
    completion_tokens,
    total_tokens,
    usage_source,
    pricing_id,
    estimated_cost_nano_usd,
    upstream_request_id,
    trace_id,
    created_at
FROM gateway_requests
WHERE id = $1;
