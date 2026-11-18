# compa — Expert API Contract

Version v1alpha3 of the Expert manifest, with shared tool catalogs (§Shared tool catalog); catalogs and tests files stay at v1alpha2. The schemas are in `api/expert/`, one directory per version. Engine API `0.2.1`. This document governs the **runtime extension interface**, not development instructions. The engine implements the API; expert repositories supply packages. Companion to `docs/MODEL_API.md`, which governs the conversation, its tools and the model layer, and to `docs/COLLECTION.md`, which governs how tools run.

## Purpose and boundaries

Experts are independently versioned, installable **data packages** describing domain-specific troubleshooting knowledge and investigative tests. A model loads an expert's guidance and tools into its conversation, or consults the expert in a sub-conversation (§Lifecycle). An expert is not an autonomous agent or a trusted executable. compa owns acquisition, storage, conversations, authorization, execution, model calls and the chat. Experts are declarative configuration and reference material consumed by those services.

An expert is **independent of any model**. Manifests must not name a model provider, a model ID, or provider-specific prompt syntax, and the loader rejects those that do. Analyst guidance is transport-neutral Markdown; how it reaches a model is the model layer's concern (`docs/MODEL_API.md`). The same expert must run unchanged with every provider.

Two kinds of repository:

- `compa-engine`, this repository: the Go engine, the public Expert API schemas and loader, the collection primitives and runner, validation, `engine/expertcheck`, and `engine/compose`, which merges the packages a deployment ships into the file system `engine.Options.Bundle` takes: `experts/<package ID>/…` and `routing/library.yaml`.
- Expert repositories: installable domain packages (networking, storage, baremetal, etc.) with their tests, schema validation and fixtures. Each expert and each catalog is a Go package exporting an `fs.FS`. The public one is `compa-experts` (`github.com/lowmagic/compa-experts`).

## Package structure

```text
experts/networking/
  expert.yaml             # identity, version, summary, API range, capability manifest
  tests.yaml              # investigative tests composed from engine primitives and declared binaries
  analyst.md              # domain guidance loaded into a conversation
  references/             # optional reference documents
  fixtures/               # sample artifacts and expected test results
  mcp/                    # optional MCP server for logic tests cannot express
```

The manifest may reference files only inside its package; paths must be normalized and never escape the package root, and a package holds no symlink. There is no install hook or script. The only executable content is the optional MCP server under `mcp/`, which the engine is to run sandboxed; running one is not built yet (`docs/COLLECTION.md`).

## Manifest example (illustrative, not final schema)

```yaml
apiVersion: compa.lowmag.cc/expert/v1alpha3
kind: Expert
metadata:
  id: openshift.networking
  version: 0.1.0
  title: OpenShift Networking
  summary: OVN-Kubernetes and SDN pod networking, services, routes and ingress on OpenShift clusters
spec:
  engineApi: '>=0.2.0 <0.3.0'
  analyst: analyst.md
  references: [references/ovn.md]
  routing:
    keys: [openshift-networking] # illustrative; routing keys as the deployment's routing library names them
    signals:
      - fact: cluster.network_type
        equals: OVNKubernetes
  catalogs:
    - id: openshift.gather
      version: '>=0.1.0 <0.2.0'
      tools: [operator_conditions, pods]
  tests: [networking.ovn_pods, networking.service_endpoints]
  binaries: [omc]               # must be on the deployment's allowlist
```

`metadata.summary` is required: one line of at most 160 characters saying what the expert covers. It is the expert's line in the index (§Lifecycle), so it carries no provenance or version history.

Route signals are *eligibility hints*, not proofs of root cause; the engineer can load a different expert. A test may use only engine primitives, binaries the package declares and the deployment allowlists, and the package's own MCP tools. A test whose requirements are not available reports unavailable; it never runs partially.

## Lifecycle

### Routing metadata

