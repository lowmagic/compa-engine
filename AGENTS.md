# Working on compa-engine

The engine behind compa: a troubleshooting companion's conversation loop, tools, sandboxed collection, case store and
answer checks, as a Go library. This file governs developing it, not the models it invokes.

Before planning, implementing, reviewing or answering questions about the product, read these in full, in order:

1. [Specification](docs/SPEC.md): what the product must do
2. [Architecture](docs/ARCHITECTURE.md): the design, its terms and invariants
3. [Technical stack](docs/TECH_STACK.md): the chosen technology and the code layout
4. [Coding standards](docs/standards.md): how the code, tests, docs and work are done, the safety rules first, on top
   of the org's [principles](https://github.com/lowmagic/.github/blob/main/PRINCIPLES.md) and standards
   (`docs/org-standards*.md`)
5. [Expert API](docs/EXPERT_API.md): expert and catalog packages
6. [Model API](docs/MODEL_API.md): the conversation, its tools, providers, egress and usage records
7. [Collection](docs/COLLECTION.md): primitives, commands, the sandbox and the evidence tools

Then the [roadmap](docs/ROADMAP.md) for implementation work.

Docs live in `docs/` and change with behavior. Never commit customer data, hostnames, tokens or deployment config (see
Safety in the [standards](docs/standards.md#safety) and Privacy in the [org standards](docs/org-standards.md)).

Follow [docs/standards.md](docs/standards.md): its Safety rules outweigh every other, and its Working section is the
workflow. Run `make check` before committing. It runs everything CI runs, except the org standards check:
`make standards` (needs the network).
