# Product Requirements Document: Production Go LLM Gateway MVP

## Overview

**Product Name:** Production Go LLM Gateway  
**Problem Statement:** Developers and small engineering teams that use multiple LLM providers often have fragmented API keys, request histories, token usage, costs, latency data, and error information across different provider dashboards. This product provides one production-oriented gateway and management surface that centralizes model access, request attribution, usage governance, and operational visibility without turning the project into an agent platform.  
**MVP Goal:** In 3 months, deliver a deployable Go-based LLM Gateway and Web management platform that supports at least OpenAI, Anthropic, and DeepSeek; handles real streaming traffic; attributes requests and usage to projects and virtual keys; exposes operational and cost visibility; demonstrates production-grade reliability practices; and produces real benchmark/profiling evidence suitable for a backend internship portfolio.  
**Target Launch:** 3 months from project start.

## Target Users

### Primary User Profile

**Who:** Developers, small engineering teams, and backend/AI infrastructure engineers building products that call multiple LLM providers.  
**Problem:** They need to manage several provider credentials and models while usage, token consumption, cost, latency, and error data are fragmented across different systems.  
**Current Solution:** Calling providers directly with provider-specific API keys and using each provider's own dashboard, sometimes combined with generic API gateways or ad-hoc logging. Existing LLM gateways may also solve many of these problems; the final differentiation against products such as LiteLLM must be validated by the research phase.  
**Why They'll Switch:** One gateway endpoint and one operational model can provide project-level attribution, virtual keys, provider abstraction, streaming-aware telemetry, reliability controls, and a unified dashboard.

### User Persona: Multi-Provider LLM Developer

- **Role:** Backend developer, AI application developer, or small-team infrastructure engineer.
- **Tech Level:** Intermediate to advanced API user; comfortable with SDKs, HTTP APIs, API keys, logs, and dashboards.
- **Goals:**
  - Integrate several LLM providers without scattering credentials and observability logic across applications.
  - Know which project, key, provider, and model generated each request and cost.
  - Operate LLM traffic with predictable failure behavior.
  - Diagnose latency, TTFT, error rate, retry behavior, and provider problems.
- **Frustrations:**
  - Fragmented provider dashboards.
  - Difficult project/user attribution when the same provider account is shared.
  - Different request, response, streaming, and error semantics across providers.
  - Generic reverse proxies do not understand token usage, LLM cost, model semantics, or end-of-stream usage data.

## User Journey

### The Story

A developer is building multiple applications that use OpenAI, Anthropic, and DeepSeek. Instead of putting provider API keys directly into every application, they deploy Production Go LLM Gateway.

They sign in to the management dashboard, create a Project, add one or more encrypted Provider Credentials, and generate a Virtual API Key for the project. Their application points its LLM requests to the gateway and authenticates with the virtual key.

For each request, the gateway authenticates the caller, applies configured traffic policies, chooses the appropriate provider/model route, proxies the request, streams tokens to the client when requested, and captures usage and operational metadata. The request is attributed to the correct project, virtual key, provider credential, provider, and model.

The developer then opens the dashboard to inspect recent requests, token usage, estimated cost, latency, TTFT, errors, and provider/model breakdowns. When an upstream provider is overloaded or unavailable, the gateway applies explicitly defined retry, circuit-breaker, or fallback behavior rather than hiding failures behind uncontrolled retries.

### Key Touchpoints

1. **Discovery:** Developer recognizes the pain of managing multiple LLM providers, keys, usage records, and reliability behavior.
2. **First Contact:** Project README/demo explains the difference between an LLM-aware gateway and a generic reverse proxy.
3. **Onboarding:** Create project → add provider credential → create virtual key → copy gateway endpoint and key.
4. **Core Loop:** Application sends LLM requests → gateway proxies/streams → usage and operational metadata are recorded → developer inspects dashboard.
5. **Retention:** The gateway becomes the stable control point for changing providers, managing keys, troubleshooting failures, and understanding cost/usage.

## MVP Features

### Core Features (Must Have)

#### 1. Multi-Provider Gateway API

