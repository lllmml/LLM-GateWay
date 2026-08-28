# Testing

## Required Before Completion

- [ ] Relevant unit, integration, failure, and race tests pass for the changed path.
- [ ] Typecheck, lint, and build pass.
- [ ] User-visible changes are checked in a desktop browser and at a usable mobile viewport.
- [ ] Streaming changes cover cancellation, slow consumers, upstream termination, malformed SSE, and errors after headers.
- [ ] Security-sensitive changes prove secrets and prompt/response bodies do not appear in logs, traces, or API responses.
- [ ] No tests were skipped, weakened, or bypassed without human approval.
- [ ] Evidence is reported in the final response.

## Commands

- All tests: `make test`
- Single Go test: `go test ./path/to/package -run '^TestName$'`
- Single frontend test: `cd web && npm test -- --run <pattern>`
- Typecheck: `make typecheck`
- Lint/format: `make lint`
- Build: `make build`
- Integration: `make integration`
- Race: `make race` (must include `go test -race ./...`)
- Benchmark/profile: `make bench`
- Browser/device check: Run `make dev`; manually complete project → provider credential → virtual key → gateway request → usage/request inspection in a current desktop browser, then check a mobile viewport.

If a Make target is not implemented yet, the current foundation task must create it before downstream agents rely on it.

## What To Test

| Change type | Minimum check |
|-------------|---------------|
| Pure logic | Unit tests for validation, classification, retry decisions, pricing, hashing/encryption, and rate-limit policy |
| Provider/API data flow | `httptest.Server` integration test using the deterministic mock provider |
| PostgreSQL data flow | Real PostgreSQL integration test covering migrations, ownership, create/finalize, aggregation, and dependency failure |
| Streaming | Chunk/flush behavior, delayed first token, cancellation, slow consumer, malformed event, mid-stream disconnect, and usage-at-end |
| Reliability | 429 + `Retry-After`, selected 5xx, timeout/reset, no retry after stream start, retry-count attribution |
| UI behavior | Browser journey, auth-denied state, filtering/pagination, secret shown-once/masked states, responsive usability |
| Auth, crypto, migrations, deployment | Human review plus focused positive/negative/failure tests |
| Observability | Metric label-cardinality review, expected spans/log fields, redaction test, telemetry-backend outage does not break traffic |

## Real-provider and benchmark checks

- Real OpenAI/Anthropic/DeepSeek smoke tests are opt-in, secret-gated, low volume, and require approval because they can incur cost.
- Compare Client → Mock Provider with Client → Gateway → Mock Provider under identical documented workloads.
- Record Go version, commit SHA, hardware, OS, mock settings, payload, concurrency, duration, warmup, transport settings, and repetitions.
- Do not publish P50/P95/P99, TTFT, throughput, or resource claims until measurement exists.

## LLM gateway checks

- Direct request: Authenticated supported request reaches only the selected provider and produces an attributable durable record.
- Untrusted content: Prompt text, retrieved documents, provider output, errors, and tool-shaped payloads are treated only as data and never as gateway instructions.
- Auth required: Missing, invalid, disabled, revoked, or cross-project credentials are rejected before upstream work.
- Failure case: Provider timeout/429/5xx/malformed stream and PostgreSQL/Redis/telemetry outages follow the documented failure matrix.
- Action check: No automatic provider fallback, destructive action, production write, or cost-bearing test occurs without explicit configuration and approval.
- Data check: Secrets, raw virtual keys, auth headers, cookies, and prompt/response bodies must not appear in model-facing development context, logs, traces, metrics, or normal request-history APIs.
