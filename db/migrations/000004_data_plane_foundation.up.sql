CREATE UNIQUE INDEX provider_credentials_identity_provider_idx
    ON provider_credentials (id, project_id, provider);

CREATE TABLE project_provider_configs (
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('openai', 'anthropic', 'deepseek')),
    credential_id UUID NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    base_url_override TEXT NULL CHECK (
        base_url_override IS NULL
        OR (
            octet_length(trim(base_url_override)) BETWEEN 1 AND 2048
            AND base_url_override !~ '[[:space:]]'
        )
    ),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, provider),
    FOREIGN KEY (credential_id, project_id, provider)
        REFERENCES provider_credentials (id, project_id, provider)
        ON DELETE RESTRICT
);

CREATE INDEX project_provider_configs_provider_enabled_idx
    ON project_provider_configs (provider, enabled);

CREATE TABLE model_prices (
    id UUID PRIMARY KEY,
    provider TEXT NOT NULL CHECK (provider IN ('openai', 'anthropic', 'deepseek')),
    model TEXT NOT NULL CHECK (octet_length(trim(model)) BETWEEN 1 AND 200),
    input_nano_usd_per_million BIGINT NOT NULL CHECK (input_nano_usd_per_million >= 0),
    output_nano_usd_per_million BIGINT NOT NULL CHECK (output_nano_usd_per_million >= 0),
    effective_from TIMESTAMPTZ NOT NULL,
    effective_to TIMESTAMPTZ NULL CHECK (effective_to IS NULL OR effective_to > effective_from),
    source_note TEXT NOT NULL CHECK (octet_length(trim(source_note)) BETWEEN 1 AND 500),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX model_prices_provider_model_effective_from_idx
    ON model_prices (provider, model, effective_from);

CREATE INDEX model_prices_provider_model_effective_window_idx
    ON model_prices (provider, model, effective_from DESC, effective_to);

CREATE TABLE gateway_requests (
    id UUID PRIMARY KEY,
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    virtual_key_id UUID NOT NULL REFERENCES virtual_api_keys (id) ON DELETE RESTRICT,
    provider_credential_id UUID NOT NULL REFERENCES provider_credentials (id) ON DELETE RESTRICT,
    provider TEXT NOT NULL CHECK (provider IN ('openai', 'anthropic', 'deepseek')),
    model TEXT NOT NULL CHECK (octet_length(trim(model)) BETWEEN 1 AND 200),
    is_stream BOOLEAN NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('in_progress', 'succeeded', 'failed')),
    started_at TIMESTAMPTZ NOT NULL,
    first_chunk_at TIMESTAMPTZ NULL CHECK (first_chunk_at IS NULL OR first_chunk_at >= started_at),
    completed_at TIMESTAMPTZ NULL CHECK (completed_at IS NULL OR completed_at >= started_at),
    latency_ms BIGINT NULL CHECK (latency_ms IS NULL OR latency_ms >= 0),
    ttft_ms BIGINT NULL CHECK (ttft_ms IS NULL OR ttft_ms >= 0),
    upstream_http_status INTEGER NULL CHECK (upstream_http_status IS NULL OR upstream_http_status BETWEEN 100 AND 599),
    error_category TEXT NULL CHECK (error_category IS NULL OR error_category IN (
        'invalid_request',
        'authentication_failed',
        'authorization_failed',
        'rate_limited',
        'provider_not_configured',
        'model_not_supported',
        'provider_rate_limited',
        'provider_timeout',
        'provider_unavailable',
        'provider_invalid_request',
        'stream_interrupted',
        'usage_persistence_failed',
        'internal_error'
    )),
    retry_count SMALLINT NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    prompt_tokens BIGINT NULL CHECK (prompt_tokens IS NULL OR prompt_tokens >= 0),
    completion_tokens BIGINT NULL CHECK (completion_tokens IS NULL OR completion_tokens >= 0),
    total_tokens BIGINT NULL CHECK (total_tokens IS NULL OR total_tokens >= 0),
    usage_source TEXT NULL CHECK (usage_source IS NULL OR octet_length(trim(usage_source)) BETWEEN 1 AND 100),
    pricing_id UUID NULL REFERENCES model_prices (id) ON DELETE RESTRICT,
    estimated_cost_nano_usd BIGINT NULL CHECK (estimated_cost_nano_usd IS NULL OR estimated_cost_nano_usd >= 0),
    upstream_request_id TEXT NULL CHECK (upstream_request_id IS NULL OR octet_length(trim(upstream_request_id)) BETWEEN 1 AND 200),
    trace_id TEXT NULL CHECK (trace_id IS NULL OR octet_length(trim(trace_id)) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (status = 'in_progress' AND completed_at IS NULL)
        OR (status <> 'in_progress' AND completed_at IS NOT NULL)
    ),
    CHECK (
        (error_category IS NULL AND status IN ('in_progress', 'succeeded'))
        OR (error_category IS NOT NULL AND status = 'failed')
    )
);

CREATE INDEX gateway_requests_project_started_at_idx
    ON gateway_requests (project_id, started_at DESC);

CREATE INDEX gateway_requests_provider_model_started_at_idx
    ON gateway_requests (provider, model, started_at DESC);

CREATE INDEX gateway_requests_status_started_at_idx
    ON gateway_requests (status, started_at DESC);

CREATE INDEX gateway_requests_virtual_key_started_at_idx
    ON gateway_requests (virtual_key_id, started_at DESC);
