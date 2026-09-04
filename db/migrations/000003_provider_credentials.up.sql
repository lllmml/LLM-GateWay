CREATE TABLE provider_credentials (
    id UUID PRIMARY KEY,
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('openai', 'anthropic', 'deepseek')),
    label TEXT NOT NULL CHECK (octet_length(trim(label)) BETWEEN 1 AND 100),
    secret_ciphertext BYTEA NOT NULL CHECK (length(secret_ciphertext) > 0),
    secret_nonce BYTEA NOT NULL CHECK (length(secret_nonce) = 12),
    key_version SMALLINT NOT NULL CHECK (key_version > 0),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at TIMESTAMPTZ NULL CHECK (rotated_at IS NULL OR rotated_at >= created_at)
);

CREATE INDEX provider_credentials_project_id_provider_status_idx
    ON provider_credentials (project_id, provider, status);
