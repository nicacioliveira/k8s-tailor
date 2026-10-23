# Advisor LLM (optional, read-only)

The advisor asks an LLM for a **second opinion** on Tailor decisions
(`Review` of a recommendation, `Summarize` of a batch). It is **off by
default** and **never decides or acts**: the result is text for a human to read.
Code lives in `internal/advisor/` (stdlib only; no provider SDK).

## Architecture

```
 recommender.Recommendation ──json──▶ advisor.DecisionFromJSON ──▶ DecisionInput
                                                                       │
   LRU+TTL cache ◀─ hit ──┐                                            ▼
                          └── miss ─▶ breaker ▶ token budget ▶ rate limit
                                                                       │
                       aggregated payload (optional anonymization) ◀───┘
                                                                       ▼
                    POST {BASE}/chat/completions  (retry only 429/5xx, temp. 0)
                                                                       ▼
        extract JSON ▶ validate enums/sizes ▶ validate cited_figures ▶ restore names
                                                                       ▼
                                                  Review / Summary (or typed error)
```

Main components:

| Component | Behavior |
|---|---|
| Client | `POST {BASE_URL}/chat/completions`, `Authorization: Bearer` (omitted without a key), extra headers, `temperature: 0`, `max_tokens`, per-attempt timeout, bounded response body (1 MiB). |
| `response_format` | Starts at `json_schema` (strict). If the provider returns 400/422 mentioning the parameter, it downgrades to `json_object` and then `none`, and remembers the choice. In any mode the JSON is extracted from the text (tolerates \`\`\` fences, prefixes and `<think>` blocks). |
| Retry | Only 429 and 5xx; exponential backoff with jitter; honors `Retry-After` (30 s cap). Timeouts and 4xx are not retried. |
| Cache | LRU (256) + TTL (15 min) keyed by SHA-256 of input + model + language + anonymization. Invalid responses never enter the cache. |
| Rate limit / budget | 1 h sliding window: provider calls (`ADVISOR_MAX_CALLS_PER_HOUR`, default 60) and tokens (`ADVISOR_TOKEN_BUDGET_PER_HOUR`, 0 = unlimited; uses `usage.total_tokens`). Cache hits do not consume. |
| Circuit breaker | Opens after 5 consecutive provider failures, 60 s cooldown, then half-open with a single probe. Cancellation by the caller does not count as a failure. |
| Stats | `Advisor.Stats()` returns counters (requests, cache hit/miss, calls, failures, invalid responses, hallucinated figures, rate-limited, budget, circuit, retries, tokens, latency, breaker state). |

### Response types

`Review`: `verdict` (`ok|review|risky`), `confidence` (`low|medium|high`),
`rationale`, `risks[]`, `suggested_action`, `cited_figures[]` (`label`, `value`,
`unit`). `Summary`: `overview`, `verdict`, `confidence`, `highlights[]`,
`attention[]` (`workload`, `reason`), `cited_figures[]`, `omitted`.

Typed errors (`errors.Is`): `ErrDisabled`, `ErrInvalidInput`, `ErrRateLimited`,
`ErrBudgetExceeded`, `ErrCircuitOpen`, `ErrResponseTooLarge`, `ErrNoContent`,
`ErrInvalidResponse` (parent of `ErrMalformedJSON`, `ErrSchemaViolation`,
`ErrHallucinatedFigure`) and `*HTTPError` (`errors.As`).

## Guard-rails

- **Read-only.** The package does not import reconciler, webhook, actuation,
  server, k8s, recommender or client-go. `TestNoWriteDependencies` runs
  `go list -deps` and fails if that changes. Integration with the recommendation
  is via JSON (`DecisionFromJSON`), precisely to avoid creating that coupling.
- **Anti-hallucination.** Every number in `cited_figures` must exist in the
  payload that was sent (in the payload's units: millicores, MiB, %, USD), with a
  configurable relative tolerance (`FigureTolerance`, default 1%; sign-insensitive).
  On failure → `ErrHallucinatedFigure`, the response is discarded, and the counter
  is incremented. Out-of-set enums, empty rationale, invalid JSON, and `attention`
  entries pointing to a workload that was not sent are also discarded.
- **Prompt injection.** Names and labels are untrusted data: reduced to
  `[A-Za-z0-9._-/:=@+]` (everything else becomes `_`), truncated to 63 characters,
  at most 8 labels, encoded as JSON inside `<<<DATA ... DATA>>>`
  (the encoder escapes `<`/`>`, so the delimiter cannot be forged). The system
  prompt explicitly says never to follow instructions found in the data.
  Output text has control characters stripped and its size capped;
  **the UI must render it as text, never as executable HTML/markdown**.
- **Key.** Never in logs, errors or `fmt` of `Config` (`String`/`GoString`
  mask the key, header values and URL passwords). Provider error bodies are
  redacted before becoming an `HTTPError`.

## Privacy

Sent to the provider: namespace, workload, kind, container, replicas, requests and
limits (current/applied/recommended), usage quantile/peak/forecast, headroom
breach percentage, flags (OOM, HPA, PDB, confidence), mode/policy, estimated
weekly cost and, in `Review`, up to 8 sanitized labels.

**Never** sent: env vars, logs, secrets, annotations, images, pod specs,
IPs, node names.

`ADVISOR_ANONYMIZE=true` replaces namespace/workload/container with `ns-1`,
`workload-1`, `container-1` (deterministic numbering for the same input), removes
labels, and reverts the tokens in all response texts. Tokens that the model
invents are left as they are. Anonymization does not hide the metrics themselves;
with a provider outside your trust boundary, assess whether the numbers (cost,
replicas) are already sensitive.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `ADVISOR_ENABLED` | `false` | Turns the advisor on. |
| `ADVISOR_BASE_URL` | - | OpenAI-compatible base (without `/chat/completions`). |
| `ADVISOR_API_KEY` | - | Optional (Ollama does not need one). |
| `ADVISOR_MODEL` | - | Required when enabled. |
| `ADVISOR_TIMEOUT` | `30s` | Per HTTP attempt. |
| `ADVISOR_ANONYMIZE` | `false` | Tokens in place of names. |
| `ADVISOR_MAX_CALLS_PER_HOUR` | `60` | `0` = no limit. |
| `ADVISOR_CACHE_TTL` | `15m` | `0` disables the cache. |
| `ADVISOR_LANGUAGE` | `en` | `en` or `pt-BR` (free text only; enums stay in English). |
| `ADVISOR_MAX_TOKENS` | `800` | Cap on response tokens. |
| `ADVISOR_TOKEN_BUDGET_PER_HOUR` | `0` | Token budget per hour. |
| `ADVISOR_RESPONSE_FORMAT` | `json_schema` | `json_schema`, `json_object` or `none`. |
| `ADVISOR_EXTRA_HEADERS` | - | `K=V,K2=V2`. |

### LiteLLM (proxy)

```sh
ADVISOR_ENABLED=true
ADVISOR_BASE_URL=http://litellm.platform.svc:4000/v1
ADVISOR_API_KEY=sk-...        # LiteLLM virtual key, via Secret
ADVISOR_MODEL=tailor-advisor  # model_name defined in the proxy's config.yaml
```

### Ollama (local, no key)

```sh
ADVISOR_ENABLED=true
ADVISOR_BASE_URL=http://ollama.platform.svc:11434/v1
ADVISOR_MODEL=llama3.1:8b
ADVISOR_RESPONSE_FORMAT=json_object   # or none if the model ignores it
ADVISOR_TIMEOUT=120s                  # CPU is slow
```

Small models get the schema and the numbers wrong more often; the validator will
discard more responses. That is the correct behavior.

### OpenAI

```sh
ADVISOR_ENABLED=true
ADVISOR_BASE_URL=https://api.openai.com/v1
ADVISOR_API_KEY=sk-...
ADVISOR_MODEL=<model supporting chat completions + structured outputs>
```

Note: the client sends `max_tokens` and `temperature: 0`. Reasoning models
that require `max_completion_tokens` or reject `temperature` may fail;
in that case use a non-reasoning model or go through a proxy (LiteLLM) that
normalizes the parameters.

### Amazon Bedrock

Checked against the AWS documentation on 2026-10-21
([Chat Completions API](https://docs.aws.amazon.com/bedrock/latest/userguide/inference-chat-completions.html)):

- Recommended `bedrock-runtime` endpoint:
  `https://bedrock-runtime.{region}.amazonaws.com/openai/v1/chat/completions`,
  authentication via AWS SigV4 **or** Amazon Bedrock API key (Bearer).
- `bedrock-runtime` does **not** implement `GET /models`.
- `bedrock-mantle` compatibility endpoint:
  `https://bedrock-mantle.{region}.api.aws/v1/chat/completions`; for some
  models the prefix is `/openai/v1` instead of `/v1` (the doc says to check the
  model card).
- The AWS example uses the model `openai.gpt-oss-120b-1:0`; Chat Completions
  availability varies by model, so check the model card.

Direct (with a Bedrock API key, no SigV4, which this client does not sign):

```sh
ADVISOR_ENABLED=true
ADVISOR_BASE_URL=https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1
ADVISOR_API_KEY=<Bedrock API key>
ADVISOR_MODEL=openai.gpt-oss-120b-1:0
```

Via LiteLLM proxy (for IAM/SigV4, Anthropic/Nova models, etc.). `config.yaml`
format per the LiteLLM docs
([Bedrock](https://docs.litellm.ai/docs/providers/bedrock)); the Bedrock model ID
is whichever one your account/region has enabled:

```yaml
model_list:
  - model_name: tailor-advisor
    litellm_params:
      model: bedrock/<bedrock-model-id>
      aws_region_name: os.environ/AWS_REGION_NAME
      # credentials: AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or role (IRSA)
```

and Tailor points to the proxy as in the LiteLLM example above. I did not verify
the behavior of `response_format` on Bedrock in this environment; the client
downgrades on its own if the provider rejects it.

## Integration (for whoever wires it up)

```go
cfg, err := advisor.ConfigFromEnv()          // env error -> fail at boot
adv, err := advisor.New(cfg, advisor.WithLogger(slog.Default()))
// adv.Enabled() == false by default; Review/Summarize return ErrDisabled.

raw, _ := json.Marshal(rec)                  // recommender.Recommendation
in, _ := advisor.DecisionFromJSON(raw)
review, err := adv.Review(ctx, in)           // use a ctx with the request deadline
```

Map `ErrDisabled` to 404/501, `ErrRateLimited`/`ErrBudgetExceeded` to 429,
`ErrCircuitOpen` to 503 and other errors to 502. Expose `adv.Stats()` on
`/metrics`.

## Limitations

- **An LLM is not a source of truth.** Tailor's deterministic engine remains
  the decision-maker. The advisor can be wrong while sounding confident.
- Validating `cited_figures` catches invented numbers **in the list**, not in
  prose (`rationale`, `risks`), nor faulty reasoning over correct numbers.
- The 1% tolerance accepts rounding, but also matches similar numbers from
  different fields (e.g., 350 peak and 350 from another metric).
- Temperature 0 reduces, but does not eliminate, variation between calls.
- Anonymization does not cover inference from unique metrics.
- The cache is in-memory per process (not shared across replicas) and the rate
  limit/budget are also per process.
- No streaming, no tool calling, no built-in SigV4 signing.
- Quality depends on the model; small local models fail the schema more often.
