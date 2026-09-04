# Provider Capability Matrix

Status: Week 6 (Anthropic Adapter). Contract facts for Anthropic were verified against the
official Anthropic Messages API, Messages Streaming, and Errors documentation
(docs.anthropic.com/en/api/messages, .../messages-streaming, .../errors) and the
anthropic-sdk-typescript source (main, src/resources/messages/messages.ts), all fetched 2026-09.
The OpenAI/DeepSeek rows carry over from Week 3-5 evidence. Provider API behavior is
fast-moving; treat this matrix as code/data that must be re-verified before launch.

## Scope of this document

The gateway exposes one documented OpenAI-compatible Chat Completions subset
(`model`, string-content `messages`, optional `max_tokens`, `stream` true/false). Each
provider adapter owns its wire format, capability validation, and error classification.
This matrix records where the supported providers genuinely differ so the differences stay
explicit instead of being silently erased by a lowest-common-denominator abstraction.

## Capability matrix

| Capability | OpenAI (`internal/provider/openai`) | DeepSeek (`internal/provider/deepseek`) | Anthropic (`internal/provider/anthropic`) |
|---|---|---|---|
| Wire family | OpenAI-compatible (`internal/provider/oaiwire`) | OpenAI-compatible (`internal/provider/oaiwire`) | Messages API — **not** OpenAI-compatible; the adapter imports neither `oaiwire` nor its wire types, only `provider` and `provider/sse` |
| Default base URL | `https://api.openai.com` | `https://api.deepseek.com` | `https://api.anthropic.com` |
| Chat path | `/v1/chat/completions` | `/chat/completions` | `/v1/messages` |
| Auth headers | `Authorization: Bearer <key>` | `Authorization: Bearer <key>` | `x-api-key: <key>` + required `anthropic-version: 2023-06-01` (never `Authorization`) |
| `max_tokens` | Forwarded only when the client sends it (Week 6 subset) | Forwarded only when the client sends it; DeepSeek applies its own server default otherwise | **Required** by the Messages API; the gateway default is `4096` when the client omits it (`defaultMaxTokens`) |
| `system` messages | Sent inside `messages` with role `system` | Same as OpenAI | Hoisted into the top-level `system` field (joined with `"\n\n"`); `messages` carry only `user`/`assistant` because the Messages API models turns as user/assistant alternation |
| Response envelope | Passthrough: `id`/`object`/`created`/`model` from upstream | Passthrough | Anthropic has no `object`/`created`; the adapter synthesizes `object: "chat.completion"` and `created: now`, and maps `stop_reason` onto OpenAI finish reasons (`end_turn→stop`, `max_tokens`/`model_context_window_exceeded→length`, `stop_sequence→stop`, `tool_use→tool_calls`, unknown→`""`) |
| Response content | `choices[].message.content` string | Same | `content` is an array of blocks; only `type:"text"` blocks are forwarded, joined without a separator so non-stream equals the concatenation a stream of text deltas would produce |
| Streaming usage request | Sends `stream_options.include_usage: true` | Sends no `stream_options`; official docs state the last chunk carries usage either way | No request flag; usage arrives as events |
| Stream framing | Unnamed `data:` events | Unnamed `data:` events | **Named** SSE events (`event: message_start` etc.) |
| Stream terminal semantics | Final usage on a dedicated chunk with `choices: []`, followed by `data: [DONE]`. Usage accepted only from `choices=[]`; any later chunk is malformed | Usage rides on the final content chunk (exactly one choice, empty content, non-null `finish_reason`), followed by `data: [DONE]`. A `choices=[]` usage chunk or usage on a chunk with content is malformed | Event flow: `message_start` (carries `usage.input_tokens`) → content blocks (`content_block_start`/`content_block_delta`/`content_block_stop`) → one or more `message_delta` (carries finish `stop_reason` and **cumulative** `usage.output_tokens`) → final `message_stop`. There is **no `data: [DONE]`**; the adapter synthesizes `[DONE]` at `message_stop` and commits usage only then |
| Stream content shape | Byte passthrough of provider chunks | Byte passthrough | Content is **re-encoded**: text deltas are synthesized into OpenAI-compatible chunk envelopes (`id`/`object:"chat.completion.chunk"`/`created`/`model`/`choices[].delta.content`, role marker on the first chunk, finish chunk on the stop reason), so downstream clients keep seeing the documented OpenAI-compatible subset |
| `[DONE]` marker | Only success marker; stream interrupted before it is never a success | Same | Same, but `[DONE]` is synthetic (emitted on `message_stop`); a stream ending before `message_stop` is never a success |
| Usage fields stored | `prompt_tokens`, `completion_tokens`, `total_tokens` | Same three are stored; DeepSeek additionally reports `prompt_cache_hit_tokens`/`prompt_cache_miss_tokens` and `completion_tokens_details.reasoning_tokens`, not separately persisted — revisit when Week 7 pricing decides cache/reasoning pricing | Same three are stored, mapped from `input_tokens`→`prompt_tokens`, `output_tokens`→`completion_tokens`; `total_tokens` is **computed** (Anthropic does not report it). Cache/thinking breakdowns (`cache_*`, `output_tokens_details.thinking_tokens`) are not separately persisted |
| Reasoning / thinking content | No reasoning channel in the subset | DeepSeek V4 thinking defaults on; non-stream drops `reasoning_content` (documented limitation), streaming passes chunks through | Gateway never enables Anthropic thinking (not in the subset), so `thinking` blocks are not expected; non-text content blocks and deltas (`thinking_delta`, `signature_delta`, `input_json_delta`, tool blocks) are **skipped gracefully**, never merged into the text channel |
| Unknown event/type handling | Malformed/unknown chunk shapes fail the stream | Same | Anthropic's versioning policy says new event types may be added and must be handled gracefully, so unknown **named events** and unknown delta types are skipped; a bare `data:` frame with no event name is outside the contract and fails loudly |
| Error JSON envelope | OpenAI `{"error": {"message", "type", "param", "code"}}` | Official docs document status codes only; message extraction is best-effort via the generic `error.message` decode | `{"type":"error","error":{"type","message","request_id"}}`; message extraction from `error.message`; live smoke test should confirm before launch |
| Upstream request ID header | `X-Request-ID`, falling back to `OpenAI-Request-ID` | Not documented by official docs; best effort via `X-Request-ID` | Documented: every response carries a `request-id` header (also present as `request_id` in error bodies) |
| HTTP status classification | 401/402 -> `provider_unavailable`; 429 -> `provider_rate_limited`; 408/504 -> `provider_timeout`; other 4xx -> `provider_invalid_request`; 5xx -> `provider_unavailable` | Same mapping as OpenAI | Documented statuses mapped explicitly: 400/404/409/413 -> `provider_invalid_request`; 429 -> `provider_rate_limited`; 504 (and 408 defensively) -> `provider_timeout`; 401/402/403 (server-side credential/billing/permission) and 500/529 -> `provider_unavailable` |
| Keep-alive / pre-inference output | Not documented | Non-streaming may emit empty lines; streaming may emit SSE comment lines (`: keep-alive`) | Streaming may emit `ping` events at any point; both are skipped |
| Model namespace | `openai/<model-id>` | `deepseek/<model-id>` | `anthropic/<model-id>`; adapter does not whitelist model IDs (catalog is data, Week 7) |
| Response header for attribution | Adapter tests assert `X-Request-ID` | Adapter tests assert `X-Request-ID` | `request-id` is captured into `upstream_request_id` (`anthropic` `TestCompleteChatTranslatesRequestAndNormalizesResponse`; data plane finalization assertions) |