An expert may supply `scope.yaml` without changing its manifest. The experts
repository validates it against `api/expert/v1alpha2/scope.schema.json`:
`expert` must match the manifest ID; `scope` gives its statement and optional
routing key/source; `retains` lists text with optional sources; `boundaries` names a
routing key or an external specialty and the mechanism required for handoff. It is
optional, and the authoring source of the generated boundaries block in the
expert's `analyst.md`. The engine does not read it: like any file of the
package, it counts toward the package's file, size and confinement limits and
is covered by its digest.

The experts repository owns these domain declarations, its tools and its routing
library. The engine matches source-reported routing keys, components and tags against
the manifests and library, preserving scores, match reasons, mapping
provenance, input evidence and registry revision. These deterministic hints
choose the expert preloaded into a case's conversation; the model chooses any
other expert itself, from the index.
Unknown named external specialties are not guessed into installed expert IDs.

The term dictionary (`api/routing/v1alpha1/terms.schema.json`, kind `TermDictionary`) stays in the experts repository; the engine does not read it.

The public `Case` and the stub source support optional `Components` and `Tags`; acquisition preserves them as JSON arrays in immutable metadata evidence.

### Operations

1. **Discover:** scan configured local expert roots and the bundled install set; do not execute discovered files.
2. **Validate:** enforce schema, unique IDs, SemVer version syntax, API compatibility, a summary within its bound, valid references, valid tests (known primitives, declared binaries), absence of provider/model references, content integrity, and resource limits. Reject malformed or incompatible packages with actionable errors.
3. **Resolve and pin:** select a concrete version and immutable content digest when an expert is loaded into a conversation. The pin — ID, version, digest and catalog versions — holds for that conversation. Resolve multiple versions deterministically; do not silently upgrade mid-conversation.
4. **Route:** evaluate declared routing keys and signals against normalized evidence, deterministically. When the case's routing key has a primary expert in the routing library and the expert is installed, the engine loads it before the opening turn's first model call: its own `load_expert` call and result follow the case block, as if the model had asked. Routing may nominate several experts and never stops the model or the engineer loading another. A signal's fact is observed, known absent (a producer read its evidence completely and reported it missing), or unknown. An unknown fact neither matches nor contradicts any signal, so `present: false` matches only a fact known to be absent, never missing or truncated collection.
5. **Load:** the system prompt carries the index, one line per installed expert: ID, summary and the routing keys it is primary for. `load_expert(id)` appends the expert's `analyst.md` and its tools, each with its parameters and summary, with a note on a tool whose capture kind the case lacks, to the conversation as the tool's result, and pins it; a tool an expert the conversation already pinned offers with the same entry is named by ID only, once listed in full in that conversation (`engine.TestCompanionLoadListsToolsOnce`). Several experts may be loaded, and a loaded expert stays for the conversation, a second load saying it is already there. An expert's reference files are offered once an installed expert ships one.
6. **Consult:** `consult(expert, question)` runs a sub-conversation with the same system prompt, tools and case block and the case updates given since, then the expert's guidance and the question, under the same tool budget; its answer returns as the tool's result and its transcript is kept (`docs/MODEL_API.md`).
7. **Collect:** the engine runs tests, catalog tools and commands the model calls in its runner with typed parameters (`docs/COLLECTION.md`), and stores each result as evidence.
8. **Record:** store each loaded expert's pin with its conversation, why it was preloaded, each tool call with who asked, the model used per call, and evidence references.

## Interfaces and contracts

### Discovery

Read-only operations: list the installed packages, describe one by ID and version, and resolve one within constraints. Each returns identity, summary, compatibility, digest, capabilities and load errors. **No network access is necessary** for local discovery.

### Routing

Experts declare the **routing keys** they serve in a versioned manifest (`spec.routing.keys`), with optional secondary/overlapping relationships; an expert may serve multiple keys and a key may map to multiple experts. A case's routing keys are whatever the case source reports; what a key is belongs to the deployment. Key names, aliases, and mappings are validated against the versioned routing library (`api/routing/v1alpha1/library.schema.json`, kind `RoutingLibrary`) maintained with the experts, which changes as the product catalog changes; packages do not invent key IDs. The manifest and query contracts support key-aware routing whether or not the registry is complete. The loader reads only these forms: an unknown field or kind is refused (`expert.TestLoadRejects`, `TestLoadRoutingKeys`, `routing.TestLoadLibraryForm`). With no routing library configured nothing is preloaded and the model loads experts from the index (`engine.TestNoRoutingLibraryPreloadsNothing`).

