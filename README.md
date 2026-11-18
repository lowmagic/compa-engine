# compa-engine

The engine behind compa, a triage and data-gathering companion for support cases, as a Go library. It does not
diagnose: it says what a case's data shows, with the evidence, asks what the data leaves open and says what would
settle it; the engineer draws the conclusions. An engineer talks to a model about a case in one conversation; the
engine owns everything around that conversation:

- the case, read through a `CaseSource` and an `ArtifactSource` a deployment supplies;
- collection from the case's artifacts, by deterministic tools run in a sandbox, each result stored as evidence;
- the conversation, sent in an order a provider can cache, with every tool call a model makes validated before it runs;
- expert and catalog packages, loaded and pinned by content digest;
- redaction before anything leaves the host, and a rule that bars cases from model calls;
- checks of each answer against the evidence it cites;
- the command line, `engine/cli`, and the inline chat it runs, `engine/chat`.

It has no `main`. A deployment builds the `compa` binary from it: [compa](https://github.com/lowmagic/compa) is the
public one, with experts from [compa-experts](https://github.com/lowmagic/compa-experts).

## Running it

A deployment's `main` passes its parts in `engine.Options` to `engine/cli` with the command line:

```go
os.Exit(cli.Main(os.Args[1:], engine.Options{
	Cases:     stubcases.New(casesDir),   // a CaseSource
	Artifacts: localdir.New(artifactsDir), // an ArtifactSource
	Config:    defaults,                   // YAML in the schema under api/config/
	Bundle:    bundle,                     // experts and catalogs, an fs.FS
}))
```

- `make check` runs the checks CI runs, offline. `make standards` checks the org standards copies and needs the
  network.
- Tests need no model: they run on a fixture provider. Live tests are opt-in, with `COMPA_LIVE_VERTEX=1` and Google
  Application Default Credentials.

## Documents

- [Specification](docs/SPEC.md): what compa must do
- [Architecture](docs/ARCHITECTURE.md): the design, its terms and invariants
- [Technical stack](docs/TECH_STACK.md): the chosen technology and the code layout
- [Model API](docs/MODEL_API.md), [Expert API](docs/EXPERT_API.md) and [Collection](docs/COLLECTION.md): the contracts
- [Roadmap](docs/ROADMAP.md): the order it gets built in
