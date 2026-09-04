-- name: CreateVirtualAPIKeyForOwner :one
INSERT INTO virtual_api_keys (
    id,
    project_id,
    name,
    key_prefix,
    key_hash
)
SELECT
    sqlc.arg('id'),
    projects.id,
    sqlc.arg('name'),
    sqlc.arg('key_prefix'),
    sqlc.arg('key_hash')
FROM projects
WHERE projects.id = sqlc.arg('project_id')
  AND projects.owner_user_id = sqlc.arg('owner_user_id')
RETURNING id, project_id, name, key_prefix, status, created_at, last_used_at, revoked_at;

-- name: ListVirtualAPIKeysForOwner :many
SELECT
    keys.id,
    keys.project_id,
    keys.name,
    keys.key_prefix,
    keys.status,
    keys.created_at,
    keys.last_used_at,
    keys.revoked_at
FROM virtual_api_keys AS keys
JOIN projects ON projects.id = keys.project_id
WHERE keys.project_id = $1
  AND projects.owner_user_id = $2
ORDER BY keys.created_at DESC, keys.id DESC;

-- name: DisableVirtualAPIKeyForOwner :one
UPDATE virtual_api_keys AS keys
SET status = CASE
    WHEN keys.status = 'active' THEN 'disabled'
    ELSE keys.status
END
FROM projects
WHERE keys.id = $1
  AND keys.project_id = $2
  AND projects.id = keys.project_id
  AND projects.owner_user_id = $3
RETURNING keys.id, keys.project_id, keys.name, keys.key_prefix, keys.status, keys.created_at, keys.last_used_at, keys.revoked_at;

-- name: RevokeVirtualAPIKeyForOwner :one
UPDATE virtual_api_keys AS keys
SET
    status = 'revoked',
    revoked_at = COALESCE(keys.revoked_at, now())
FROM projects
WHERE keys.id = $1
  AND keys.project_id = $2
  AND projects.id = keys.project_id
  AND projects.owner_user_id = $3
RETURNING keys.id, keys.project_id, keys.name, keys.key_prefix, keys.status, keys.created_at, keys.last_used_at, keys.revoked_at;