## Public-subset changes

- Week 6 added an optional integer `max_tokens` to the OpenAI-compatible subset
  (`internal/dataplane/http.go` decode accepts it and requires a positive value; absent stays
  absent on the wire). OpenAI/DeepSeek forward it unchanged when present; Anthropic maps it
  onto its required `max_tokens` and applies the documented `defaultMaxTokens` (4096) when
  the client omits it. Rejected `max_tokens <= 0` fails before any upstream work.

## Not supported by the gateway subset (rejected before any provider)

- Non-string message content (image/content parts) — the subset only accepts string `content`.
- `tools`, `tool_choice`, `response_format`, `thinking`, `reasoning_effort`, `temperature`, etc.
  — the gateway rejects unknown meaningful request fields with `unsupported_parameter`
  instead of forwarding provider-specific knobs silently.
- Automatic cross-provider fallback, model aliasing, circuit breaking (post-MVP).

## Verifying the matrix

Each row above must be traceable to an adapter constant, a decoder rule, or a named test.
When a provider changes its contract, update this matrix and the affected adapter tests in
the same change. Real-provider smoke tests remain opt-in, secret-gated, and approval-gated;
rows flagged "live smoke test should confirm" (Anthropic error envelope fields, plus the
existing DeepSeek error envelope / request-ID uncertainty) must be confirmed before launch.
