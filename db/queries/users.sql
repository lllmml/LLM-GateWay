-- name: UpsertUserFromGitHub :one
INSERT INTO users (
    id,
    github_id,
    github_login,
    avatar_url
) VALUES (
    $1,
    $2,
    $3,
    $4
)
ON CONFLICT (github_id) DO UPDATE SET
    github_login = EXCLUDED.github_login,
    avatar_url = EXCLUDED.avatar_url,
    updated_at = now()
RETURNING id, github_id, github_login, avatar_url, created_at, updated_at;

-- name: GetUserByID :one
SELECT id, github_id, github_login, avatar_url, created_at, updated_at
FROM users
WHERE id = $1;

-- name: GetUserByGitHubID :one
SELECT id, github_id, github_login, avatar_url, created_at, updated_at
FROM users
WHERE github_id = $1;