Input: bounded normalized case facts and available evidence metadata, including known routing keys, component identifiers, and optional document tags. Output: ranked *candidate* expert IDs plus matched signals, relationship type, provenance, and exclusions. Explicit key ownership or identified component matches should outweigh weaker symptom/text matches. Document tags suggest routing but do not establish ownership. Priority is advisory, and an engineer may override it. No expert may claim exclusive ownership of a case. Record why each expert was suggested: the matched signals and the registry revision.

### Evidence

Experts do not read the database directly. Their guidance says what evidence matters and how to get it; the engine decides which source queries, evidence reads and collections are permitted and returns bounded results.

### Tests

Investigative tests are knowledge, so they live with the expert. Each test declares an ID and version, a typed parameter schema, its steps as a pipeline of engine primitives and argv commands (`docs/COLLECTION.md`), the binaries it uses, the shape of its output, a pointer to the analyst guidance that interprets it, and fixtures that exercise it. Tests are **read-only**. They may set preconditions or eligibility rules, and can return unavailable or insufficient-evidence without manufacturing a result.

```yaml
apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Tests
tests:
  - id: networking.ovn_pods
    version: 0.1.0
    params: {namespace: {type: string, default: openshift-ovn-kubernetes}}
    run:
      - [omc, get, pods, -n, '{{namespace}}', -o, json]
      - jq: '.items[] | {name: .metadata.name, phase: .status.phase, restarts: [.status.containerStatuses[].restartCount]}'
    interpret: analyst.md#ovn-pods
    fixtures: fixtures/ovn-pods/
```

The schemas are `api/expert/v1alpha3/manifest.schema.json` and `api/expert/v1alpha2/tests.schema.json`.

Parameters are substituted as whole argv elements, never spliced into a string. The engineer's shell, `!<command>` in the chat, is separate.

### Guidance in a conversation

`analyst.md` is loaded once into a running conversation, beside other experts' guidance, and stays there; it does not repeat itself for a model that forgot. It:

- names each tool by its full ID where it explains it;
- refers to the case's attachment list for attachment names;
- says what evidence is missing and how to get it;
- runs read-only checks through the tools rather than suggesting them, and suggests to the engineer only what the tools cannot run;
- suggests to the customer exact commands, marked when destructive;
- hands off, when evidence establishes another specialty's mechanism, by saying so with that evidence and naming, loading or consulting its expert from the index, or saying none is installed.

Whatever the model writes from it is stored as the model's message, never as fact (`docs/MODEL_API.md`).

### Version and provenance

Version the API independently of expert packages. An expert declares supported API range. Within a conversation, keep using the pinned expert version and its test and schema versions, so its behavior does not change mid-case. The model used is recorded per call (`docs/MODEL_API.md`), not frozen for the conversation. Analytical results are not cached.

## Shared tool catalog

Experts should not each define their own tools for common operations. The engine provides only generic primitives — reading, listing and searching files, matching patterns — and the command allowlist, which every expert already shares. Every tool beyond those is technology-specific, and so lives in an expert repository: `omc` queries, must-gather and sosreport paths, operator conditions, journal windows.

### Catalog packages

A catalog is a package of tools — parameterized, typed tests in the same `tests.yaml` format experts use — and nothing else. It is versioned, digested, size-limited and validated like an expert, so many experts can use one tool definition.

```text
catalogs/openshift-gather/
  catalog.yaml            # identity, version, API range, binaries, tool list
  tests.yaml              # the tools
  fixtures/               # one directory per tool
```

```yaml
apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Catalog
metadata: {id: openshift.gather, version: 0.1.0, title: OpenShift must-gather tools}
spec:
  engineApi: '>=0.2.0 <0.3.0'
  binaries: [omc]
  tests: [openshift.gather.operator_conditions, openshift.gather.pods]
```

