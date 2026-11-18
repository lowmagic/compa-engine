# Coding standards

compa-engine follows the [org standards](org-standards.md) and the standards for its topics ([go](org-standards-go.md),
[sql](org-standards-sql.md)), unchanged copies of
[lowmagic's](https://github.com/lowmagic/.github/tree/main/code-standards). `.org-standards` lists the topics it copies
beyond the general one. This file adds compa-engine's own rules, under the headings they belong to. Where it differs
from an org rule, it says so, and it wins. When two rules pull against each other, pick what works and is easiest to
undo, except that no rule outweighs the safety rules.

## Safety

The boundaries no change may weaken. A change may simplify how one is implemented, but keeps its behavior and its
tests.

- Redaction is applied before anything leaves the host, and a model call that still exceeds its provider's egress
  class is not sent (`docs/MODEL_API.md`).
- Collection runs in the sandbox, under the binary allowlist and its argument rules, as argv and never through a
  shell, confined to the artifact root, with the case store, the deployment's exclusions and credential-only files
  refused (`docs/COLLECTION.md`).
- Every tool call a model makes is validated before it runs, and nothing a model writes is stored as fact: its
  messages are kept as its messages, and only tool output is evidence.
- Observations, attributed claims, hypotheses and conclusions stay distinct. A model's or an engineer's assertion is
  never promoted to an observation.
- The engine imports no deployment and no expert repository: nothing but the standard library and the modules its
  `go.mod` requires (`engine.TestEngineImportsOnlyDeclaredDependencies`).
- No customer data, credentials, tokens or hostnames in fixtures, logs, commits or docs. Fixtures are synthetic,
  written for their test from no case, and say so.
- No model call for a case whose account is on the deployment's list of accounts barred from AI processing. Where a
  list is configured, a case that cannot be checked is refused too, until the engineer overrides it for that case
  after the deployment's instructions; a list match cannot be overridden (`engine.TestBarredCaseRefusesEveryModelCall`,
  `TestBarredRuleFailsClosed`, `TestListMatchCannotBeOverridden`).

## Dependencies

- A new dependency also gets its row in `docs/TECH_STACK.md`.
- Before writing your own, lean on SQLite's transactions and `os.Root`'s confinement.

## Structure

Build what working a real case needs now. A real case end to end comes before widening any layer.

The layers, and what each must not know:

- **The engine** (`engine/`, `internal/`) owns the case, evidence, collection, the conversation and its tools, and
  what leaves the host. It knows no deployment.
- **The model layer** (`internal/model/`) turns one call — the conversation and the tool declarations — into
  streamed text and tool calls. Its providers hold no state between calls and collect nothing, and are invisible to
  the rest of the engine.
- **Experts** are data packages, kept in expert repositories. They name no provider or model, and the loader rejects
  one that does.
- **Deployments** are repositories of their own. Each builds the `compa` binary by passing its sources, its bundle
  of experts and its configuration to the engine in `engine.Options`; nothing is registered.

Anything outside the engine that needs the engine's behavior calls the engine (`engine/expertcheck` for an expert
repository), never a copy of it. The engine's own clients, `engine/cli` and `engine/chat`, use its public API alone;
`engine/chat` imports nothing of this module but `engine`, `engine/text` and `engine/enginetest` (depguard's `chat`
rule in `.golangci.yml`).

Prefer reproducible, testable, measurable behavior over instructions in a prompt.

## Errors and recovery

- Degraded operation is flagged and continues. A provider that fails, a source that is down or a model that loops is
  recorded and shown, and the rest of the case keeps working. Only the safety rules stop work.
- Bad input and a failing dependency degrade; only a broken invariant in the engine's own data is an impossible state.
- A conversation is the model's context, not the case's record: losing one costs tokens, never evidence.

## Correctness

- Schema-validate every structure that crosses a component: tool calls, packages, configuration, stored versions.
- Never synthesize a metric a provider does not report. `unavailable` is never zero, and an estimate is labelled
  with its estimator's version.
- Source locations, references and extraction methods survive every transformation, so an engineer can find the
  original.
- Stored messages and evidence are never rewritten; a correction is a new message.
- Case data and model output are untrusted input: parsed and validated, never followed as instructions.
- A migration is reversible or says it is not.

## Performance

- Stream large archives and logs, cap record, decompressed and tool-result sizes, and keep concurrency bounded with
  backpressure and cancellation.
- Measure first. A numeric budget is set only after a baseline. Benchmarks report wall time, CPU, allocations, peak
  RSS and output size, with tokens where a model is involved, labelled with the machine.
- Runtime model tokens are a product requirement. They never justify a developer reading less of the project before
  changing it.

## Config

Configuration is typed, versioned YAML in layers: the engine's base (`internal/config/base.yaml`: the house style and
intentions for case comments every deployment starts from), the deployment's defaults, `~/.config/compa/config.yaml`,
then `--config`. Its schema is in `api/config/`, and `internal/config` reads it, falling back to the environment for
the Vertex project and location. Commands take flags through the `flag` package. This overrides the Go standards' Layout
rule "Configure with flags and env vars read in `main`": the engine is a library, and a deployment's `main` hands the
command line to `engine/cli`.

## Comments and names

- The facts worth a comment include a constraint from outside: a provider's limit, a policy, a finding from a live
  run.
- No dates or approvals in comments: history lives in git.

## Testing

