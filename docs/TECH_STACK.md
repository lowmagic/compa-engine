# compa — Technical Stack

Technology marked open remains open. Each pinned version was the newest release at least two weeks old when it was adopted.

| Area | Choice | Why |
|---|---|---|
| Main application | Go 1.26, module `github.com/lowmagic/compa-engine` | Streaming large artifacts, concurrency, memory predictability; a deployment's binary is one static binary |
| Chat UI | Bubble Tea inline mode + Lip Gloss + Bubbles, the v2 line under `charm.land`: `bubbletea/v2` v2.0.10, `lipgloss/v2` v2.0.6, `bubbles/v2` v2.2.1; Markdown rendered by a small renderer over Lip Gloss, no new dependency | Prints into the terminal's own scrollback, so its search and copy work; reusable input controls |
| Shell | `!` from the chat opening the engineer's shell, and `compa in` ingestion, over an owner-only Unix socket to the chat holding the case; proposed later: embedded PTY (`creack/pty` or equivalent), optional tmux adapter | Works inside and outside tmux |
| Database | SQLite with WAL, through `modernc.org/sqlite` v1.60.1 | Transactional local case state, conversations, evidence index; a pure-Go driver with FTS5 built in keeps a deployment's binary one static binary with no cgo |
| Compression | zstd through `github.com/klauspost/compress/zstd` v1.20.1; gzip support for incoming artifacts | Storage and I/O efficiency |
| Search | SQLite FTS5 and indexed structured queries first | Simple, deterministic retrieval before more complex search |
| Acquisition | `CaseSource` and `ArtifactSource` implementations passed by the deployment in `engine.Options`; the engine ships a stub case source and a local-directory artifact source | Keeps the engine independent of any one support system |
| Model layer | `internal/model`'s narrow `Provider` interface: free-text answers with tool calls, streamed, with cache breaks. Providers: `vertexgemini` (`google.golang.org/genai` v1.72.0, streaming through `GenerateContentStream`), `vertexanthropic` (`github.com/anthropics/anthropic-sdk-go` v1.78.0, streaming through `NewStreaming`), `openai` (Chat Completions over the standard library's `net/http`, streamed as server-sent events, no SDK), and `fixture`. Later: Anthropic Messages API | Both model families are on Vertex under Application Default Credentials, through Google's and Anthropic's own SDKs; the OpenAI-compatible provider reaches OpenRouter's models and self-hosted inference endpoints, its few request and stream fields not worth a dependency; see `MODEL_API.md` |
| Tests without a live model | The `fixture` provider | Deterministic contract tests offline |
| Contracts | Versioned JSON Schema 2020-12 for configuration, expert packages and the routing library, validated with `github.com/santhosh-tekuri/jsonschema/v6`. Its `propertyNames` failures report a wrong instance location, so the schemas use `patternProperties` or explicit `properties` instead | Validated where they are read |
| Collection | Go primitives (`github.com/itchyny/gojq` for `jq` and `yq`, `go.yaml.in/yaml/v3` for YAML, …), confined to the artifact root by the standard library's `os.Root`; argv runner with a binary allowlist; the sandbox an interface, macOS `sandbox-exec` and Linux bubblewrap (`/usr/bin/bwrap`, with `/usr/bin/prlimit` bounding memory) the defaults, a deployment able to pass its own in `engine.Options.Sandbox`, every one held to the contract tests in `engine/sandboxtest` | Behavior independent of installed tools; bubblewrap runs unprivileged in user namespaces, where the host allows them; see `COLLECTION.md` |
| MCP | `github.com/modelcontextprotocol/go-sdk`, adopted when the engine first runs an expert MCP server: engine-side client for expert MCP servers | The official SDK, stable at v1 |
| Config | Versioned typed configuration | Validation |
| Expert packages | Versioned local data packages + schema validator and loader | Domain knowledge independent of Go runtime; see `EXPERT_API.md` |
| Tests | Go `testing`, fixtures, race detector, benchmarks; `make check` runs gofmt, `go vet`, `staticcheck`, `golangci-lint` v2.15.0 with the org's `.golangci.yml`, `go test -race`, `go mod tidy -diff` and `shellcheck` over any tracked shell script (`docs/standards.md` §Running it) | Deterministic coverage and performance evidence; the lint config follows the org's standards |

## Code layout