A catalog differs from an expert:

- It has no analyst text, references, routing or MCP server. It is never routed, listed in the index or loaded.
- A catalog says **what** a tool returns, never what it means. Every tool has a `description` of what it collects and the shape of its output, and a `fixtures` directory; a tool may not carry `interpret`.
- A tool's ID is the catalog ID and one more segment (`openshift.gather.operator_conditions`), so two catalogs cannot define the same tool.
- A test, a catalog's tool or an expert's own, may carry a `summary` (v1alpha2): one line of at most 120 characters saying what it reads and answers. `load_expert` lists each of the expert's tools on one line: its ID with its parameters, required ones first and marked `*`, a type named where it is not a string, then its summary, or its description's first sentence where it has none (`engine.TestCompanionCatalogTools`, `TestToolSignature`); a model asks for a tool's full description and parameter schema with `describe_tool`. It is optional, and `compa expert check` does not warn on a test without one. A test ID must not contain `__`.
- A catalog depends on no other catalog.
- A catalog may declare `captures` (v1alpha2): the kinds of capture its tools read, such as `must-gather` or `sosreport`, each with up to eight names of files or directories whose presence within an attachment marks one (`{kind: must-gather, marks: [cluster-scoped-resources]}`). A catalog whose tools read any file, such as `compa-experts`' `generic.files`, declares none. The engine looks for a mark up to five levels under the case's artifact root, the case store and hidden entries left out, at most 50,000 entries; a tool of a catalog none of whose kinds is there is listed by `load_expert` with a note naming the missing kind, and `run_tool` still runs it, its result noting the missing kind; once such an attachment is there, neither notes anything. The marks are the experts' knowledge; the engine only looks for them.
- A capture kind may name an `identity` tool (engine API `0.2.1`): the short name of a tool of the same catalog, which takes the attachment's directory, relative to the artifact root, as the string parameter `dir`, needs no other parameter, and reads only beneath it (`{kind: must-gather, marks: [cluster-scoped-resources], identity: capture_identity}`); the loader rejects an `identity` naming no tool of the catalog or one that does not take `dir` so (`expert.TestLoadV1alpha2Rejects`). When the chat opens a case (`Session.ReadInventory`), the engine runs it once over each attachment whose contents hold the kind's marks, through the runner like any tool, its record stored as evidence, and builds the artifact inventory from what it returns (`docs/ARCHITECTURE.md`, `docs/COLLECTION.md` §Principle). Its output, format `lines`, is one JSON object a capture found, none when there is none, each validated against `compa.lowmag.cc/expert/identity/v1alpha2` (`api/expert/v1alpha2/identity.schema.json`): `capture`, the capture's root or report directory relative to the artifact root, beneath `dir`; `target_kind`, `cluster` or `host`; `target`, the cluster ID or host name, null where it cannot be read; `time`, the capture time in RFC 3339 with its offset where the capture states one; `local_time`, `YYYY-MM-DDTHH:MM:SS[.fraction]` without a zone where it states none; `raw_time`, the time as read; and `problems`, short reasons for what could not be read. Every field is present, null where the capture does not say. A line that does not pass is an uncertain capture saying why, never a guess. The identity is the catalog's knowledge, how each kind states its target and time; the rules of currency are the engine's (`docs/ARCHITECTURE.md`).

Catalogs live in the same roots as experts and are told apart by manifest file. IDs are unique across experts and catalogs; a clash is reported and neither package is used. The provider- and model-name check applies to catalog manifests too.

### Dependencies

An expert declares, per catalog, a version range and the tools it uses. There is no whole-catalog form: a catalog release that adds a tool changes no expert's guidance until the expert lists it, and what `load_expert` appends stays small.

```yaml
apiVersion: compa.lowmag.cc/expert/v1alpha3
kind: Expert
spec:
  engineApi: '>=0.2.0 <0.3.0'
  catalogs:
    - id: openshift.gather
      version: '>=0.1.0 <0.2.0'
      tools: [operator_conditions, pods]   # short names; the engine expands them to full IDs
  tests: [storage.csi_attach]              # the expert's own, domain-specific tests
```

