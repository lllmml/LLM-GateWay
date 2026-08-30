-- name: CreateProviderCredentialForOwner :one
INSERT INTO provider_credentials (
    id,
    project_id,
    provider,
    label,
    secret_ciphertext,
    secret_nonce,
    key_version
)
SELECT
    sqlc.arg('id'),
    projects.id,
    sqlc.arg('provider'),
    sqlc.arg('label'),
    sqlc.arg('secret_ciphertext'),
    sqlc.arg('secret_nonce'),
    sqlc.arg('key_version')
FROM projects
WHERE projects.id = sqlc.arg('project_id')
  AND projects.owner_user_id = sqlc.arg('owner_user_id')
RETURNING id, project_id, provider, label, key_version, status, created_at, rotated_at;

-- name: ListProviderCredentialsForOwner :many
SELECT
    credentials.id,
    credentials.project_id,
    credentials.provider,
    credentials.label,
    credentials.key_version,
    credentials.status,
    credentials.created_at,
    credentials.rotated_at
FROM provider_credentials AS credentials
JOIN projects ON projects.id = credentials.project_id
WHERE credentials.project_id = $1
  AND projects.owner_user_id = $2
ORDER BY credentials.created_at DESC, credentials.id DESC;

-- name: RotateProviderCredentialForOwner :one
UPDATE provider_credentials AS credentials
SET
    secret_ciphertext = $4,
    secret_nonce = $5,
    key_version = $6,
    rotated_at = now()
FROM projects
WHERE credentials.id = $1
  AND credentials.project_id = $2
  AND projects.id = credentials.project_id
  AND projects.owner_user_id = $3
RETURNING credentials.id, credentials.project_id, credentials.provider, credentials.label, credentials.key_version, credentials.status, credentials.created_at, credentials.rotated_at;

-- name: DisableProviderCredentialForOwner :one
UPDATE provider_credentials AS credentials
SET status = CASE
    WHEN credentials.status = 'active' THEN 'disabled'
    ELSE credentials.status
END
FROM projects
WHERE credentials.id = $1
  AND credentials.project_id = $2
  AND projects.id = credentials.project_id
  AND projects.owner_user_id = $3
RETURNING credentials.id, credentials.project_id, credentials.provider, credentials.label, credentials.key_version, credentials.status, credentials.created_at, credentials.rotated_at;