- **Description:** Expose a unified gateway API that can route requests to at least OpenAI, Anthropic, and DeepSeek through provider adapters while preserving necessary provider-specific behavior.
- **User Story:** As a developer, I want one gateway endpoint for several LLM providers so that my applications do not need separate infrastructure and credentials for each provider.
- **User Value:** Reduces direct provider coupling and creates a single operational control point.
- **Success Criteria:**
  - At least OpenAI, Anthropic, and DeepSeek can be called through the gateway in end-to-end integration tests.
  - Provider adapters normalize only concepts that are genuinely common while allowing provider-specific capabilities to remain explicit.
  - Provider errors are mapped into stable gateway error categories without discarding useful upstream details.
  - The gateway documents which API surface is OpenAI-compatible and which capabilities remain provider-specific.
- **Priority:** Critical

#### 2. Project, Virtual API Key, and Provider Credential Management

- **Description:** Allow users to create projects, store provider credentials securely, generate revocable virtual gateway API keys, and attribute traffic to the correct project and key.
- **User Story:** As a developer, I want project-scoped virtual keys so that provider secrets are not distributed across every application and usage can be attributed correctly.
- **User Value:** Centralizes credential governance and creates a durable identity for traffic attribution.
- **Success Criteria:**
  - Users can create, list, disable, rotate, and revoke virtual API keys.
  - Virtual API keys are randomly generated, prefix-identifiable, displayed once, and stored in a non-recoverable hashed representation.
  - Provider credentials are stored using recoverable encryption because the gateway must use them for upstream calls.
  - Requests can be attributed to project, virtual key, provider credential/account, provider, and model.
  - Sensitive secrets and authorization headers are redacted from logs.
- **Priority:** Critical

#### 3. Streaming / SSE Proxy

- **Description:** Proxy LLM streaming responses without buffering the full response, while handling flushing, client disconnects, context cancellation, upstream cancellation, backpressure, and end-of-stream accounting.
- **User Story:** As an application developer, I want streaming responses to pass through the gateway with minimal additional delay so that user-perceived model responsiveness is preserved.
- **User Value:** Makes the gateway usable for real interactive LLM workloads rather than only non-streaming APIs.
- **Success Criteria:**
  - Streaming works end-to-end for the supported providers where their APIs support it.
  - The gateway does not intentionally buffer the entire streaming response before sending data downstream.
  - Client cancellation propagates to the upstream request.
  - Streaming tests cover client disconnects, slow consumers, upstream termination, and errors after headers have been sent.
  - TTFT and stream completion timing can be measured.
- **Priority:** Critical

#### 4. Usage, Cost, and Request Attribution

- **Description:** Persist durable usage records containing request identity, project/key/provider/model attribution, token usage, estimated cost, latency, TTFT, streaming state, status, error type, and retry count where available.
- **User Story:** As a developer, I want every gateway request to produce an attributable usage record so that I can understand where LLM traffic and spending come from.
- **User Value:** Solves the central fragmentation problem across providers and projects.
- **Success Criteria:**
  - Successful requests generate a durable usage record.
  - Usage records include project, virtual key, provider, provider account/credential, model, timestamps, latency, status, and streaming flag.
  - Prompt/completion/total token counts are stored when the provider supplies them.
  - Estimated cost is calculated from an explicit model pricing table/version rather than hard-coded ad hoc logic.
  - Usage collection works for streaming requests where token usage may only become available at stream completion.
  - Dashboard queries can aggregate requests, tokens, estimated cost, latency, and errors by project/provider/model over time.
- **Priority:** Critical

#### 5. Reliability and Observability Baseline

- **Description:** Implement the minimum production-credible reliability and observability mechanisms required to understand and safely operate the gateway.
- **User Story:** As an operator, I want rate limiting, controlled retries, clear failure behavior, metrics, and traces so that upstream problems do not turn into uncontrolled cost, latency, or outages.
- **User Value:** Demonstrates predictable behavior under failures instead of merely forwarding HTTP requests.
- **Success Criteria:**
  - Rate limiting exists at least at the virtual-key or project boundary.
  - Retries are limited to explicitly classified retryable failures, respect context deadlines and `Retry-After`, use bounded backoff/jitter, and avoid retrying after a stream has started.
  - Retry behavior documents the risk of duplicate provider cost.
  - Circuit breaking and fallback are included only if their semantics are validated during technical design; they are not added merely for feature count.
  - Prometheus-compatible metrics expose traffic, latency, TTFT, error, retry, and token/cost-oriented aggregates without high-cardinality request/user labels.
  - OpenTelemetry traces connect gateway request handling to upstream provider calls and usage recording.
  - Grafana (or equivalent) provides a minimal operational dashboard.
  - Failure tests cover upstream timeout, 429, 5xx, connection reset, client cancellation, streaming disconnect, Redis failure if Redis is used, and PostgreSQL failure.