- Ranges use the `engineApi` syntax and, before 1.0, pin the minor version. Changing a tool's parameters or output shape is a breaking catalog change. Tools keep their own versions, recorded in the pin; experts do not constrain them.
- Each expert resolves to the highest installed catalog version within its own range. Two experts in one conversation may pin different versions of one catalog, so upgrading one never changes another.
- An expert's own test ID may not begin with the ID of a catalog it declares.
- A catalog that is not there — not installed in any root, or the ID an expert's — refuses the expert when the roots are discovered: it is reported as a package that could not be loaded, naming the catalog, and is not installed, while the packages beside it are; `compa expert check` and `expertcheck.Load` fail on it. So a set composed without a catalog is found when it loads, not when a case needs the tools (`expert.TestDiscoverRefusesExpertWithoutItsCatalog`, `engine_test.TestPartialBundle`, `expertcheck_test.TestPartialRoot`). A conversation that pinned the expert before keeps it, from its snapshot.
- Any other unmet dependency — no installed version in range, or a listed tool absent from the resolved version — does not stop the expert. Its own tests and guidance work; the missing tools are not offered. It is made visible: a warning at discovery naming the expert, catalog, range and installed versions; the dependency recorded as unresolved in the pin; and the warning shown with the expert when it is listed or loaded.

### Names in tool calls

`run_tool` names a tool by its full ID, with the same parameter validation as an expert's own test. It runs any tool an installed package defines, not only one a loaded expert lists; a call naming no installed tool, or with parameters its schema rejects, is refused with the reason as its result. A result is labelled `<tool ID>@<version>`.

### Interpretation

The catalog describes a tool's output; the expert explains what it means, because the same output means different things to different experts. The expert's `analyst.md` names each tool it uses by full ID where it explains it. `load_expert` appends the analyst text and the tool's summary, and a result is labelled `<tool ID>@<version>` in its provenance, so the model links the result to its guidance by ID. References are not used for this, since a reference would enter the conversation only when the model asked for it.

An expert whose `analyst.md` does not mention a tool it lists still loads, and the tool is still offered; the loader warns, saying which tool to explain. An expert repository's CI can fail on any loader warning (`compa expert check --fail-on-warning`), so that bundled experts explain every tool they use, while local development is not blocked.

### Pinning and digests

An expert's digest still covers only its own files. Its pin gains the catalogs it uses — each with ID, version, digest and the versions of the tools used — or the dependency marked unresolved; an expert using no catalog has no catalog field. Catalogs are snapshotted by digest like experts, and a pinned conversation uses each package's installed copy while its digest matches and the snapshot otherwise, with a warning. An explicit re-pin resolves the expert and its catalog ranges again; that is how a fix to a shared tool reaches a conversation, and it happens only in a new conversation, including the one `/compact` starts.

A catalog tool's result records the catalog and its version, not the expert that uses it, so the same tool run with the same parameters for two experts is one piece of evidence; the tool call in the conversation records who asked.

### Versions

Expert manifests are at `v1alpha3`; tests files and catalogs are at `v1alpha2`. The loader selects each schema by `apiVersion` and reports one it does not read by name, so an older engine given a newer package says so rather than reporting a confusing schema error. Compatible additions to the engine API before 1.0 take a patch version, so every `<0.3.0` range still includes the engine.

Test IDs beginning `evidence.` are reserved for the engine, and the loader rejects a package that defines one. The evidence tools themselves are offered in every conversation (`docs/COLLECTION.md` §Evidence tools), so analyst text may refer to them.

### Checking packages

