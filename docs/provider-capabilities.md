# Provider Capability Matrix

Status: Week 5 (Provider Abstraction + DeepSeek). Contract facts were verified against the
official DeepSeek API documentation fetched on 2026-09 (https://api-docs.deepseek.com/) and
the OpenAI Chat Completions contract already implemented in Week 3/4. Provider API behavior
is fast-moving; treat this matrix as code/data that must be re-verified before launch.

## Scope of this document

The gateway exposes one documented OpenAI-compatible Chat Completions subset
(`model`, string-content `messages`, `stream` true/false). Each provider adapter owns its
wire format, capability validation, and error classification. This matrix records where the
two supported providers genuinely differ so the differences stay explicit instead of being
silently erased by a lowest-common-denominator abstraction.

## Capability matrix

| Capability | OpenAI (`internal/provider/openai`) | DeepSeek (`internal/provider/deepseek`) | Evidence |
|---|---|---|---|
| Default base URL | `https://api.openai.com` | `https://api.deepseek.com` | adapter `defaultBaseURL` constants |
| Chat completions path | `/v1/chat/completions` | `/chat/completions` | adapter constants; adapter tests assert the upstream path (OpenAI: `/v1/chat/completions`, DeepSeek: `/chat/completions`) |
| Auth header | `Authorization: Bearer <key>` | `Authorization: Bearer <key>` | adapter request tests |
| Streaming usage request | Sends `stream_options.include_usage: true` | Sends no `stream_options`; official docs state the last chunk carries usage either way | OpenAI `TestStreamChatTranslatesRequestAndExtractsUsage`; DeepSeek stream request-body test asserts `stream_options` absent |
| Stream terminal semantics | Final usage arrives on a separate chunk with `choices: []`, followed by `data: [DONE]`. Usage accepted only from `choices=[]`; any later chunk is a malformed sequence | Usage rides on the final content chunk (exactly one choice, empty content, non-null `finish_reason`), followed by `data: [DONE]`. A `choices=[]` usage chunk or usage on a chunk that still carries content is malformed | OpenAI `internal/provider/openai:TestStreamChatRejectsMalformedFinalUsageSequences`; DeepSeek `internal/provider/deepseek:TestStreamChatRejectsMalformedFinalUsageSequences` |
| `[DONE]` marker | Only success marker; stream interrupted before it is never a success | Same | shared data-plane streaming tests |
| Usage fields stored | `prompt_tokens`, `completion_tokens`, `total_tokens` | Same three are stored. DeepSeek additionally reports `prompt_cache_hit_tokens`/`prompt_cache_miss_tokens` (`prompt_tokens = hit + miss`) and `completion_tokens_details.reasoning_tokens`; these breakdowns are **not separately persisted** yet — revisit when Week 7 pricing decides cache/reasoning pricing | `docs/provider-capabilities.md` note; usage extracted from same three fields |
| Reasoning / thinking content | No reasoning channel in the Chat Completions subset | DeepSeek V4 thinking mode is enabled by default and returns `reasoning_content` on messages/deltas. Non-stream responses are normalized to the gateway `ChatResponse` envelope, which has no reasoning field, so `reasoning_content` is intentionally not forwarded (documented limitation). Stream chunks pass through byte-for-byte, so any `reasoning_content` present in provider chunks reaches the client. Gateway never sends `reasoning_content` back (official docs: ignored in subsequent turns when no `tools` are present; the gateway subset rejects `tools`) | DeepSeek `TestCompleteChatNormalizesReasoningResponseToContentOnly`; stream passthrough is covered by `internal/dataplane:TestHandlerStreamsThroughDeepSeekClientOverHTTP` |
| Error JSON envelope | OpenAI `{"error": {"message", "type", "param", "code"}}` | Official docs document status codes only (400/401/402/422/429/500/503), not a JSON envelope; message extraction is best-effort via the same generic `error.message` decode. Live smoke test should confirm before launch | `oaiwire.ClassifyResponseError`; matrix note |
| Upstream request ID header | `X-Request-ID`, falling back to `OpenAI-Request-ID` | Not documented by official docs; best effort via `X-Request-ID`. `upstream_request_id` stays null when absent | adapter request-ID helpers; matrix note |
| HTTP status classification | 429 → `provider_rate_limited`; 408/504 → `provider_timeout`; other 4xx → `provider_invalid_request` (includes DeepSeek 402 insufficient balance); 5xx → `provider_unavailable` | Same generic mapping | `oaiwire.ClassifyResponseError`; shared failure tests |
| Keep-alive / pre-inference output | Not documented | Non-streaming may emit empty lines; streaming may emit SSE comment lines (`: keep-alive`). The SSE decoder ignores comment lines and blank frames | `internal/provider/sse` decoder; DeepSeek stream test interleaves a keep-alive comment |
| Model namespace | `openai/<model-id>` | `deepseek/<model-id>`; adapter does not whitelist model IDs (catalog is data, Week 7) | `provider.ParseModel` + registry tests |

## Not supported by the gateway subset (rejected before any provider)

- Non-string message content (image/content parts) — the subset only accepts string `content`.
- `tools`, `tool_choice`, `response_format`, `thinking`, `reasoning_effort`, `temperature`, etc.
  — the gateway rejects unknown meaningful request fields with `unsupported_parameter`
  instead of forwarding provider-specific knobs silently.
- Automatic cross-provider fallback, model aliasing, circuit breaking (post-MVP).

## Verifying the matrix

Each row above must be traceable to an adapter constant, a decoder rule, or a named test.
When a provider changes its contract, update this matrix and the affected adapter tests in
the same change. Real-provider smoke tests remain opt-in, secret-gated, and approval-gated.
