# ADR-001: Modular monolith with three HTTP listeners

- Status: Accepted
- Date: 2026-08-28

## Context

The gateway needs three different trust and runtime boundaries:

- public, latency-sensitive LLM traffic with long-lived streaming;
- authenticated management traffic with ordinary request/response timeouts;
- private operational endpoints such as health, metrics, and later `pprof`.

Splitting these concerns into microservices during a 12-week MVP would add service networking, deployment, and distributed-failure work before the gateway path is understood. Combining everything behind one listener would make it easy to expose operational endpoints or apply CRUD timeouts to streams.

## Options considered

1. One Go process and one HTTP listener.
2. One Go process with three HTTP listeners.
3. Separate Data, Control, and Operations binaries/services.

## Decision

Use one Go 1.26 modular-monolith process with three independently configured `http.Server` instances:

- Data Plane on `:8080` by default.
- Control Plane on `:8081` by default.
- Operations Plane on `:9090` by default and kept private in production.

Each server owns a separate `ServeMux` and timeout policy. `ReadHeaderTimeout` bounds the time spent reading only request headers. The Data Plane also has a bounded `ReadTimeout` for the full request read, which prevents clients from sending headers and then trickling a request body forever during the Week 1 foundation. The Data Plane intentionally has no server-wide `WriteTimeout`, because that timeout would cover the lifetime of valid long-running SSE streams. `IdleTimeout` applies only between requests on an idle keep-alive connection. All listeners are bound before serving so a port conflict fails startup without leaving a partially running application.

On shutdown, the process stops accepting new traffic, starts graceful shutdown for Control, Data, and Operations concurrently under one application-level deadline, force-closes any server that does not drain by the deadline, and closes PostgreSQL. Package boundaries remain conceptual seams that can support a later binary split without introducing service boundaries now.

## Consequences

- Data, Control, and Operations policies are visible and testable from the first milestone.
- A single process keeps local development, deployment, tracing, and failure analysis simple.
- One process is still one failure and scaling domain; a crash affects all planes.
- Server lifecycle code must coordinate three listeners and avoid partial startup.
- A future split must preserve domain/service boundaries instead of moving HTTP handlers wholesale into services.

## Verification

- Unit tests assert that the Data Plane has no global `WriteTimeout`, does have bounded request-read protection, and other planes use bounded timeouts.
- Lifecycle tests cover all three listeners, basic routing, port conflicts, active-request draining, concurrent graceful shutdown, forced close after timeout, and database closure.
- Manual verification checks readiness with PostgreSQL available, unavailable, and restored, followed by an actual `SIGTERM`.

## Revisit when

- Data and Control planes require materially different scaling or deployment cadence.
- The Operations plane needs a stronger process-isolation boundary.
- Benchmarks or failure tests show that shared process resources cause unacceptable interference.