`compa expert check [--fail-on-warning] [--json] [--update] <root> [root …]` runs the loader's checks over the given roots before a package ships, and every usable package's fixture cases (§Fixture cases), reporting every package, its catalog resolution, its cases and its warnings, in text or as JSON in `compa.lowmag.cc/expert/check/v1alpha2`; it exits 1 on any error, a failing case among them, or on any warning with `--fail-on-warning`, which is how an expert repository's CI fails on loader warnings. Both the check and fixture runs are public in `engine/expertcheck`, which builds without the deployment, so an expert repository needs neither the deployment's build nor a copy of the runner: `Command` is `compa expert check` whole, and `Load` and `Run` run one test or catalog tool against a fixture artifact root through the engine's loader and runner at the default limits, with no sandbox or allowlisted binary, so a step that runs a command reports unavailable.

### Fixture cases

Every test with `fixtures`, a catalog's tool or an expert's own, keeps `cases.yaml` in that directory: the runs `compa expert check` makes against a fixture root, and what each returns (`api/expert/v1alpha2/cases.schema.json`, kind `Cases`).

```yaml
apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Cases
tool: generic.files.head          # the test whose fixtures directory holds the file
root: ../root                     # the artifact root, relative to the file, inside the package
modified: 2026-03-01T12:00:00Z    # every entry's modification time in the copy the cases run over
requires: [normal, invalid_param] # labels the cases must cover, optional
credentials: [auth/kubeconfig]    # names the runs refuse as holding only credentials, optional
cases:
  - name: the first two lines     # unique in the file
    covers: [normal]              # optional
    note: …                       # optional, for people
    params: {path: logs/app.log, lines: 2}
    status: ok                    # ok, unavailable or failed
    truncated: false              # optional
    reason: ''                    # the engine's reason, whole; empty when ok
    output: |                     # the whole output; empty when not ok
      …
```

- The root is copied to a temporary directory, every entry's time set to `modified`, and each case runs through the engine's runner at the default limits, with no sandbox or allowlisted binary.
- Status, reason, truncation and output are compared exactly. A case fails on any difference; the file fails on a name repeated, a label in `requires` no case covers, a `tool` other than the test naming the directory, or a root outside the package. A test with `fixtures` and no `cases.yaml` is an error.
- `compa expert check` runs every usable package's cases after loading; a failing case is an error of its package. `--update` writes each case's status, reason, truncation and output from its run, keeping the rest of the file and its comments; the diff is read before it is committed.
- `expertcheck.RunCases` runs one cases file whose root lies outside any package, such as a repository's boundary cases over a root holding symlinks, by the same rules.

### Compiling a routing library and checking versions

`compa expert routing --keys <file> --library <file> [--write [--revision <rev>]] <root> [root …]` compiles a routing library from the manifests under the roots, which load as `compa expert check` loads them. The keys file is a `RoutingLibrary` (`api/routing/v1alpha1/library.schema.json`) naming each key with its name, aliases, components, tags and provenance, and no experts; each expert's `spec.routing.keys` adds the expert under its key with its relationship, `primary` where none is given, and its provenance, the expert's ID and its manifest's path. A key the keys file does not name, an alias in place of its canonical ID, an expert in the keys file, or a library the engine's loader refuses fails it. Without `--write` it fails unless the library file holds the compiled bytes; with it, it writes them, and refuses content changed under the file's revision unless `--revision` gives a new one. `expertcheck.LoadKeys` reads a keys or library document by the engine's own rules, and its `Canonical` answers which key a name or alias names, case aside, so an experts repository maps its own data onto keys without a copy of the rule.

`compa expert versions [--base <rev>] <root> [root …]`, in a git checkout, fails when a package under the roots changed since the base revision with its ID and version unchanged. A package's content is every file of its directory but the directory's own Go files, which embed it; a package new since the base, or renamed, is not compared. The base is the merge base with `origin/main`, or, where that is `HEAD`, its parent.

## Security and isolation

- Experts are **untrusted input**. Never interpolate their values into a shell command, filesystem path, SQL or command argv without typed validation.
- No package-supplied executable hooks, and no direct access to secrets, network credentials, raw customer stores, or store mutation.
- Enforce authorization, case isolation, redaction and size/token budgets in Go, not through analyst prose.
- Model-requested collection runs only through the engine's runner, under its allowlist, argument rules and sandbox.
- Expert content reaches a model only as part of a conversation's call, subject to the egress class's redaction policy and the barred-accounts rule (`docs/MODEL_API.md`). An expert cannot widen its own permissions, budgets, or egress by what it declares or what its analyst text says.
- Validate compressed/package inputs against decompression bombs, traversal, oversized text and symlinks.
- Bundles are immutable once pinned; a conversation keeps its pinned version; a new conversation, including the one `/compact` starts, resolves again.

