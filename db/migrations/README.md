# Database migrations

Migrations use sequential `golang-migrate` up/down SQL files.

- `000001_control_plane_foundation` creates `users`, `web_sessions`, and `projects` with the initial ownership/session constraints.
- `000002_virtual_api_keys` creates project-scoped virtual key metadata with one-way digest storage and logical revoke state.
- `000003_provider_credentials` creates project-scoped, recoverably encrypted provider credential envelopes and lifecycle metadata.
- Run migrations through the repository Makefile targets; integration tests apply each migration in an isolated PostgreSQL schema.
- Never edit a migration after it has been applied to a shared environment without explicit approval and a recovery plan.
