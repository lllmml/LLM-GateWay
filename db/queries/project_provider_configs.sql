-- name: UpsertProjectProviderConfigForOwner :one
INSERT INTO project_provider_configs (
    project_id,
    provider,
    credential_id,
    enabled,
    base_url_override,
    updated_at
)
SELECT
    projects.id,
    sqlc.arg('provider'),
    credentials.id,
    sqlc.arg('enabled'),
    sqlc.narg('base_url_override'),
    now()
FROM projects
JOIN provider_credentials AS credentials
  ON credentials.id = sqlc.arg('credential_id')
 AND credentials.project_id = projects.id
 AND credentials.provider = sqlc.arg('provider')
 AND credentials.status = 'active'
WHERE projects.id = sqlc.arg('project_id')
  AND projects.owner_user_id = sqlc.arg('owner_user_id')
ON CONFLICT (project_id, provider)
DO UPDATE SET
    credential_id = EXCLUDED.credential_id,
    enabled = EXCLUDED.enabled,
    base_url_override = EXCLUDED.base_url_override,
    updated_at = now()
RETURNING
    project_id,
    provider,
    credential_id,
    enabled,
    base_url_override,
    updated_at;
