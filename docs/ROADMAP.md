# Roadmap

The order compa-engine gets built in. Each slice is usable on its own, names what a user would notice and the
[spec](SPEC.md) sections it delivers, and has a done-when someone can demonstrate. A requirement not yet in a slice
stays in the spec and waits for one here.

## 1. The companion as a library

Status: done

A deployment passes a case source, an artifact source, a bundle of experts and its configuration in
`engine.Options`, and its binary gives an engineer the chat: a case opened and oriented, questions answered with
tools run in the sandbox and their results kept as evidence, experts loaded or consulted, drafts written, each answer
checked against the evidence it cites, and usage recorded for every call.

Delivers: Acquisition, Evidence storage and retrieval, The companion, Expert packages, The model layer, The
engineer's shell without the embedded shell, Usage without re-sending a saved call, User interface.

Done when: `make check` passes with no provider reachable; and a deployment's binary on a synthetic case with the
fixture provider opens the case, answers a question citing evidence a tool returned, writes a draft kept as an item,
and shows the case's usage after it is reopened.

Answer-check coverage includes hyphen and en-dash citation ranges and preserving the cited source bytes for
both checkers (`TestCitationEnDashRanges`, `TestCheckerQuotesEnDashRange`). Retained model text stays labelled
as model text; consult handoffs carry claim bases, and action premises and scoped negative checks remain reviewable.
The opt-in `TestLiveEpistemicReview` and `TestLiveEpistemicPackets` add synthetic paired evaluations with
report-only rubrics. Each was run once live, one run per scenario: the harness ran end to end after one fix, isolating
each review scenario's store from the stub case; they make no empirical benefit claim and do not replace behavioral
evaluation of deployment-specific experts.

## 2. The deployment's house section

Status: done

The companion's prompt names roles, not an organization, and a deployment says in its configuration who "support" is,
which knowledge sources it has and its tools' conventions. The engine appends that text to the system prompt as a
section of its own, inside the prefix a provider caches.

