-- name: CreateProject :one
INSERT INTO projects (
    id,
    owner_user_id,
    name,
    slug
) VALUES (
    $1,
    $2,
    $3,
    $4
)
RETURNING id, owner_user_id, name, slug, status, created_at, updated_at;

-- name: GetProjectForOwner :one
SELECT id, owner_user_id, name, slug, status, created_at, updated_at
FROM projects
WHERE id = $1
  AND owner_user_id = $2;

-- name: ListProjectsForOwner :many
SELECT id, owner_user_id, name, slug, status, created_at, updated_at
FROM projects
WHERE owner_user_id = $1
ORDER BY created_at DESC, id DESC;

-- name: UpdateProjectForOwner :one
UPDATE projects
SET
    name = COALESCE(sqlc.narg('name'), name),
    slug = COALESCE(sqlc.narg('slug'), slug),
    status = COALESCE(sqlc.narg('status'), status),
    updated_at = now()
WHERE id = $1
  AND owner_user_id = $2
RETURNING id, owner_user_id, name, slug, status, created_at, updated_at;