- **Priority:** Critical

## Out of Scope (Not in MVP)

| Feature | Why Wait | Planned For |
|---|---|---|
| Autonomous agents / planner-executor workflows | Not part of the gateway problem and would dilute the backend focus | Not planned unless product scope changes |
| AI-based routing or AI-generated policy | Adds cost and nondeterminism without a validated gateway need | Later only with evidence |
| Kafka-based usage pipeline | Not justified until simple async/batched persistence is demonstrably insufficient | Version 2 / scale-triggered |
| Kubernetes | Deployment complexity does not improve the core MVP unless actual scale/operations require it | Later |
| Microservice decomposition | A modular monolith or small number of binaries is more appropriate for a 3-month project | Later if independent scaling requires it |
| Service mesh | Duplicates infrastructure concerns without solving the LLM-specific problem | Skip for MVP |
| Complex enterprise RBAC / IAM | Excessive scope for initial users; simple project/tenant authorization is enough | Version 2 |
| Complex billing/invoicing system | Usage and cost attribution matter; charging customers is a separate business system | Version 2 |
| Heavy frontend analytics platform | Backend gateway quality is the primary goal | Version 2 |
| Custom distributed consensus or message queue | High implementation risk with little product value | Skip |
| Custom circuit-breaker implementation when a mature library is suitable | Reinvents reliability primitives unnecessarily | Skip unless used as a learning experiment outside production path |
| Multi-region active-active deployment | Premature for MVP and difficult to validate meaningfully | Later |
| Native support for a large catalog of providers | Three well-supported providers provide more value than many shallow adapters | Later |

## Success Metrics

### Primary Metrics

1. **End-to-End Provider Support:** 3 production API providers supported by MVP completion.
   - How to measure: Automated integration tests and manual smoke tests for OpenAI, Anthropic, and DeepSeek.
   - Why it matters: Proves the provider abstraction is real rather than a single-provider proxy with unused interfaces.

2. **Attributable Gateway Traffic:** 100% of successful requests in the end-to-end test suite produce a queryable usage record linked to project, virtual key, provider, and model.
   - How to measure: E2E tests from Virtual API Key → Gateway → Mock/real Provider → Usage Record.
   - Why it matters: Directly validates the product's core usage-governance problem.

3. **Production-Grade Streaming Path:** Streaming passes all defined integration and failure scenarios before MVP completion.
   - How to measure: Automated tests for chunk delivery, client disconnect, upstream cancellation, slow consumer behavior, and stream completion accounting.
   - Why it matters: Streaming is one of the strongest LLM-specific gateway requirements and a major backend engineering signal.

4. **Measured Performance Evidence:** A reproducible benchmark report exists before release and contains real P50/P95/P99 gateway-added latency, TTFT overhead, throughput/concurrency, CPU, memory, allocation, and error-rate results.
   - How to measure: Compare Client → Mock Provider against Client → Gateway → Mock Provider for streaming and non-streaming workloads using documented hardware and load parameters.
   - Why it matters: Prevents unverified “high-performance” claims and creates defensible resume metrics.
   - Numeric target: **TBD after baseline benchmark**; final values must come from measurement rather than invention.

### Secondary Metrics

- At least one reproducible failure/degraded-mode test for each critical dependency used in the MVP.
- Integration tests cover PostgreSQL, provider adapters, gateway API, virtual-key authentication, and streaming.
- Prometheus metrics, OpenTelemetry traces, and a Grafana operational dashboard are available in the demo environment.
- `pprof` CPU and memory profiles are captured for representative benchmark workloads.
- Project documentation explains at least 10 major architectural decisions and trade-offs well enough to support interview discussion.
- No provider API secret or full virtual API key is intentionally emitted into application logs.
- The primary demo user journey can be completed end-to-end without manual database edits.

## UI/UX Direction

**Design Feel:** Clean, fast, professional, infrastructure-oriented, low-distraction.  
**Inspiration:** Developer infrastructure consoles such as API management, observability, and cloud control-plane dashboards rather than consumer chat applications.

### Key Screens

1. **Overview**
   - Purpose: Give a compact operational summary.
   - Key Elements: Request volume, tokens, estimated cost, latency/TTFT, error rate, provider/model breakdown.
   - User Actions: Change time range, filter by project/provider/model, navigate to details.

