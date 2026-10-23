// Package advisor is an OPTIONAL, READ-ONLY LLM reviewer for k8s-tailor
// decisions. It talks to any OpenAI-compatible endpoint
// (POST {base}/chat/completions: LiteLLM, Ollama, vLLM, OpenAI, OpenRouter,
// Bedrock, ...) using only the standard library.
//
// Contract (enforced by design and by tests):
//
//   - The advisor NEVER decides or acts. It returns an opinion (Review /
//     Summary) that callers may display. Nothing in this package mutates a
//     cluster: it does not import the reconciler, webhook, actuation, server
//     or k8s packages (nor client-go). TestNoWriteDependencies verifies this
//     with `go list -deps`.
//   - It is disabled by default (Config.Enabled == false).
//   - Cluster-derived strings (namespace, workload, labels) are untrusted
//     DATA: sanitized to a strict charset, truncated, JSON-encoded inside
//     explicit delimiters, and optionally replaced by stable tokens.
//   - Only aggregated numeric metrics are sent. Never env vars, logs,
//     secrets, annotations or pod specs.
//   - Model output is validated: strict enums, bounded text, and every
//     number in cited_figures must exist in the input (see Config.FigureTolerance).
//     Invalid output is discarded and returned as a typed error.
package advisor
