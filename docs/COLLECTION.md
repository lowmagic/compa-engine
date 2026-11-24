# compa — Collection Contract

This document governs how the engine collects data from case artifacts: built-in primitives, commands, the runtime of expert tests, and the sandbox they run in. Companion to `docs/EXPERT_API.md`, which defines how experts declare tests, and `docs/MODEL_API.md`, which defines the conversation and the tool calls through which a model asks for collection.

## Principle

Only the engine collects. A model never reads files, runs commands or reaches data except through the tool calls of its conversation. The engine validates each call before it runs, stores the result as evidence, and returns it to the model as the call's result (§Results).

Every collection runs through one runner, whatever asked for it:

- a model's tool call, in the main conversation or in a consult: a primitive, an expert test or catalog tool through `run_tool`, or a command through `command`;
- the engine's own ingestion;
- the engine's runs of the catalogs' capture `identity` tools when the chat opens a case (`Session.ReadInventory`): each once over each attachment whose contents hold its kind's marks, at most as many attachments as the overview lists, with the attachment's directory as `dir`, under the runner's sandbox, allowlist, limits and timeout like any tool, each record stored as evidence; an attachment its source reports unchanged, with the same identity tools installed, keeps the runs made before, read back from the evidence (`docs/EXPERT_API.md` §Catalog packages). They call no model, so they run on a case barred from AI processing as on any other; the inventory made from them is what a model may later be shown, under the barred-accounts rule and redaction like everything else.

The engine's evidence tools (below) read the evidence store rather than the case's artifacts, so they run in the engine, not the runner; their results are views of what is stored and are not stored again.

The engineer's interactive shell is separate and is not subject to this contract; its output enters the case only through explicit ingestion (`docs/SPEC.md`).

## Built-in primitives

Read-only operations over the case's artifact root, implemented in Go so their behavior and output limits do not depend on what is installed on the host.

| Primitive | Purpose |
|---|---|
| `find` | List files or directories by glob, size, modification time and depth; with `details`, each with its kind, size and modification time |
| `grep` | Regex search with file globs, context lines and a match limit; `keep` keeps each matching file's first lines alongside its matches, such as a table header; `from: end` keeps the last `max_matches` matches of the whole input instead of the first, each with its context, in the order read, holding them within the output cap |
| `read` | A line or byte range of a file; head and tail |
| `jq` | Query JSON (gojq or equivalent) |
| `yq` | Query YAML, such as must-gather resources |
| `logs.window` | Slice log lines by time window |
| `logs.count` | Count repeated lines; first and last occurrence |
| `aggregate` | Count, sort-and-count, unique values |
| `diff` | Compare two files or two resources |
| `archive.list` | List the members of an archive |

Each primitive but `aggregate` is offered to the model directly as a tool of the same name with each `.` written `_`: `logs_window`, `logs_count`, `archive_list`; the others keep their names. `aggregate` reads a previous stage's output, so it is a stage of `command`'s pipeline. A test's steps and a pipeline's stages name primitives by their dotted names.

Each primitive takes typed parameters, resolves paths relative to the artifact root and rejects any that escape it, orders its output deterministically, and caps its output at a configured size. Truncated output is marked as truncated, never presented as complete.

`find` lists what lies under `path`, the root by default, in lexical order of path, one entry a line: files (`type: file`, the default), directories (`dir`) or both (`any`), never the directory `path` names. `glob` matches an entry's base name; `min_size` and `max_size` keep files by bytes; `newer_than` and `older_than`, RFC 3339, keep files and directories modified strictly after or before. `min_depth` and `max_depth` count depth as find(1) does, from `path`: an entry directly inside it is at depth 1, and a file `path` names is at depth 0. Each is 0 to 64, 0 setting no limit, and `min_depth` may not exceed `max_depth`; a value out of range fails a package's load, and a collection giving one runs nothing. An entry shallower than `min_depth` is not listed, though a directory there is still searched; a directory at `max_depth` is listed if it matches and never entered, so nothing below it is read or counted. `max_results`, 1,000 by default, counts the entries listed; one more within the depths marks the output truncated, stopped at the match limit. With `details: true`, each line is four fields separated by tabs, the path last: `file` or `dir`; the size in bytes, `-` for a directory; the modification time in UTC, RFC 3339 with the fraction of a second the file carries, if any (`2026-10-02T00:00:01.5Z`), so the time printed is the time `newer_than` and `older_than` compare, though a time with a fraction does not sort as text among times without one; and the path. The details come from the walk's own listing, and no file is opened; the time is the one the file carries in the artifact root, as its source fetched and extracted it. Without `details`, a line is the path alone. The filters, the order, `max_results` and the stop record are the same with details as without, and a walk passes excluded entries, symlinks, files that are not regular and files holding only credentials before any depth or filter applies (`collect.TestFindDetails`, `TestFindDetailsKeepTheFilters`, `TestFindDepth`, `TestFindDepthAndMaxResults`, `TestFindPrunesAtMaxDepth`, `TestFindRefusesBadDepths`, `TestWalksSkipExcludedPaths`, `TestCredentialsOnlyFilesAreNeverRead`).