2. **Projects**
   - Purpose: Manage the logical boundary used for traffic ownership and attribution.
   - Key Elements: Project name, status, virtual keys, provider configuration summary, recent usage.
   - User Actions: Create/edit project, open project details.

3. **Virtual API Keys**
   - Purpose: Create and manage gateway credentials.
   - Key Elements: Key prefix, project, status, creation time, last-used time if available.
   - User Actions: Create, disable, rotate, revoke.
   - Security Rule: Full secret is shown only at creation.

4. **Provider Credentials**
   - Purpose: Configure recoverable upstream provider credentials.
   - Key Elements: Provider, credential label, masked identifier, status, validation state.
   - User Actions: Add, rotate, disable, test/validate where appropriate.
   - Security Rule: Raw provider secret is never displayed after submission.

5. **Usage & Cost**
   - Purpose: Explain where LLM consumption comes from.
   - Key Elements: Requests, prompt/completion tokens, estimated cost, trends, project/provider/model breakdown.
   - User Actions: Filter and inspect aggregates.

6. **Requests**
   - Purpose: Debug recent gateway traffic without storing sensitive prompt bodies by default.
   - Key Elements: Request ID, timestamp, project, virtual key prefix, provider, model, status, latency, TTFT, token count, retry count, error category.
   - User Actions: Filter, open metadata detail, follow trace ID.

7. **Observability**
   - Purpose: Surface operational health.
   - Key Elements: Error rate, latency percentiles, TTFT, active requests, retries, provider health, circuit state if implemented.
   - User Actions: Navigate to Grafana/tracing views where appropriate.

### Design Principles

- **Backend-first:** UI exists to operate and demonstrate the gateway; frontend work must not dominate the roadmap.
- **Operational clarity:** Prefer tables, filters, status indicators, and time-series summaries over decorative visuals.
- **Safe by default:** Never expose secrets or raw prompt/response bodies by default.
- **Progressive detail:** Overview first; request-level detail only when debugging.
- **Responsive enough, not mobile-first:** Desktop is the primary operating environment; mobile should remain usable but is not a core optimization target.

## Technical Considerations

**Platform:** Web management dashboard + Go backend gateway/control plane.  
**Responsive:** Yes; desktop-first.

**Gateway Performance Goals:**
- Minimize gateway-added latency rather than optimize for vanity raw QPS.
- Minimize added TTFT for streaming workloads.
- Avoid full-response buffering on the streaming path.
- Reuse upstream connections through correctly configured Go `http.Transport`.
- Propagate request cancellation and deadlines to upstream providers.
- Benchmark concurrent streaming connections on documented hardware.
- Final numerical latency/concurrency targets are **TBD after the baseline benchmark**.

**Web Performance Goals:**
- Primary dashboard views should load quickly under expected MVP data volume.
- Prefer server-side aggregation/pagination over transferring unbounded request history.
- Avoid frontend complexity that requires a dedicated analytics architecture during MVP.

**Security/Privacy:**
- TLS for all non-local traffic.
- Gateway virtual API keys stored as hashes and shown once.
- Provider secrets stored using recoverable encryption; key-management mechanism finalized in technical design.
- Authorization headers, cookies, API keys, and provider secrets redacted from logs and traces.
- Raw prompt/response bodies are **not retained by default**.
- Tenant/project ownership must be enforced in backend authorization and database queries.
- Sensitive configuration changes should be auditable.

**Scalability:**
- Architecture should be production-credible but optimized for a 3-month portfolio project rather than hyperscale.
- PostgreSQL is expected to be the source of truth for durable control-plane state and usage records.
- Hot-path database access should be minimized through in-memory/config caching where justified.
- Redis is added only when a concrete need such as distributed rate limiting or shared cache semantics justifies it.
- Kafka is not an MVP dependency.

**Architecture Constraint:**
- Control Plane and Data Plane should be conceptually separated.
- Physical deployment as one modular system or a small number of binaries is preferred initially; microservices are not an MVP requirement.
- The Technical Design Document will define exact package boundaries, runtime topology, data flow, and failure semantics.

## AI / Automation Scope