- `engine/` — the public `engine` package, with `engine/localdir`, `engine/stubcases`, `engine/sourcetest`, `engine/sandboxtest`, `engine/expertcheck` and `engine/compose`, which merges expert, catalog and routing-library packages into the bundle a deployment passes; `engine/cli`, the command line, which a deployment's `main` hands its arguments and `engine.Options`; `engine/chat`, the inline chat `engine/cli` runs; both clients of the public package that import nothing under `internal/`; `engine/text`, small text helpers for both; and `engine/enginetest`, fixture engines for tests, on the fixture provider.
- `internal/companion/` — the conversation loop, the tool table and the versioned companion prompt.
- `internal/model/` — the `Provider` interface; `vertexgemini`, `vertexanthropic`, `openai` and `fixture` beneath it; `internal/model/pricing`, the price table.
- `internal/` — the engine's other packages: store, evidence, invocations (the invocation records, their usage and cost), collect (primitives, the argv runner and the sandbox), expert, sources, redact, config, schema, semver, escape, routing (deterministic hints), ledger (the case ledger's update, its validation, its tool's declaration and its prompt), check (the answer checks' statements, findings, their validation and their tool's declaration) and benchcase (a synthetic case for benchmarks). The kernel's — store, evidence, invocations, collect, expert and sources — depend on no model provider, directly or through another package (`engine.TestKernelDoesNotDependOnProviders`).
- `api/` — the public versioned schemas: `expert/`, `routing/`, `config/`, `ledger/` and `check/`; their fixtures are under `internal/schema/testdata/`.
- `testdata/` — synthetic fixtures, and benchmark corpora once they exist; production expert packages live in expert repositories, such as `compa-experts`.

Create directories only as the work requires them.

## Model profile

Four profile classes name a model and an effort: `companion`, every call of the main conversation and of each consult; `ledger`, the case ledger's calls; `editor`, the editor's checks of each answer; and `reviewer`, the reviews the engineer asks for (`docs/MODEL_API.md`). The engine ships no default model: a deployment's configuration names the models and each class's profile, and a class with no profile is not run. `/model` switches model and effort for the running conversation, never the ledger's, the editor's or the reviewer's. This is configuration, not architecture, and none of it reaches an expert package.

A deployment's configuration names its models, each with the efforts it accepts: `gemini-3.8-flash` takes `low`, `medium` and `high` and refuses `minimal`; `gemini-3.5-flash-lite` takes all four and thinks only when given one; `claude-sonnet-4-6` takes none.

Both model families are reachable through Vertex, through the model layer's two Vertex providers. They are also available through their own apps (Gemini CLI, Claude Code), which compa does not use.

`gemini-3.5-flash-lite`, at $0.30 / $2.50 per million input / output tokens, is the cheap model. Gemini 3.8 Flash's introductory price ($0.75 / $3.75) doubles on 2027-01-01. Claude models are called with tool use alone, never the structured-output feature.

The conversation is sent most-stable first, with cache breaks after its static prefix, its case block and each call's last message (`docs/MODEL_API.md`), so cached input should dominate. Cached-input discounts are still not assumed into any estimate until measured.

Price and capability figures here are a snapshot and are not maintained in this document. Per-token list prices used for cost estimates live in the engine's price table (`internal/model/pricing`), with their source and retrieval date. A deployment revisits its choice of models whenever the model landscape moves.

## Constraints

- The engine must work without Python. An expert MCP server may use another runtime where the deployment provides it.
- Expert packages are declarative, apart from an optional MCP server the engine runs sandboxed; no install hooks. Tests compose engine primitives and allowlisted binaries.
- Support bundled pinned expert releases, local override paths, and offline operation; remote exact-release installation is optional.
- Support bounded worker pools and I/O backpressure; do not materialize huge must-gathers in memory.
- Do not depend on a particular hosted LLM feature for baseline product correctness.
- The engine must build, test and operate with no provider reachable: everything that calls no model keeps working, and tests run on the fixture provider. No provider may become a dependency of the kernel.
- Model-layer constraints — the engine assembles everything the model sees, and no provider default alters it — are requirements in `docs/SPEC.md` §5, with mechanisms in `docs/MODEL_API.md`.
- Prefer one durable local store plus content blobs over premature distributed infrastructure.

## Decisions to confirm

Supported RHEL/Fedora/macOS builds; model invocation interfaces and licensing; filesystem and encryption requirements; approach to artifact confidentiality; packaging/distribution route; sanctioned network endpoints and per-provider egress classification; the companion's default model, and whether a review on a different model family earns its cost.