`grep` prints each match as `<file>:<line>:<text>` and its context lines as `<file>-<line>-<text>`. `keep`, 0 to 100 (0 by default), keeps the first `keep` lines of each file that has a match, emitted once in line order alongside its matches — with `-` when kept as header or preamble, and `:` when the line is also a surviving match — without counting toward `max_matches`. A file with no match emits none of its leading lines, and a `keep` out of range fails a package's load and refuses a run (`collect.TestGrepKeep`, `TestGrepKeepFromEnd`, `TestGrepRefusesBadKeep`).

The line primitives (`grep`, `read`'s line ranges, head and tail, `logs.window`, `logs.count`, `aggregate`, `diff`) read a gzipped file (`.gz`, not a gzipped tar) decompressed, such as a must-gather's node journals; the per-file read bound counts decompressed bytes and compressed ones alike, so a file that decompresses without end, or of empty members, is cut where a plain one would be. A byte range of a gzipped file is refused. A file read only to the bound, plain or gzipped, marks its stage truncated; every read stops when the collection's time is up, even one that writes nothing.

`logs.window` keeps the lines whose leading stamp falls in `[start, end)`, both RFC 3339; a line without a stamp follows the decision for the line before it. `layout`, a Go time layout, reads the stamp; without one, RFC 3339 and three `YYYY-MM-DD HH:MM:SS` forms are tried. A layout matches the line's prefix of as many whitespace-separated fields as it has, so a space-padded day (`_2`) may follow a run of spaces, and text closing the stamp, such as a bracket, need not be in the layout. Optional, each changing nothing when absent: `skip`, characters before the stamp, such as klog's level letter or an opening bracket; `anchor`, an RFC 3339 time, usually the capture's, that dates stamps without a year — each takes the latest year that puts it no more than a day after the anchor, line by line, so unordered input is dated correctly and a stamp more than a year older than the anchor is not; and `zone`, an IANA name or a fixed offset (`+05:30`, `+0530`), for stamps whose layout has no zone, UTC otherwise. A file with lines of which none has a stamp the layout reads fails the stage with that reason, since an empty window would read as no activity. Layouts per file kind are data in the catalogs that use them, not in the engine.

### Exclusions

Some entries directly under the artifact root are never collected. The case store, `.compa`, is always one: a deployment may make the case folder both the store's home and the artifact root. A deployment adds others in `collection.exclude`, such as an artifact tool's own state directory. A primitive refuses an excluded path, and any path whose symlinks lead into one, since an archive may carry a relative link; a walk from the root passes excluded entries by. A refused path says it is excluded rather than that it does not exist.

Exclusions name entries at the top level only, not names at any depth: a sandbox mechanism can deny a fixed path cheaply, and matching a name anywhere would mean walking the tree before each command.

**Files that hold only credentials** are never read by a primitive, at any depth. The deployment names them, compiled in, so no configuration file can remove one: a base name matches a file of that name, and a name with a directory, such as `auth/kubeconfig`, a file whose path ends with it, gzipped or not. Naming one, directly or through a symlink, is refused as holding only credentials; a walk passes over it. An OpenShift deployment, for example, names the files an OpenShift installer leaves. Files that hold secrets among what a diagnosis needs, such as MachineConfigs and installer logs, are read, and redaction on egress replaces their secrets (`docs/MODEL_API.md`). The rule applies to primitives, not to sandboxed commands, whose sandbox denies only the top-level exclusions above. It is a safety boundary (`docs/standards.md` §Safety), held by `collect.TestCredentialsOnlyFilesAreNeverRead` and `TestCredentialFilesAreTheDeployments`; a deployment tests the names it gives.

## Commands

A command is an **argv array**, for example `["omc", "get", "pods", "-n", "openshift-etcd", "-o", "json"]`.

- **No shell.** The engine executes the binary directly. Shell metacharacters (`|`, `;`, `$(…)`, globs) are passed as literal arguments; nothing is expanded.
- **Pipelines** are a list of stages, each a primitive or an argv command. The engine connects each stage's output to the next stage's input itself.
- **Allowlist.** The first argv element must name a binary on the deployment's allowlist, which is engine configuration. A deployment adds the binaries its experts need — an OpenShift deployment adds `omc`, for example. Experts declare the binaries their tests use; a test whose binary is not allowlisted reports unavailable.
- **Argument rules.** Each allowlisted binary carries data-defined rules that reject arguments able to execute code or write files, even inside the sandbox — for example `find -exec`, `-execdir`, `-delete` and `-fprint`; `awk` `system()` and output redirection; `sed` `e` and `w` commands.
- **Automatic execution.** An allowlisted, read-only command runs without confirmation. It appears in the chat's tool line as it runs, and the engineer can cancel the turn.

## Sandbox

Every command, and every argv stage of an expert test, and every expert MCP server runs in a sandbox with:

- the artifact root mounted read-only, with excluded entries beneath it unreadable, listing and metadata included, whether or not they exist yet;
- a scratch working directory, removed afterwards, as the only writable location;
- no network access unless the test declares a need and configuration allows it;
- no credentials, and a minimal fixed environment;
- a timeout, a memory limit where the mechanism has one, and an output cap.

Primitives run in the engine's own process, not in the sandbox: confined to the artifact root by `os.Root`, never following a link out of it, refusing anything that is not a regular file without waiting on it, and bounded by the run's timeout, which also stops a read in progress, each file's read bound and the output cap. `jq` and `yq` hold one matched file and its documents at a time.

**A run that stops early says how far it got.** Its record's `stopped` says why it ended before its input did — `timeout`, `match_limit`, `output_cap` or `read_bound` — and, for a primitive reading files, the file and line it stopped in, how many files it was given and opened, and the first twenty it never reached with a count of the rest, with any note such as which matches `from: end` kept or a file read only to the read bound. The first stage to stop is the one recorded. Its evidence opens with the stop in one line, before its output, whether the run failed (a timeout) or not (a match limit): `stopped: reached its match limit in node-a/journal.log at line 4211; not reached: 37 of 52 files (node-b/journal.log, …)`. A run may be allowed more time than the pipeline timeout, 60 s, up to a ceiling, 5 minutes by default (`collection.limits.maxTimeoutSeconds`): a model asks with `timeout_seconds` on any collecting tool call, the tool's declaration stating the default and the ceiling, and one over the ceiling is refused with why. The allowance is part of what ran, so a longer run is other evidence. The engineer sees a stop on the chat's tool line and in the full listing `ctrl+o` prints: `12 lines · stopped at the match limit · 3 of 5 files not read`, or `timed out after 60 s`. Going past the ceiling is not offered, and a run does not resume where an earlier one stopped.

The mechanism is the platform's default unless the deployment passes its own (`engine.Options.Sandbox`): `sandbox-exec` on macOS, and on Linux bubblewrap, in new user, mount, network, PID, IPC and UTS namespaces over a root holding only `/usr`, a few files from `/etc`, the binary's directory, the artifact root and scratch, each denied entry covered by an empty node no one may read. Memory is the process's address space, bounded on Linux by `prlimit` where the host has it; `sandbox-exec` bounds none, and a collection's record names the mechanism that ran it. CPU is bounded by the timeout alone. The properties above are the contract, and every mechanism passes `engine/sandboxtest`.

## Evidence tools

Every conversation, and every consult, offers three tools the engine runs over the case's stored evidence, so a model can read what it was shown only by excerpt, the part of a large item it needs, a comment whole, or the lines two logs share. They are declared as `evidence_list`, `evidence_read` and `evidence_grep`, and name items by their short names, `e12`, where the table below says ID:

| Tool | Parameters | Returns |
|---|---|---|
| `evidence.list` | `type`, `contains`, both optional | The stored items, newest first, at most 100: ID, type, bytes and lines, source and location. `type` keeps one evidence type; `contains` items whose source or location contains the text |
| `evidence.read` | `id`; `from` (1 by default); `lines` (200 by default, at most 400) | That item's lines from `from`, numbered, each cut at 2,000 bytes, the whole cut at 64 KiB with where to read on |
| `evidence.grep` | `pattern` (RE2, at most 200 characters); `ids`, up to 10 IDs separated by spaces; `context` (0–5, 0 by default); `max` (1–200, 50 by default) | Each item's matching lines, numbered, with the context asked for, a gap marked `--`; at most `max` matches and 64 KiB in all, and the first 64 MiB of each item |

They reach any of the case's evidence except what the engineer has left out of what models read (`comment_exclusions`): such an item is refused by name, missing from the list, and skipped by grep. Test IDs beginning `evidence.` stay reserved for the engine, and no package may use them (`docs/EXPERT_API.md`).

## Results

Each collection is stored as evidence with:

- what ran: the test ID, version and parameters, and the package that defines the test — the expert, or for a catalog tool the catalog and its version, never the expert that asked, so one tool run with the same parameters for two experts is one piece of evidence; or the exact argv or pipeline;
- the artifact paths it read, or for an evidence tool the evidence items it read;
- exit status, duration, and whether output was truncated;
- the bounded output.

A knowledge-base lookup is not a collection: its result is stored as `reference_search` or `reference_document` evidence, its provenance the service and `reference`, never as a collection's record (`docs/MODEL_API.md`).

The model receives the result as its tool call's result: the evidence's short name (`e12`), its size, and an excerpt bounded at 16 KiB and 400 lines that says where to read on with `evidence_read`. The tool call stays in the conversation, recording who asked — the main conversation or a consult — and the next model call carries it.

## Expert MCP servers

For logic that primitives and commands cannot express, an expert package may ship an MCP server (stdio). It declares each tool's input and output schemas, a `read_only` risk class, a resource budget and whether it needs the network. The engine:

- is the server's only client; no model can reach it;
- starts it once per case, inside the sandbox above;
- rejects any tool not declared `read_only`;
- stores each tool result as evidence, as for commands.

An MCP server is the exception. A test that can be expressed with primitives and commands should be.

## Contract tests

The tags are group labels, naming each case's area: **[P1]** collection and the sandbox, **[P4]** exclusions, **[C3]** the conversation loop, **[C9]** the case's state and inventory.

- **[P1]** Reject an argv command whose binary is not allowlisted, one using a denied argument (`find -exec`, `awk` `system()`, `sed` `e`), and any path outside the artifact root.
- **[P1]** Pass shell metacharacters in argv and pipelines as literal arguments; nothing is expanded or interpreted.
- **[P1]** Cap each primitive's output and mark truncation.
- **[P1]** Verify a sandboxed command cannot write outside its scratch directory, read outside the artifact root, or reach the network when no need is declared.
- **[P4]** Verify no primitive and no sandboxed command reads or lists an excluded entry, directly or through a symlink, with the case store open in the artifact root; walks from the root pass excluded entries by.
- **[P1]** Verify an expert test that uses an unallowlisted binary reports unavailable and does not run partially.
- **[P1]** Verify each collection is stored as evidence with what ran, exit status, duration and truncation.
- **[P1]** Verify an expert MCP tool not declared `read_only` is rejected, and that the server is reachable only by the engine.
- **[C3]** Verify a model's tool call runs, its result is stored as evidence and returned as a bounded excerpt with its short name, and the next model call carries it (`engine.TestCompanionConversation`).
- **[C9]** Verify the identity tools run through the runner over each marked attachment's directory, each run stored as evidence with what ran, and a cancelled reading records no inventory (`engine.TestInventoryCurrentAndOutdated`, `TestInventoryRemadeOnlyOnChange`).

## Open decisions

- The initial allowlist and argument rules for each deployment.
- Default output caps, timeouts and resource limits.
- Which language runtimes a deployment provides for expert MCP servers.