**AI Surface:** No autonomous AI feature in the product. AI is used as an assistant during development through Coding Agents.  
**Allowed Data:** Coding Agents may read repository code, specs, tests, and development documentation. Production provider secrets and real sensitive user payloads should not be intentionally exposed to development assistants.  
**Provider / Retention:** Not a product requirement; development-assistant provider/account settings should follow the developer's chosen privacy and retention controls.  
**Output Contract:** Coding Agent output is code, tests, documentation, and review suggestions governed by repository specs and human code review.  
**Confirmation Rules:** Architecture changes, schema changes, security-sensitive code, dependency additions, destructive migrations, and production/deployment changes require explicit human review.  
**Verification Prompts:** Each implementation milestone should be followed by code review, concept explanation, tests, failure checks, and benchmark/profiling where relevant.

## Browser/Device Support

- Chrome, Edge, Firefox, and Safari: current major desktop versions.
- Mobile browsers: usable responsive layout; no mobile-native app.
- Tablet optimized: Not required for MVP.
- Primary development/demo environment: Desktop browser.

## Constraints & Requirements

### Budget

- **Development tools:** Flexible.
- **Hosting/Infrastructure:** Flexible; prefer simple/self-hosted or low-cost managed services until a managed option provides clear engineering value.
- **Third-party services:** Allowed when they materially improve engineering quality, realism, deployment, observability, or portfolio value.
- **Total:** No fixed monthly cap defined; unnecessary expensive infrastructure should be avoided.

### Timeline

- **MVP Development:** 12 weeks / approximately 3 months.
- **Beta Testing:** Integrated into the final development phase rather than a separate consumer-product beta.
- **Launch Target:** End of the 3-month project window.

### Technical Constraints

- Gateway core is primarily implemented in Go.
- Backend and gateway engineering quality take priority over frontend scope.
- The system must support real HTTP streaming/SSE behavior.
- PostgreSQL is expected to serve as the durable source of truth unless the Technical Design Document identifies a concrete exception.
- Redis, Kafka, Kubernetes, and other infrastructure are not added for resume keyword value.
- Provider abstraction must not erase meaningful provider-specific capabilities.
- Generic API-gateway concerns should not be reimplemented without reason; the project must focus engineering depth on LLM-specific concerns.
- The system must be benchmarkable against a mock provider baseline.
- Important architectural choices must be documented so they can be explained without relying on generated code.

## Open Questions & Assumptions

### Open Questions

- **Product differentiation:** The attached research material defines a competitor-analysis task but does not contain the completed findings. The final positioning versus LiteLLM, Portkey, Helicone, OpenRouter, Cloudflare AI Gateway, and generic API gateways remains to be validated.
- **Public API shape:** Whether the MVP should expose only an OpenAI-compatible API, native provider APIs, or a mixed strategy will be finalized in technical design.
- **Redis:** Whether Redis enters MVP immediately or only after distributed rate-limit/config-cache requirements are demonstrated.
- **Circuit breaker:** Whether to include it in MVP and which mature Go implementation to use.
- **Fallback semantics:** Which models/providers may safely substitute for each other and when fallback should be disabled.
- **Usage ingestion:** Direct/batched PostgreSQL writes versus a lightweight async worker/outbox design.
- **Cost pricing data:** How model pricing is versioned and updated without coupling request handling to volatile pricing tables.
- **Authentication for Web users:** Exact login/session mechanism for the management dashboard.
- **Multi-tenancy boundary:** Whether MVP models only user→project ownership or introduces an explicit Organization/Tenant entity.
- **Deployment target:** Exact hosting environment for the public demo.
- **Frontend stack:** To be chosen for minimal distraction from backend work.

### Assumptions

- The primary value of the MVP is technical credibility and developer utility, not near-term revenue.
- Three providers implemented deeply are more valuable than many shallow provider adapters.
- Backend interviews and portfolio review will value measurable engineering evidence more than infrastructure keyword count.
- Prompt and response body retention is not necessary for the core product and should remain disabled by default.
- A modular monolith or small number of deployable binaries is sufficient for the MVP.
- Performance claims will be based only on reproducible measurements from the completed implementation.

## Quality Standards

### Code Quality

- Use idiomatic Go and standard library facilities where they are appropriate; do not introduce frameworks without a concrete benefit.
- Handle errors explicitly and preserve actionable context without leaking secrets.
- Propagate `context.Context` correctly through request, provider, repository, and background work boundaries.
- Avoid goroutine and connection leaks.
- Use interfaces at real architectural boundaries, especially provider/repository abstractions, not for every type.
- Schema migrations are version-controlled and reproducible.
- Dependencies are reviewed for maintenance quality, security, and necessity.
- Critical logic has tests before the feature is considered complete.

