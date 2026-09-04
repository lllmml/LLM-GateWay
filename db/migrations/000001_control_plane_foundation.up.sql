CREATE TABLE users (
    id UUID PRIMARY KEY,
    github_id BIGINT NOT NULL UNIQUE,
    github_login TEXT NOT NULL CHECK (length(trim(github_login)) > 0),
    avatar_url TEXT NULL CHECK (avatar_url IS NULL OR length(trim(avatar_url)) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE web_sessions (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (length(token_hash) = 32),
    CHECK (expires_at > created_at),
    CHECK (last_seen_at >= created_at)
);

CREATE INDEX web_sessions_expires_at_idx ON web_sessions (expires_at);

CREATE TABLE projects (
    id UUID PRIMARY KEY,
    owner_user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (octet_length(trim(name)) BETWEEN 1 AND 100),
    slug TEXT NOT NULL CHECK (
        octet_length(slug) BETWEEN 1 AND 63
        AND slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
    ),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX projects_owner_user_id_slug_idx ON projects (owner_user_id, slug);
CREATE INDEX projects_owner_user_id_status_idx ON projects (owner_user_id, status);