Delivers: The companion (the prompt's method, shared by every deployment).

Done when: no prompt file names an organization; a configuration's house text appears once in the system prompt,
before the first cache break; and the live scenarios and the checks' benchmark are compared before and after the
rewording on one model.

The comparison was run five times per arm on gemini-3.8-flash, from one tree: `TestLiveCompanionBehaviour`'s fourteen
scenarios and `TestLiveCheckReplay` on that model, the prompt as it was with no house section against the prompt
naming roles with a deployment's house section (`COMPA_LIVE_HOUSE`). Every step of both arms ended without error.
Most yes-or-no measures matched in every run. Three did not: where the customer tells two nodes' captures as one
node's disk filling, the answer kept the nodes apart in five runs before the rewording and three after, the other two
repeating the customer's progression; the turn after the opening of the standing-theory scenario cited the refuting
line in three runs before and two after; a compaction's handoff named the engineer's last message in four runs before
and five after. Own-voice certainty terms and code spans found in no evidence were a little more frequent after. The
checkers' prompts are the same bytes in both arms, yet the editor caught fewer shapes in the second arm and the
reviewer more, which is the size of the model's variance on these runs. Five runs per arm show no effect of the
rewording that the variance does not cover; the told-progression scenario is the one to watch.

## 3. Re-sending a saved call

Status: done

An engineer debugging an answer can send a stored call again, to the same provider or another, and compare what
comes back.

Delivers: Usage ("Allow a saved call to be re-sent").

Done when: a stored call is re-sent on the fixture provider, its result is recorded as a call of its own linked to
the original, and nothing in the conversation changes.

`compa case resend <case> <call> [--model name [--effort effort]]` sends the request again, to its model or a
configured one, and prints the original's answer beside the new one (`docs/ARCHITECTURE.md` §History and re-running).

## 4. Expert MCP servers

Status: planned

An expert can ship a server for logic that primitives and commands cannot express, and its tools run like any
other: sandboxed, read-only, their results evidence.

Delivers: Expert packages ("An expert may ship an MCP server").

Done when: a fixture expert's server answers a tool call inside the sandbox, a tool not declared read-only is
refused, and no model can reach the server.

## 5. Posting a private comment

Status: planned

An engineer can post a draft to the case as a private comment from the chat, where the deployment's case source
allows it.

Delivers: Acquisition ("posting private case comments").

Done when: a case source that implements posting receives the item the engineer chose, exactly as kept, only after
the engineer confirms it; and a source that does not implement it offers nothing.

## 6. An embedded shell

Status: planned

An engineer can keep a shell open beside the chat, inside tmux or outside it, and offer what a command prints as
evidence.

Delivers: The engineer's shell ("A persistent embedded shell").

Done when: a command run in the embedded shell inside tmux is offered as evidence without a nested tmux, and
nothing is kept until the engineer accepts it.

## 7. An OpenAI-compatible provider

Status: done

A deployment can name models on OpenRouter, or on an inference server of its own, for any profile: the engine calls
one OpenAI-compatible Chat Completions endpoint, held to the egress class the deployment declares for it and redacted
by that class's policy.

Delivers: The model layer (the providers).

Done when: a fixture endpoint's streamed answer, tool calls and usage are read, an endpoint declared past
`reasoning.maxEgressClass` is refused before it is reached, and a two-tool turn runs live through OpenRouter, the
second call reading from cache.

## 8. More than one OpenAI-compatible endpoint

Status: planned

A deployment can name models on OpenRouter and on an inference server of its own at once, each endpoint with its own
base URL, key variable, egress class and cache marks, where configuration now holds one endpoint.

Delivers: The model layer (the providers).

Done when: two configured endpoints each answer a profile's calls from a fixture server, each held to its own egress
class by the gate and redacted by its policy, and a model names its endpoint without naming a URL; and a deployment's
example configuration names an OpenRouter model beside its Vertex ones.

## 9. Knowing an endpoint's cache

Status: planned

Reopening a conversation on an OpenAI-compatible endpoint says when its provider's cache has expired, as it does for
Vertex, whether the endpoint caches by the engine's marks or on its own: an endpoint's configuration may say how long
its cache lives, where only the marks' lifetime is known now.

Delivers: The companion (reopening, "whether the provider's cache has expired").

Done when: a reopened conversation on an endpoint with a configured cache lifetime says the cache expired only past
it, and one with none says nothing of a cache.

## 10. The command line apart from the engine

Status: done

The `engine` package holds the engine and its two APIs, what a deployment passes in `engine.Options` and the
`Session` its clients use (`docs/ARCHITECTURE.md` §The engine's APIs). The command line is `engine/cli`, which a
deployment's `main` hands the arguments and its options, and which runs the chat, `engine/chat`; both are clients of
the public API alone. A deployment no longer passes the chat in `engine.Options`.

Delivers: User interface (the commands as they were; nothing an engineer notices changes).

Done when: package `engine` imports no `flag` package and holds no usage text; both deployments' binaries build on
`engine/cli`, and `compa <case>` and `compa chat` run in a terminal; no file under `engine/chat` imports `internal/`;
and the linter fails a change that gives `engine/chat` an import beyond `engine`, `engine/text`, `engine/enginetest`
and third-party modules.

## 11. Commands a deployment names

Status: done

The answer checks treat a code span that opens with a program and its arguments as an instruction, not a claim. The
engine names only general-purpose programs; a deployment names its products' CLIs in `checks.commands`
(`docs/MODEL_API.md` §Answer checks).

Delivers: The companion (the answer checks, with no vendor's tool in the engine).

Done when: `internal/check/statements.go` names no product's program; a span of a program the configuration names is
not flagged and one it does not name is (`engine.TestCheckSpanCommand`); and a deployment that names its programs
checks answers as before, shown by its own test.

## 12. A base configuration beneath the deployment's

Status: done

The engine ships a base configuration layer, `internal/config/base.yaml`, beneath a deployment's defaults: the house
style and the intentions for case comments every deployment starts from. A deployment's defaults keep only what is its
own; one that names a style or intentions replaces the base's whole (`docs/standards.md` §Config).

Delivers: The companion (drafts by intention, in a house style, for a deployment that names none).

Done when: an engine built with no comments drafts with the base's style and intentions, and one naming its own has
exactly those (`engine.TestBaseConfigUnderTheDeployment`); and each deployment's effective configuration is the same
bytes before and after its defaults drop what the base holds.

## 13. One fixture-case format

Status: done

Every test with fixtures keeps its cases in one format the engine defines, and `compa expert check` runs them,
comparing each result exactly, so an experts repository keeps no runner of its own (`docs/EXPERT_API.md` §Fixture
cases).

Delivers: Expert packages (experts define their tools declaratively, checked by the engine).

Done when: `compa expert check` runs the cases of both expert repositories' bundles, failing on a result that differs
(`expertcheck_test.TestCases`), and neither repository has a case runner of its own.

## 14. Routing libraries and versions checked by the engine

Status: done

An experts repository compiles its routing library and checks that a changed package took a new version with the
engine's own commands, `compa expert routing` and `compa expert versions`, keeping only the data its keys come from
(`docs/EXPERT_API.md` §Compiling a routing library and checking versions).

Delivers: Expert packages (the routing library maintained with the experts, by the engine's rules).

Done when: a library compiled from a keys file and the manifests reads back by the engine's rules and refuses an alias
or an unknown key (`expertcheck_test.TestCompileLibrary`, `TestCompileLibraryRefuses`); a stale library and a changed
package with its version kept fail (`TestRoutingCommand`, `TestVersions`); and no experts repository copies the
registry's name rule or the version check.

## 15. Next steps the engineer answers by number

Status: done

An answer ends with what could be done next, in two lists: the steps the companion can carry out itself, numbered,
and what only the engineer or the customer can do, in a list of its own with no numbers. The engineer answers with a
number alone and the companion carries that step out. A draft and a consult's answer carry neither list.

Delivers: The companion (the prompt's method, shared by every deployment).

Done when: on the live model, an answer to a direct question about a capture that holds more than the question needs
ends with both lists, the engineer's unnumbered, and the message "1" is carried out
(`TestLiveCompanionBehaviour`, scenario `steps`).

The scenario ran three times on gemini-3.8-flash at medium. Every answer ended with a numbered list of two steps and
a list for the engineer of one item, not numbered. Answered "1", each run carried its first step out with the tools,
in 1, 1 and 2 calls, and one ended that answer with new lists numbered from 1. An earlier set of three, whose report
cut the lists' text, counted 2, 3 and 1 numbered steps and one unnumbered item each. Before the prompt said to end
every answer so, a short answer carried no numbered list in two runs of two, and the companion answered "1" by saying
it had numbered nothing. It is the model's behaviour on one synthetic case: nothing in the engine reads the lists.

## 16. Restrained Markdown, lettered next steps and quiet chat footers

Status: done

Answers render with restrained Markdown — tiered headings (`#` underlined, `##` coloured, `###` plain bold), plain `-`
lists, child and continuation paragraphs aligned under their parent item's text, indented fenced code blocks preserving
`#` lines with fence markers hidden, and routine `- **Label:**` bullet bolding stripped — and prompt next steps with
capital letters (`A.`–`D.`) under `Next (enter a letter, or ask a question):` while accepting uppercase or lowercase
replies. The empty input cues next-step letters when the latest answer offered them, bare `f` and `r` single-key
interception is removed so ordinary messages beginning with `f` or `r` never trigger checks or paid reviews, and
post-answer metadata collapses to a single quiet footer line.

Delivers: The companion, User interface.

Done when: `make check` passes; `chat.TestMarkdownRestrainedHierarchyAndNesting` verifies tiered headings, nested list
and continuation alignment, and indented fenced `#` lines; `chat.TestInputPlaceholderTracksNextSteps` verifies the
context-aware reply cue; and `chat.TestLeadingRTypesNormallyAfterDraft` and
`chat.TestLeadingFTypesNormallyAndCheckOpensDraftFindings` verify normal input without single-key interception.

## 17. Compact-tag case ledger without item IDs or duplicate verbs

Status: done

`/ledger` (`engine.RenderLedger`) renders open asks under `Awaiting customer:` and `Awaiting <Support>:`, pending
commitments under `By <Support>:` and `By customer:`, and comment items under compact `[ask]`, `[commit]`, `[report]`
and `[provide]` tags with trailing `(answered in c#N)` statuses — dropping `lN` IDs and per-line actor names that the
section or comment header already gives. `ledger_update` instructs the model not to open `text` with redundant actor or
kind verbs, and `RenderLedger` deterministically strips leading duplicate `<who> <verb>` prefixes and normalizes
configured support whitespace on stored rows too.

Delivers: The companion, User interface.

Done when: `make check` passes; and `engine.TestRenderLedgerByComment` and `chat.TestLedgerCommand` verify compact-tag
rendering on both fresh and legacy stored ledger rows.

## 18. Three-mode comment drafts, optional steering and post-editor citation stripping

Status: done

The base comment intentions consolidate to `internal` (internal note), `update` (customer update and read-only data
requests) and `kcs` (reusable KCS draft), each instructing restrained Markdown and inline fenced code blocks naming real
files or commands. `Session.Draft` accepts `<mode> [intention]` so free-text steering appends while preserving the
mode's audience and certainty rules. `[eN:lines]` citations stay through `CheckCites`, `AnswerFlags`, the editor and
anchor revision, and remain in the stored conversation history for `/review [aN]` and ranked evidence, while
`engine.StripDraftCites` strips them outside fenced blocks and inline code for displayed and copied drafts.

Delivers: The companion, User interface.

Done when: `make check` passes; `engine.TestBaseConfigUnderTheDeployment` verifies the three base modes;
`engine.TestEditorDraftKeepsCorrection` and `engine.TestStripDraftCitesAndCustomIntention` verify steering, post-editor
`[eN]` stripping and citation resolution on `/review a1`; and `chat.TestReviewCommand` verifies `/review a2` on a draft.

## 19. Browser tabs for comments and turns, and two-key verdicts

Status: done

The evidence browser's four tabs are `evidence`, `comments`, `turns` and `checks`: the comments tab replaces the items
tab and `/comments`, keeping its own list beside the evidence's; `/turns` opens the turns tab, which lists each answer
as it was shown — drafts as `aN · draft: <mode>` with citations stripped — `o` adding the editor's steps and `w`
showing the turn open wrapped with no line numbers so the mouse selects clean text. The checks tab takes two verdict
keys: `y` or enter supported, `x` wrong — unsupported where no line is close and none is marked, else incorrect.

Delivers: User interface.

Done when: `make check` passes; `chat.TestComments`, `chat.TestBrowserTurnsAsStored`, `chat.TestEditorCorrectionShown`
and `chat.TestChecksSummary` verify the tabs, the raw view, the pipeline toggle and the two verdict keys.

## 20. Ten commands, copying by answer and a quiet reopen

Status: done

The chat's commands are ten: `/draft`, `/copy`, `/turns`, `/ledger`, `/evidence`, `/check`, `/review`, `/model`,
`/compact` and `/debug`. `/copy [aN]` and `ctrl+y` copy an answer or draft as it was shown, a draft's citations taken
out, in place of pinned items; `/debug` takes `on`, `off`, `usage`, `experts`, `load <expert>` and `ranked`, which
replace `/usage`, `/experts`, `/load` and `/ranked`. `/compact` in a case is the summary reload, carrying the case's
state, the ledger, the ranked evidence and the last exchange beside the handoff. A reopened case offers no panel: its
lines say what is new and what the next message resends, and the next message goes on. `/analyze`, `/pin`, `/items`,
`/reload` and `/catchup` leave the chat; the engine keeps them.

Delivers: User interface.

Done when: `make check` passes; `chat.TestDraftKeptAndCopied` verifies `/copy`, `ctrl+y` and a refused `/copy a9`;
`chat.TestReopenIsQuiet`, `TestReopenLines` and `TestCompactInCase` verify the quiet reopen and `/compact`;
`chat.TestDebugCommands` and `TestCompletionKeys` verify `/debug`'s arguments and the menu.

## 21. Argument popovers

Status: done

Past a command's name and a space, the `/` list offers its arguments, filtered as typed and never printed: `/draft`'s
modes, `/copy`'s answers newest first with the start of each, `/model`'s models then the chosen model's efforts, and
`/debug`'s arguments. Enter runs the option chosen and tab completes it; an argument typed whole runs as typed.

Delivers: User interface.

Done when: `make check` passes; `chat.TestArgumentPopovers` verifies each command's options, tab and enter, and that
no row enters the transcript; `chat.TestCompletionExactArgumentRuns` verifies an argument typed whole runs as typed.

## 22. The engineer's shell and compa in beside the chat

Status: done

`!` opens the engineer's shell in the case's folder under a three-line banner, with `COMPA_CASE` and `COMPA_INGEST` set;
`!<command>` is refused, saying how. While the chat holds the case it serves the store to `compa in` over an
owner-only Unix socket, named to the shell and in the store's directory, so `compa in` reaches it from the shell or any
other. In a compa shell `compa in ["question"] [-t tags] [-f file …]` takes the case from `COMPA_CASE`, the question
and tags kept as the note; outside one, `compa in <case> …` is unchanged. Back from the shell the chat lists what was
kept, and the next message names it.

Delivers: The engineer's shell.

Done when: `make check` passes; `engine.TestIngestServer` verifies the socket's modes, the store's pointer, ingesting
and closing; `cli.TestParseIn` and `TestIngestIntoAHeldCase` verify both forms of `compa in` and reaching a held
case; `chat.TestShellOpensWithCompaIn` verifies the banner, the environment, the refusal, the list and the next
message.

## 23. Expert reference files, read on request

Status: done

An expert can declare reference files (`{path, summary}`) in `spec.references` for lookup material that does not belong
in every conversation's `analyst.md`. Loading the expert lists them with their summaries, and `read_reference` reads a
declared reference whole from the conversation's pinned package snapshot as domain guidance (not evidence) — offered
only when an installed expert declares a reference, isolated per consult, bounded by a 32 KiB soft warning in
`compa expert check` and a 64 KiB hard refusal, redacted on egress, counted in the command check across compaction, and
recorded in the trajectory.

Delivers: The companion, Expert packages.

Done when: `make check` passes; `expert.TestLoadRejects`, `TestLoadReferenceWarnings`, and
`cli_test.TestExpertCheckReferenceWarnings` verify validation and bounds; `engine.TestExpertListsReferences`,
`TestReadReferencePinnedAndRepeat`, `TestReadReferenceRepeatInTurn`, `TestReadReferenceSameBatch`,
`TestReadReferenceRefusals`, `TestReadReferenceConsult`, `TestReadReferenceAbsentWhenNoneInstalled`,
`TestReadReferenceOfferedForPinnedSnapshot`, `TestReadReferenceRedacted`, `TestUnsourcedCommandFromReference`, and
`TestTrajectoryReferences` verify runtime behavior, command checks, and trajectory; and
`chat.TestReadReferenceLabels` verifies the chat's status and tool-line labels.

## 24. Grep keeps leading lines alongside matches

Status: done

`grep` takes an optional `keep` parameter (0 to 100, 0 by default) that keeps the first `keep` lines of each file with
a match, such as a table's header or file preamble, in line order without counting them toward `max_matches` or
duplicating them when context or matches overlap, in both `from: start` and `from: end` runs, and refuses out-of-range
values at load and run time.

Delivers: Collection.

Done when: `make check` passes; `collect.TestGrepKeep`, `TestGrepKeepFromEnd`, `TestGrepRefusesBadKeep` and
`TestCheckStage` verify single- and multi-file runs, match and context overlap, match-limit accounting, `from: end`
eviction and cap holding, zero-value identity, and out-of-range refusals.
