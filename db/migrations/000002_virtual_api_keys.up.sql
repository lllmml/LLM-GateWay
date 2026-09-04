CREATE TABLE virtual_api_keys (
    id UUID PRIMARY KEY,
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (octet_length(trim(name)) BETWEEN 1 AND 100),
    key_prefix TEXT NOT NULL CHECK (
        octet_length(key_prefix) = 8
        AND key_prefix ~ '^[A-Za-z0-9_-]{8}$'
    ),
    key_hash BYTEA NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NULL CHECK (last_used_at IS NULL OR last_used_at >= created_at),
    revoked_at TIMESTAMPTZ NULL CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    CHECK (length(key_hash) = 32),
    CHECK (
        (status = 'revoked' AND revoked_at IS NOT NULL)
        OR (status <> 'revoked' AND revoked_at IS NULL)
    )
);

CREATE INDEX virtual_api_keys_project_id_status_idx ON virtual_api_keys (project_id, status);
