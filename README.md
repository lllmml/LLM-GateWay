# Production Go LLM Gateway

Production Go LLM Gateway is a Go modular-monolith foundation for an LLM-aware gateway and Web management console. It is intended to centralize multi-provider LLM access, virtual gateway keys, usage/cost attribution, streaming behavior, and operational visibility over the 12-week MVP roadmap.

It is not a normal Nginx or Envoy proxy because the gateway must understand LLM-specific concerns such as provider/model semantics, streaming lifecycle, virtual-key attribution, usage extraction, and cost records rather than only forwarding HTTP bytes.

## Current Status

Week 1 foundation is implemented. The repository currently contains the process shape, configuration loading, PostgreSQL connectivity, health endpoints, basic structured logging, graceful shutdown, and tests around those foundation pieces.

OpenAI, Anthropic, DeepSeek provider adapters, streaming proxying, virtual gateway keys, provider credential CRUD, usage/cost attribution, Prometheus metrics, OpenTelemetry traces, and the management frontend are roadmap work and are not all implemented yet.

## Architecture

- Data Plane: `:8080` for future public LLM traffic.
- Control Plane: `:8081` for future management APIs.
- Operations Plane: `:9090` for private health and operational endpoints.
- PostgreSQL: durable source of truth for control-plane state and request/usage records.

The application is one Go process with three independently configured `net/http` servers. It is intentionally not split into microservices during the MVP foundation.

## Quick Start

Copy the example environment file and set the required local development values:

```bash
cp .env.example .env
```

Required development config:

- `DATABASE_URL`
- `CREDENTIAL_MASTER_KEY`

Generate a development `CREDENTIAL_MASTER_KEY` value with:

```bash
openssl rand -base64 32
```

Then run:

```bash
make bootstrap
make dev
```

## Health Endpoints

- Data Plane: `GET http://localhost:8080/` currently returns `404` until the gateway API is implemented.
- Control Plane: `GET http://localhost:8081/` currently returns `404` until management APIs are implemented.
- Operations liveness: `GET http://localhost:9090/health/live`
- Operations readiness: `GET http://localhost:9090/health/ready`

Readiness depends on PostgreSQL connectivity.

## Make Commands

- `make bootstrap` - install/generate local prerequisites.
- `make dev` - run the local development environment.
- `make test` - run the normal test suite.
- `make typecheck` - run Go/frontend type checks.
- `make lint` - run formatting and lint checks.
- `make build` - build backend and frontend artifacts.
- `make integration` - run integration tests with real service dependencies.
- `make race` - run race-enabled Go tests.
- `make bench` - run reproducible benchmark scenarios.

## Documentation

- PRD: [docs/PRD-Production-Go-LLM-Gateway-MVP.md](docs/PRD-Production-Go-LLM-Gateway-MVP.md)
- Technical design and 12-week roadmap: [docs/TechDesign-Production-Go-LLM-Gateway-MVP.md](docs/TechDesign-Production-Go-LLM-Gateway-MVP.md)
- ADRs: [docs/adr/](docs/adr/)
- Three-listener decision: [docs/adr/ADR-001-modular-monolith-three-listeners.md](docs/adr/ADR-001-modular-monolith-three-listeners.md)
