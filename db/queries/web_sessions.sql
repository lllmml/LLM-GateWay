-- name: CreateWebSession :exec
INSERT INTO web_sessions (
    id,
    user_id,
    token_hash,
    expires_at,
    created_at,
    last_seen_at
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $5
);

-- name: GetValidWebSessionByTokenHash :one
SELECT
    users.id AS user_id,
    users.github_id,
    users.github_login,
    users.avatar_url,
    web_sessions.expires_at
FROM web_sessions
JOIN users ON users.id = web_sessions.user_id
WHERE web_sessions.token_hash = $1
  AND web_sessions.expires_at > $2;

-- name: DeleteWebSessionByTokenHash :exec
DELETE FROM web_sessions
WHERE token_hash = $1;