### Design Quality

- Consistent layout, spacing, typography, and component behavior.
- Desktop-first but responsive.
- Accessibility basics: labels, keyboard navigation for critical controls, sufficient contrast, meaningful error states.
- No decorative frontend work that delays gateway engineering.

### Documentation Quality

- Architecture diagrams describe actual implemented topology.
- API examples work against the current version.
- Failure semantics are documented.
- Benchmark methodology records hardware, workload, concurrency, mock-provider behavior, and measurement method.
- Resume claims must map to reproducible evidence.

### What This Project Will NOT Accept

- Placeholder features that exist only in UI.
- “Production-grade” claims unsupported by tests or failure semantics.
- “High-performance” claims unsupported by benchmark data.
- Secrets in logs, traces, screenshots, fixtures, or committed configuration.
- Raw prompt/response retention enabled by default.
- Infrastructure added only to inflate the technology list.
- Blind Coding Agent changes that the project owner cannot explain.
- Half-working reliability features that hide errors or create uncontrolled duplicate LLM cost.

## Risk Mitigation

| Risk | Impact | Mitigation Strategy |
|---|---|---|
| Scope becomes too large for 3 months | High | Keep five core capabilities; aggressively defer Kafka, Kubernetes, complex RBAC, billing, and broad provider catalogs |
| Project becomes a LiteLLM clone without a clear reason to exist | High | Complete competitor validation and center the project on measurable Go gateway engineering, streaming, attribution, reliability semantics, and benchmark evidence |
| Provider APIs diverge more than the abstraction allows | High | Maintain a common core plus explicit provider-specific extensions; avoid lowest-common-denominator design |
| Streaming path leaks goroutines/connections or buffers responses | High | Integration tests for cancellation/disconnects, load tests, `pprof`, connection metrics, and code review focused on lifecycle management |
| Retries cause duplicate LLM charges | High | Retry only classified failures, cap attempts, stop retries after streaming starts, expose retry metadata, respect deadlines and provider hints |
| Usage data is lost or slows the hot path | High | Define explicit durability semantics; start with the simplest design that meets them, then introduce async/batched persistence only if benchmark evidence justifies it |
| PostgreSQL becomes a hot-path bottleneck | Medium | Avoid unnecessary per-chunk writes; batch/async usage persistence where justified; cache relatively static configuration |
| Redis becomes a critical dependency accidentally | Medium | Define fail-open/fail-closed semantics by feature; keep PostgreSQL as source of truth; do not place recoverability solely in Redis |
| Metrics create cardinality explosions | Medium | Never label Prometheus series with request IDs; strongly limit labels such as user/project IDs; use logs/traces/durable records for high-cardinality identifiers |
| Security flaw exposes provider credentials | High | Encrypt provider secrets, hash virtual keys, redact telemetry, enforce project authorization, add security-focused integration tests |
| Frontend consumes too much time | Medium | Use a minimal component stack; limit screens to operational workflows required for the demo |
| Benchmark numbers are misleading | High | Use controlled mock-provider baselines, publish methodology, compare direct vs gateway paths, and avoid claims unsupported by repeated tests |
| Coding Agent creates code the owner cannot explain | High | Maintain `spec/`, require review/explanation after each milestone, and treat tests/benchmarks as acceptance gates |

## MVP Completion Checklist

### Development Complete

- [ ] OpenAI, Anthropic, and DeepSeek are supported through the gateway.
- [ ] Project management works.
- [ ] Virtual API key creation, authentication, disable/rotation/revocation work.
- [ ] Provider credential storage and use work without exposing raw secrets.
- [ ] Streaming/SSE works end-to-end.
- [ ] Client cancellation propagates upstream.
- [ ] Durable request/usage attribution works.
- [ ] Token and estimated cost accounting work for supported cases.
- [ ] Rate limiting works at the agreed boundary.
- [ ] Retry semantics are implemented and tested.
- [ ] Circuit breaker/fallback are either implemented with documented semantics or explicitly deferred.
- [ ] Web Dashboard supports the core operating workflow.
- [ ] Basic error handling and graceful shutdown work.

### Launch Ready

- [ ] Deployment is reproducible.
- [ ] Environment/secrets configuration is documented.
- [ ] Database migrations run automatically or through a documented release step.
- [ ] Prometheus metrics are exposed.
- [ ] OpenTelemetry traces are available.
- [ ] Grafana operational dashboard is available.
- [ ] README includes architecture overview, setup, API usage, screenshots, and benchmark links.
- [ ] Security/privacy defaults are documented.
- [ ] Public demo does not expose real provider credentials or sensitive prompts.