## Distribution

Support: (1) **bundled, tested expert set** installed with the engine (default); (2) local development directory overrides with explicit precedence and warnings; (3) optional fetch of an exact release/digest into a local cache. Never download moving `main` automatically, and never require a network connection for normal work. Release integrity and signing policy are open decisions; at minimum verify artifact digests. The engine should be operable with two locally installed packages with no source modifications.

## Contract tests / first acceptance gate

The tags are group labels, naming each case's area: **[C3]** the conversation loop, **[C4]** the expert contract, **[C9]** the case's state and inventory, **[TC]** shared tool catalogs.

- Install two independently versioned experts from local directories; discover and route both without modifying Go code.
- Reject duplicate IDs, incompatible API ranges, missing files, path escapes, unsafe symlinks, tests using unknown primitives or undeclared binaries, malformed manifests, bad digests, and packages exceeding resource limits.
- Run two expert tests against fixture artifacts; validate their parameters and the evidence they store.
- **[C4]** `compa expert check` runs each package's fixture cases and fails on a result that differs in status, reason, truncation or output, a repeated name, a required label no case covers, a file of another tool, a root outside the package and a test with fixtures but no cases; `--update` writes the results, keeping the file's comments and styles; a cases file outside a package runs by the same rules over a root holding symlinks, refusing the credential files it names (`expertcheck_test.TestCases`, `TestCasesFileRules`, `TestCasesUpdate`, `TestRunCases`; `schema.TestFixtures` over `cases`).
- **[C4]** `compa expert routing` compiles each manifest's routing keys into the library under the keys file's keys, experts in ID order with their relationship and provenance, refuses an alias, an unknown key, keys naming experts and names of two keys, fails on a stale library and refuses changed content under the same revision; `compa expert versions` fails on a package changed with its version kept, and passes a new version or a change to the package's Go file alone (`expertcheck_test.TestCompileLibrary`, `TestCompileLibraryRefuses`, `TestRoutingCommand`, `TestVersions`).
- **[C4]** Load a v1alpha3 manifest with `metadata.summary`; reject one without a summary, with a summary over 160 characters or over a line, at v1alpha2, or carrying `spec.analysis` or `spec.evidence.suggested`; still load catalogs and tests files at v1alpha2 (`schema.TestFixtures` over `manifest-v1alpha3`, `expert.TestLoadRejects`, `TestLoadV1alpha2Rejects`, `TestDiscoverV1alpha2Fixtures`).
- **[C3]** Refuse a tool call naming no installed tool, or with invalid parameters, with the reason as its result, and run the valid calls of the same response (`engine.TestCompanionRefusesCalls`).
- **[C4]** Load an expert into a conversation; upgrade the installed copy; verify the conversation runs its tests at the pinned version, from the case's snapshot, and the conversation `/compact` starts takes the upgrade (`engine.TestCompanionPinAcrossUpgrade`).
- Run without access to external expert repositories or model APIs, using the fixture provider.
- Reject a manifest naming a model provider or model ID.
- **[C3]** Load one unchanged expert in a conversation with the fixture provider; the record shows its pin and the model of each call (`engine.TestCompanionConversation`).
- **[C3]** Preload the primary expert of the case's routing key before the opening turn's first call (`engine.TestCompanionConversation`).
- **[C3]** Consult an expert: its answer returns as the tool's result, its transcript is kept (`engine.TestCompanionConsult`), and its calls pass the barred-accounts rule and redaction, which run before every call (`companion.TestGateRefuses`, `TestRedactedBeforeEachCall`).

### Shared tool catalogs

Tagged **[TC]**, each with the test that gates it. Every case runs with no model, over the fixtures in `testdata/catalogs/`.