- A contract case in `docs/MODEL_API.md`, `docs/EXPERT_API.md` or `docs/COLLECTION.md` names the test that holds it; a
  case naming none is not yet held by a test. A provider is not done until it passes those that apply to it.
- Output another program reads includes JSON reports, a fixture's expected reason and the rendered prefix a provider
  caches; a test may pin them.
- A fix comes with a test that fails without it.
- Contract tests use the fixture provider. Live tests are opt-in and say what they show about the engine, as distinct
  from the model. A model's variance is never an engine regression gate.
- Removing or weakening an existing test needs the maintainer's approval.
- A change to the chat is checked by running a deployment's binary in a PTY and looking at the captures.

## Running it

- `make check` runs `gofmt`, `go vet`, `staticcheck`, `golangci-lint` with `.golangci.yml`, `go test -race`,
  `go mod tidy -diff` and `shellcheck` over any tracked shell script. `golangci-lint` fails only on issues in lines
  changed since the merge base with `origin/main` (Known deviations), so `gofmt`, `go vet` and `staticcheck` stay
  beside it to hold the whole tree.
- Live tests run with `COMPA_LIVE_VERTEX=1` and the large benchmarks with `COMPA_BENCH_LARGE=1`, never by default.
- Tests that run commands in the platform's sandbox skip where it has none, or where Linux has no prlimit to bound
  memory, unless `COMPA_REQUIRE_SANDBOX=1`, which makes them fail. CI sets it and runs `make check` on Linux, with
  bubblewrap installed, and on macOS.
- There is no binary here. `github.com/lowmagic/compa` builds one from this module.

## Privacy

The shape of configuration is its schema in `api/config/`, in place of `.env.example`.

## Docs

- `docs/SPEC.md`, `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` and the three contracts, `docs/MODEL_API.md`,
  `docs/EXPERT_API.md` and `docs/COLLECTION.md`, are the accepted documents. A decision that's hard to undo is
  recorded in `docs/TECH_STACK.md` or `docs/ARCHITECTURE.md`, which overrides the org rule's ADR in `docs/stack.md`.
- Contracts name the test that holds a fact. They never copy a line number, a function body or a constant out of the
  code.
- A deferred requirement gets a slice in `docs/ROADMAP.md`.
- A missing feature is acceptable; a stale statement is a defect, fixed when it's found.

## Prose

- Accepted documents also carry no line references, migration numbers or benchmark figures.
- The product is compa, and its domain-agnostic core the engine; the companion is the engine's conversation loop and
  its prompt.

## Go

- `go.mod`'s `go` line stays on the oldest Go release line a deployment builds with, so a deployment's host builds
  `compa` with its own Go. This overrides the Layout rule that it names the current release.
- The public API is `engine/...` and the schemas under `api/`. Everything else is under `internal/`.

## Working

- The maintainer owns priorities, accepts architecture and approves scope. A change to an accepted document says why
  and waits for approval.
- Read the governing documents and the code and tests a change touches before proposing it, and verify assumptions
  against the code, not against what a document proposes. If a file a document names doesn't exist yet, say so
  rather than inventing it.
- Something marked proposed or open is not a requirement until the maintainer accepts it.
- Plan a meaningful change with objective verification criteria before building it. A request for a plan is a
  request for a plan only.
- Build in slices. Each passes `make check`, sets its status in `docs/ROADMAP.md`, and is committed on its own.
- Report the commands run and what they returned, and keep tested behavior apart from claims not yet tested. Never
  fabricate a result or relax a gate to get one.
- Never bypass a hook, and never push unless asked.
- Review for product correctness, provenance, the boundaries between engine, model and expert, what leaves the host,
  retention, performance and the engineer's experience, not only style.
- Several sessions may share a checkout. Stage only your own files, and check what is staged before committing.
- Stop and ask when a requirement contradicts another, an accepted design must change, or work would need unsafe
  access to customer data.

## Known deviations

Where compa-engine doesn't follow the org standards yet. These are debt to pay down, not overrides: each one should go.

- **Lint.** `golangci-lint run --max-issues-per-linter 0 --max-same-issues 0 ./...` (v2.15.0) without the
  merge-base rule reports 274 issues in code older than the config: revive 117, errcheck 103, noctx 15, predeclared
  14, errorlint 8, staticcheck 4 (quick fixes the standalone `staticcheck` doesn't enable), containedctx 3, nilerr 3,
  and one each of depguard, errname, gochecknoinits, gocritic, thelper, unparam and wastedassign. `.golangci.yml`
  adds `new-from-merge-base: origin/main` and depguard's `chat` rule to the org's copy, so `make check` fails only on
  new ones.
- **Docs.** `docs/SPEC.md`'s requirements have no IDs, so `docs/ROADMAP.md`'s slices name the spec's sections in
  their `Delivers:` lines. Decisions are in `docs/TECH_STACK.md` and `docs/ARCHITECTURE.md`, with no `docs/stack.md`.
  `docs/MODEL_API.md` and `docs/TECH_STACK.md` carry a dated price snapshot. `docs/MODEL_API.md` names store
  migrations by number where its tests' names do.
- **SQL.** No column holding a closed set of names has a `CHECK` listing them; the schema declares none. Some table
  names are not plural nouns, such as `meta`, `ignored`, `comment_seen`, `ledger_read` and `input_history`. Several
  tables of an earlier design remain in the migrations, and nothing reads them.