### Quality Checks

- [ ] Unit tests cover routing/provider normalization, rate-limit logic, cost calculation, credential validation, and other pure logic.
- [ ] Integration tests cover PostgreSQL, gateway endpoints, virtual-key auth, and provider adapters using mock upstream servers.
- [ ] Failure tests cover upstream timeout, 429, 5xx, connection reset, client cancellation, and streaming disconnect.
- [ ] Redis/PostgreSQL degraded-mode behavior is tested if Redis is present.
- [ ] Core journey works end-to-end: Virtual API Key → Gateway → Provider → Usage Record → Dashboard.
- [ ] No known critical security bugs.
- [ ] `go test -race` passes for relevant packages/workloads.
- [ ] Benchmark report compares direct mock-provider access with the gateway path.
- [ ] Streaming and non-streaming benchmarks are included.
- [ ] CPU and memory profiles have been reviewed with `pprof`.
- [ ] Final performance/resume claims use measured values only.

### Learning / Interview Ready

- [ ] Can explain why an LLM Gateway is not merely Nginx/Envoy.
- [ ] Can explain the provider abstraction and its limits.
- [ ] Can explain Go `http.Server`, `http.Transport`, keep-alive, connection reuse, and cancellation.
- [ ] Can explain SSE streaming, flushing, buffering, and backpressure.
- [ ] Can explain retry safety and duplicate-cost risk.
- [ ] Can explain Redis vs PostgreSQL failure semantics.
- [ ] Can explain logs vs metrics vs traces and Prometheus cardinality.
- [ ] Can explain virtual-key hashing vs provider-secret encryption.
- [ ] Can explain data-model/index choices for usage attribution.
- [ ] Can explain benchmark methodology and the real measured bottlenecks.

## Next Steps

1. **Immediate:** Review and approve this PRD.
2. **Next:** Create the Technical Design Document (Part 3).
3. **Then:** Convert technical decisions into a repository `spec/` directory.
4. **Build:** Implement milestone-by-milestone with Coding Agent assistance.
5. **Review:** Perform code review and concept explanation after each major implementation.
6. **Verify:** Run integration/failure tests and benchmark gates.
7. **Launch:** Deploy the demo and package architecture/benchmark evidence for the portfolio.

---
*Created: 2026-08-28*  
*Status: Ready for Technical Design*

---
## Handoff Context
<!-- Machine-readable summary for the next workflow step. Do not delete; the next prompt in the workflow reads this block. -->
- Stage: prd
- App name: Production Go LLM Gateway
- User level: C
- Target platform: web + backend gateway
- Budget: flexible
- Timeline: 3 months
- AI in product scope: no; AI-assisted development only
- Source files: research-Production-Go-LLM-Gateway.md → PRD-Production-Go-LLM-Gateway-MVP.md

---

### Machine-Readable Summary

```json
{
  "appName": "Production Go LLM Gateway",
  "oneLiner": "A production-oriented Go LLM gateway that unifies multi-provider access, virtual keys, request attribution, usage/cost visibility, streaming, and reliability controls.",
  "targetUsers": "Developers, small engineering teams, and backend/AI infrastructure teams using multiple LLM providers",
  "phase": "Foundation",
  "mustHave": [
    "Multi-provider gateway API",
    "Project, virtual API key, and provider credential management",
    "Streaming/SSE proxy",
    "Usage, cost, and request attribution",
    "Reliability and observability baseline"
  ],
  "niceToHave": [
    "Circuit breaker if justified",
    "Safe model/provider fallback",
    "Redis-backed distributed rate limiting if justified"
  ],
  "notInMvp": [
    "Kafka",
    "Kubernetes",
    "Microservices",
    "Service mesh",
    "Complex enterprise RBAC",
    "Autonomous agents",
    "AI-based routing",
    "Complex billing",
    "Heavy frontend analytics"
  ],
  "successMetrics": [
    "3 providers supported end-to-end",
    "100% successful E2E test requests attributable to project/key/provider/model",
    "Streaming failure scenarios pass",
    "Reproducible P50/P95/P99 latency and TTFT benchmark report",
    "Prometheus, OpenTelemetry, Grafana, integration tests, failure tests, and pprof evidence available"
  ]
}
```
