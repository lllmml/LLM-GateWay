# Database migrations

Migrations use sequential `golang-migrate` up/down SQL files.

- `000001_control_plane_foundation` creates `users`, `web_sessions`, and `projects` with the initial ownership/session constraints.
- Run migrations through the repository Makefile targets; integration tests apply each migration in an isolated PostgreSQL schema.
- Never edit a migration after it has been applied to a shared environment without explicit approval and a recovery plan.