| Case | Test |
|---|---|
| **[TC]** A catalog package is validated like an expert, with its own rules: tool IDs under the catalog ID, a description and fixtures for every tool, no `interpret`, analyst text, routing or MCP; IDs unique across experts and catalogs | `expert.TestLoadV1alpha2Rejects`, `TestDiscoverIDClashBetweenKinds`, `TestDiscoverIDClashWithDuplicates` |
| **[TC]** A catalog range must stay within one compatible line | `semver.TestCheckCompatible`, `expert.TestLoadV1alpha2Rejects` |
| **[TC]** An expert whose analyst text does not name a tool it lists loads with a warning naming the tool | `expert.TestLoadWarnsOnUnexplainedTool` |
| **[TC]** Two experts each resolve and pin the catalog version within their own range | `expert.TestResolveCatalogVersionsPerExpert` |
| **[TC]** A pinned catalog survives an upgrade or deletion of the installed copy through its snapshot; an explicit re-pin takes the upgrade; a tampered snapshot is refused | `expert.TestCatalogPinSurvivesUpgrade` |
| **[TC]** A dependency on an installed catalog that is unmet, no version in range or a listed tool absent, leaves the expert usable without the missing tools, warned at discovery and resolution and recorded unresolved in the pin | `expert.TestResolveUnmetCatalogs` |
| **[TC]** An expert whose catalog is not installed at all, or whose catalog ID is an expert's, is refused at discovery as a package that could not be loaded, the packages beside it used | `expert.TestDiscoverRefusesExpertWithoutItsCatalog` |
| **[TC]** An expert using no catalog has a pin without a catalog field | `expert.TestPinWithoutCatalogs` |
| **[TC]** A catalog tool's result records the catalog, not the expert, and is one evidence item for two experts | `evidence.TestCatalogToolIsOneEvidenceItem` |
| **[TC]** A catalog tool whose binary is not allowlisted reports unavailable and runs nothing | `evidence.TestCatalogToolUnallowlisted` |

- **[C4]** `load_expert` lists the expert's catalog tools beside its own tests, sorted, at the resolved versions, each by its summary, with a note on a tool whose capture kind is absent (`engine.TestCompanionCatalogTools`, `TestOfferedByCapture`; `expert.TestResolveCatalogVersionsPerExpert`).
- **[C4]** `run_tool` runs a catalog tool no loaded expert lists; invalid parameters are refused with the reason as the result (`engine.TestCompanionCatalogTools`).
- **[C4]** A fixture-provider conversation runs a catalog tool through `run_tool`, and a later turn reads its result by name; an expert using no catalog and one using a catalog are loaded in one conversation (`engine.TestCompanionCatalogTools`).
- **[C9]** A catalog's capture kind names an `identity` tool of its own that takes `dir`; one naming a missing tool, a full ID, or a tool without `dir` or needing another parameter is rejected (`schema.TestFixtures` over `catalog` and `identity`, `expert.TestLoadCatalog`, `TestLoadV1alpha2Rejects`).
- **[C9]** The inventory runs each identity tool once over each attachment holding its kind's marks, stores each run as evidence, and applies the rules of currency over synthetic captures: of two captures of one cluster the later is current and the earlier outdated, superseded by it, whatever the attachments' numbers; different clusters are both current; a document has no currency (`engine.TestInventoryCurrentAndOutdated`); one sosreport extracted twice is a duplicate of the first (`engine.TestInventorySosreportExtractedTwice`); an unread target, an identity tool that failed, an invalid line, a capture outside the attachment, two lines that disagree, and times of one target with and without a zone are uncertain, saying why (`engine.TestInventoryUncertain`). The item is made again only when an attachment or what it was identified as changes, and a cancelled reading records nothing (`engine.TestInventoryRemadeOnlyOnChange`).

## Open decisions

Precedence when routing-key mappings disagree; schema naming/final field definitions; multi-expert conflict and priority semantics; signed release distribution; initial primitive set and risk taxonomy; scope of references and semantic indexing. Resolve through an approved architecture decision rather than implicit implementation.
