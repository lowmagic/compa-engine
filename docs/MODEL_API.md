# compa — Model contract

This document governs the **runtime model interface**: what the engine sends a model, what it accepts back, and what it
records. It is not development instructions. Companion to `docs/EXPERT_API.md`.

## Terminology

The companion, the conversation, a consult, the model layer and a provider are defined in `docs/ARCHITECTURE.md`
§Terminology. Two more terms:

- **A turn**: one message of the engineer's and everything the companion does until it answers.
- **A model call**: one request to a provider and its response; a turn holds one or more.

## Purpose and boundaries

The model reads the case, calls the engine's tools, loads expert guidance and writes answers in plain language. The
engine owns everything that makes that safe and reproducible.

| Concern | Owner |
|---|---|
| The conversation: its store, its order, compaction | Engine |
| Sources, acquisition, evidence and its storage | Engine |
| Tools: their table, validation, running them, storing their results | Engine |
| Collection (primitives, catalog tools, commands, sandbox) and permissions | Engine |
| Experts: index, loading, pinning, consults | Engine |
| Redaction, the barred-accounts gate, usage and cost records | Engine |
| What to say, which tools to call, which expert to load | Model |
| Domain guidance, routing signals | Expert package |
| One call's transport, credentials, cache marks, usage normalization | Provider |

Nothing the model writes is stored as fact. Its messages are stored as its messages; tool output is evidence. There is
no findings table for it to write into. A model that is slow, wrong, unavailable or malicious can degrade the quality
of an answer and nothing else.

## The conversation

One conversation per case, or per troubleshooting chat, owned by the engine, stored in the case store
(`docs/ARCHITECTURE.md`), and sent whole on every call. A conversation is the model's context, not the case's record:
the record is the evidence the tools stored and the messages as they were said.

**Order.** Everything a provider sees is append-only after a fixed prefix:

1. **System**: the companion prompt, the expert index (`docs/EXPERT_API.md`), the house style and intentions for
   case comments (`comments.style`, `comments.intentions`), the engine's base unless the deployment's configuration
   replaces them, and the deployment's house section (`companion.house`) where it has one. The companion prompt names
   roles — the support engineer, the support organization, a knowledge-base article, the product's CLI — and no
   organization; the house section is
   the deployment's free text saying who support is, which knowledge sources it has and its tools' conventions,
   appended as the last section, under a heading of the engine's, as written: nothing in it is templated, and it
   replaces none of the prompt. A deployment with none has no such section. The same bytes for every case and every
   call of a build and its configuration.
2. **Tools**: the declarations of §Tools. The same set, in the same order, on every call; none appears or disappears
   when an expert loads.
3. **The case block**: an introduction saying what the block holds, then the case's details and description; the
   artifact inventory where the case has one (`docs/ARCHITECTURE.md` §Data model), each identity run's evidence by its
   short name, in place of the attachment overview, and the overview otherwise; the newest five comments, digested by
   the deterministic comment digest, newest first; and every older comment on one line of its short name, date and
   author kind, read whole with `evidence_read`. Each item is under its short name, comments the engineer leaves out
   of what models read are left out, and the same case makes the same bytes. Written once when the conversation
   starts; a troubleshooting chat with no case has none.
4. **Messages**: the engineer's, the model's, and each tool call with its result. Comments or attachments that arrive
   later are appended as a `# Case update` message, holding the current items no case block or update of the
   conversation has shown yet — an older comment's line counts as showing it — never rewritten into the block; an item
   named elsewhere, as the case ledger names the comments it reads, is still shown
   (`engine.TestCaseUpdateShowsCommentsTheLedgerNamed`). An expert's guidance enters as the result of `load_expert`.
   When the case's routing key has a primary expert in the routing library, the engine's own `load_expert` call and its result
   follow the case block before the first turn; no model call made it, so no invocation records it. The case ledger
   (§The case ledger) follows as a `# Case ledger` message where it holds items, before the first call; at the start
   of each later turn, after any case update, what changed in it since the conversation last saw it — new items and
   items whose status changed — is appended as a `# Case ledger update`. What a conversation has seen is read from
   its own ledger messages, so a reopened or compacted conversation is judged by what it holds; nothing is appended
   while a turn runs. When the editor (§Answer checks) corrects an answer, its correction follows the answer as a
   message of its own; when it leaves anchors, the engine's request for one revision follows, then the main model's
   revision; where a revision is not taken, failed or cancelled, a message records what the engineer was shown. The
   answer as written is never rewritten, and the main model's next call reads what was shown. A review the engineer
   asks for (§Answer checks) is appended as a message of its own when no turn runs, and read at the next call.

**Preparing a case.** Before the opening turn of a case with no conversation yet, and before `compa ask`'s turn, the engine
reads the comments, then counts and inventories the attachments while the ledger reads the comments it has not; the
opening waits for the ledger up to 60 seconds from its start, and past that orients without it, the engine's
instruction saying so, the ledger reading on and reaching the conversation at the next turn. A case reopened on its
conversation, and `--no-open`, make no model call, the ledger's included, until the engineer chooses one (Reopening,
below); a case whose model calls are refused starts no ledger.

**Consults.** `consult(expert, question)` runs a sub-conversation with the same system prompt, tools and case block —
the same cached prefix — then each case update the main conversation was given since, later comments and attachments
and the case ledger alike, so a correction after the case block reaches the expert; then the expert's guidance and the
question with the context the main model passes. It runs
its own tool loop under the same budget and returns its answer to the main conversation as the tool's result. Its
transcript is kept and linked to the call that started it. Several consults in one response run concurrently.

**Compaction.** When a call's estimated input passes a configured threshold, 150,000 tokens by default, the turn's
reply says so and the chat offers `/compact`. `/compact` has the model write a handoff — each material point as
observed, attributed and to whom, or inferred, cited with the identity and time window it holds for; the strongest
evidence against each open explanation, and every correction and retraction; what is ruled out, by which check and
only as far as it reached; evidence missing, truncated or uninspected kept as unknown; the open hypotheses with what
would discriminate them; what is awaited from whom; the experts loaded; and the engineer's last request — and starts
a new conversation from the case block as the case is now, the whole case ledger and the handoff, linked to the old
one, which stays. Above the model's context limit a turn compacts on its own first and says so, where the model has
answered in the conversation: one holding only its case block has nothing to hand off. Nothing else is summarized or
dropped: a provider keeps nothing between calls and compacts nothing.

**Reopening.** What a reopened conversation has yet to be shown is judged against the conversation the engineer last
worked: one a reload or a compaction began is, until its first turn, judged by the one it continues. Where the case
gained comments or attachments, a catch-up has the ledger read the comments it has not, waited for as the opening
waits, then runs one turn on the opening's budget: the case update and the ledger's update are appended at its start
as at every turn, and the engine's instruction names the new comments and attachments and asks, briefly, what changed
and whether it changes the current understanding. With nothing new it calls no model. Where the provider's cache has
expired on a conversation past a size, there are three ways on, none taken without the engineer:

- **Full**: the conversation as it is; its next call resends it whole without the cache, its estimated tokens said.
- **Suggested**, with no model call: a new conversation linked to the old one as a compaction's is, begun from the
  case block as the case is now, the whole case ledger, and one message of the engine's, `# Carried from the earlier
  conversation`, in the place of a compaction's handoff: the engineer's pinned items and drafts as a compaction carries
  them; the top of the ranked evidence, at most 12 items and 24 KiB of their lines in all, each under its name, kind
  and why it ranked, with the lines cited or pointed at read from the evidence and numbered as stored, or its first
  lines where none was named, the item reaching the bound cut and said so, and items the engineer leaves out of what
  models read left out; and the last exchange verbatim, the engineer's last message and the model's last answer, said
  to be the model's reading. The same inputs make the same bytes. After it, each of the engineer's verdicts that a
  span is unsupported or incorrect (§Answer checks) that no model call has read yet, in order, as messages of their
  own.
- **Summary**: the same, with the model's handoff of the old conversation before what is carried, asked for as a
  compaction asks: one call over the old conversation, given what the case gained first; the verdicts carried are
  those unread before that call.

The ranked evidence is derived each time from the stored messages of the conversation and its compaction ancestors,
not its consults, and never stored: each evidence item the conversation named scores for what was done with it —
lines the engineer pointed at, a citation in a pinned item or draft, a citation in another of the model's answers, an
excerpt a tool returned — weighted in that order and decayed by the turns since; the case block's own items, which
are always readable by name, are left out. Reading it calls no model and writes nothing.

**Pins.** An expert loaded into a conversation stays pinned to it (`docs/EXPERT_API.md`); a bundle update reaches a
conversation only through `/compact` or a new conversation.

**A model change.** `/model` changes the model for the next call and keeps the conversation: its messages are the
engine's text, sent whole to whichever model is selected. The next call reads nothing from a provider cache made for
another model. Data a provider attaches to a message for its own use, such as a thought signature, is sent back only
to the model that wrote it.

**A policy change.** A conversation is redacted on every send by the current policy (§Egress), never stored redacted,
so a new policy version applies to the next call whole.

One model call at a time runs in a conversation; a consult's calls are its own.

## Provider caching

Asking a provider to cache a prefix is caching the engine requested. The engine marks cache breaks on messages: after
the case block, and at the end of the last message of every call, so each earlier call's end stays a break. A break
says the prefix ending there is sent unchanged in every later call.

- **Claude** takes `cache_control` breakpoints, four at most a call: one on the system prompt, which caches the tools
  before it too, and one on each of the conversation's last three breaks, so a call reads everything through the
  previous call's end from the cache (`vertexanthropic.TestCacheBreakpoints`). The cache lives Claude's default five
  minutes. A prefix under the model's minimum is silently not cached.
- **Gemini on Vertex** cached nothing implicitly in live use, so the Gemini provider makes an explicit cache of the
  system prompt, the tools and the conversation through its last break before the newest message, where that is at
  least Vertex's 4,096-token minimum by the estimate. Where that break is results alone, the cache ends at the model's
  calls before them: Vertex refuses a cache ending in function responses ("Requests ending with a model turn are not
  supported") and takes one ending in the model's signed calls (live), so within a tool loop the cache
  still grows (`vertexgemini.TestExplicitCacheInToolLoop`, `TestLiveOpeningCache`). Tools and the system instruction
  must be inside a Gemini explicit cache, which holds them; a call using the cache sends neither again. A call reuses
  the longest live cache whose contents its own still begin with, sending the rest as new content, and caches the
  whole again only once what lies past the cache passes a quarter of what it holds, so a conversation growing at its
  end is not cached anew each turn (`vertexgemini.TestExplicitCacheGrows`). A cache lives ten minutes unless
  configuration says otherwise (`reasoning.vertex.cacheMinutes`) and expires rather than being deleted; a prefix
  Vertex refuses is not tried again, and the call reads the older cache it has, or none. Each call says what it did —
  made a cache, reused one, a make refused with Vertex's error, or none under the minimum or without a break — with
  the messages the cache it read holds and their estimated tokens; its invocation record keeps that
  (`vertexgemini.TestExplicitCacheGrows`, `TestExplicitCacheSkipped`).

Cache reads appear in usage as `cache_read_tokens`, and the status line shows the share of input read from cache.

## Tools

Declared once, statically, in this order, and offered on every call.

| Tool | Does | Runs in |
|---|---|---|
| `find`, `grep`, `read`, `jq`, `yq`, `logs_window`, `logs_count`, `diff`, `archive_list` | The engine's primitives over the artifact root, with the parameters `docs/COLLECTION.md` gives them | the engine (`os.Root`) |
| `command` | An argv command under the allowlist and its argument rules, its output optionally read by primitive stages in turn (`then`), `aggregate` among them | the sandbox |
| `run_tool` | Any installed catalog tool or expert test, by ID, with its parameters | the runner |
| `describe_tool` | A catalog tool's or test's full description and parameter schema | the engine |
| `evidence_list`, `evidence_read`, `evidence_grep` | The stored evidence by short name (`docs/COLLECTION.md` §Evidence tools); `evidence_read` also reads a comment whole | the engine |
| `load_expert` | One expert's guidance and its tools, each on one line with its parameters (required ones marked) and summary, appended as the result, the expert pinned | the engine |
| `consult` | One expert, one question, as a sub-conversation (§The conversation) | the loop |
| `<service>_search`, `<service>_fetch` | The operations of each reference adapter the deployment passes, as its offer gives them: `kcs_search` and `kcs_fetch` for a service `kcs` | the adapter |

A function name is the tool's name; a catalog tool or test is named by its ID as `run_tool`'s parameter, so no ID is
written into a function name. The expert index is in the system prompt, so no tool lists experts; an expert's
reference files are offered once an installed expert ships one.

- **Validation before running.** Each call is checked before it runs: the tool exists, its parameters validate against
  its schema, a command's binary and arguments pass the allowlist and its argument rules, a `run_tool` ID names an
  installed tool or test whose parameters accept what was given. A call that fails is not run; its result is the
  refusal and its reason, and the turn goes on.
- **The question.** Every tool that returns evidence — the primitives, `command`, `run_tool`, the evidence tools and
  each reference service's tools, all but `load_expert`, `describe_tool` and `consult`, whose own question is the
  consult's — declares a required `question`, one line of a bounded length ending in `?`: what the call should settle,
  whose answer would change what the model does or concludes next, not a description of the call. The
  engine takes it out of the arguments before the call is validated and run, so no runner, catalog tool or argument
  rule sees it and no stored collection holds it; the stored call keeps it as the model wrote it. A call without one
  is run, not refused, since a refusal would cost a model call, and is counted (§Trajectory); a vague question, or one
  restating the action, is reported, never corrected (`engine.TestToolsDeclareTheQuestion`,
  `TestQuestionTakenOutBeforeTheCallRuns`). The chat's tool line gives a batch's calls in the order the model made
  them, though they run together, each with its own question after what it did: on one line where the batch fits the
  terminal's width, otherwise on a row for each call, cut to the width; live and reopened; `/debug` gives each whole
  (`chat.TestToolLineShowsTheQuestion`, `TestBatchedToolLineGivesEachCallItsQuestion`).
- **The answers.** Any call, one taking no question included, may give `answers`: a list of an item and, in one line,
  what it settled about the question it was fetched for, a search that found nothing with how far it read. They are
  taken out of the arguments as the question is and kept in the stored call. An answer is the model's reading, never
  evidence, and is checked as a statement, the call still run: one naming no item a tool of the conversation returned
  before the call, an evidence item's excerpt or the item an `evidence_read` or `evidence_grep` read, is flagged; its
  inline code spans are searched for in that item's lines the conversation was shown, as the span search reads them
  (§Answer checks), a command line not looked for and arithmetic evaluated, and one in none of them is flagged. The
  flags are given in the trajectory and, after the tool line, under `/debug`, each answer whole
  (`engine.TestAnswersDeclaredAndTakenOut`, `TestTrajectoryAnswers`, `chat.TestDebugShowsTheAnswers`). Answers stay
  optional and a call without them runs; the ask for them comes with each result (Results as evidence, below).
- **Results as evidence.** A collection's and a lookup's result is stored as evidence with its provenance, and
  returned to the model as its short name (`e12`), what ran, its size in lines and bytes, then, where the call asked a
  question, that question again on a line of its own asking for what the item settled in the next call's `answers`, or
  its citation if the model answers instead, then any note on how it ran, and an excerpt bounded at 16 KiB and 400
  lines, under the stored line numbers, that says where to read on with `evidence_read`. The head stays the first
  line, so a client restoring a stored result names its item from it, and the question's line is no line shown
  (`engine.TestExcerptEvidence`, `TestExcerptAsksForTheAnswer`). The evidence tools are views of what is stored, not
  stored again, so a line cited from one is the stored item's line. Short names are given once per case, in the order
  items are named, and never renumbered or reused, so `e12` means the same item in every conversation, consult,
  compacted conversation, item and command.
- **Timeouts.** A collecting call may ask for more time with `timeout_seconds`, up to the runner's ceiling; the
  allowance is part of what ran.
- **Lookups.** A lookup redacts its query and applies the barred-accounts check before anything leaves the host. A
  search's outcome is `complete` (with no hits, a search that found nothing, not a failure), `truncated`, `denied`,
  `failed` or `unavailable`; a fetch's `complete`, `more_pages`, `truncated`, `denied`, `failed` or `unavailable`. A
  result other than a complete or truncated one says so in its evidence's note.
- **Soft budget.** At most 40 tool calls in one engineer turn, consults included, and 8 in the opening turn, whose
  case block and ledger already hold the case's state. A call past it is refused saying the budget is spent, the
  results message says so, and the next request asks for an answer (`NoCalls`). A model that still calls tools gets
  two more calls, each refused, and the turn then ends with what it has, saying why. The engineer's next message,
  `continue` or anything else, is a new turn with a budget of its own.
- **Token budget.** Beside the calls, a turn's model calls, consults included, may send at most 400,000 tokens of
  input, summed over the calls: each call sends the whole conversation, so a long tool loop costs about calls times
  context. A call counts once its provider reports its usage: its uncached and cache-written input whole, and what
  it read from the provider's cache at a tenth, the price of a cache read beside input on Gemini and Claude;
  a call whose provider reports no input or cache-read count counts its estimated input whole (§Usage accounting's
  estimator) (`companion.TestTokenBudget`, `TestTokenBudgetCountsCacheReadsAtATenth`). Past it the turn ends as past
  the soft budget, the refusal and the results message saying the token budget is spent; the next turn has a budget
  of its own. With cache reads counted whole, turns in live use ended well short of the soft budget.
- **Concurrency.** The tool calls of one model response run concurrently, at most 8; the results keep the calls'
  order.
- **Failure.** A tool that fails returns its failure as its result; it never ends the turn.

## Citations

The model cites evidence by short name and lines, `[e12:41-58]` or `[e12:41]`, or an artifact by `path:line`; an item
cited whole, `[e12]`, and several in one bracket, `[e1, e4]` or `[e1, e4:2]`, are read too. The chat resolves each to
its evidence or file and renders it as a link to those lines. A citation naming evidence the conversation was never
shown, lines of it the conversation was not shown, an unknown path, or lines past the end is shown flagged, the
lines it was shown said. A bracket naming case-ledger items, alone or among evidence names, `[l26]`, `[l26, e45]` or
`[e36, l22, e63:4]`, is read too: each evidence name is checked as any, and each ledger item is flagged, never
resolved as evidence, as the ledger's reading of its comment, `c#12 (e45)`, whose lines to cite instead, or, where
the ledger has no such item, as naming nothing (§The case ledger; `engine.TestLedgerItemCitedFlagged`,
`TestCitesReadLedgerItems`, `chat.TestLedgerItemCitedFlagged`). What reads a text's citations as evidence — the span
search's closest line, the checkers' quoting, the ranked evidence, the trajectory's cited items — takes the evidence
names of such a bracket and never a ledger item.

Line ranges accept either a hyphen or an en dash, including repeated ranges in one evidence bracket and
artifact paths: `[e12:1–2, 4–5]`, `capture/records.log:1–2`. An em dash is not a range separator.
`engine.TestCitationEnDashRanges` covers parsing, `TestCheckerQuotesEnDashRange` covers source bytes reaching
both checkers, and `TestCheckMalformedCitation` distinguishes valid ranges from malformed brackets.

Retained answers and drafts keep their model origin when carried into a new conversation: pinning is not
verification or engineer endorsement. Consult handoffs distinguish observations, attributed reports and hypotheses,
with evidence names and lines and material corrections or alternatives. `engine.TestToolsDeclareTheQuestion`
requires consult's unbounded, required question context. The editor labels a mechanism asserted beyond the evidence
as `unestablished_link`; the reviewer checks factual premises even in proposed actions. A negative check excludes
only what its conditions and coverage could detect.

`engine.TestLiveEpistemicReview` and `TestLiveEpistemicPackets` are opt-in, manually adjudicated evaluations,
not semantic regression gates. Fixtures and guidance are written from no case, loaded through the normal expert
loader without an external bundle. They exercise paired evidence conditions, real consults, pinning, reloads and
checker packets, but are not equivalent to behavioral tests of deployment experts. Each was run once live, one run
per scenario on gemini-3.8-flash: every fixture loaded and every scenario, packet and rubric ran, and the run found one
harness fault, since fixed: a review scenario's store also held `liveSession`'s stub case, which the evidence tools
reached. No benefit is claimed for them. `COMPA_LIVE_VERTEX=1` enables provider calls;
`COMPA_EPISTEMIC_SCENARIOS`, `COMPA_EPISTEMIC_CHECKERS`, `COMPA_LIVE_RUNS` and
`COMPA_EPISTEMIC_REPORT` select scenarios, checker roles, repetitions and an external report.

**Exposure.** What a conversation was shown is recorded as the engine builds each request, never read from the text:
each engine-authored message is stored with the lines of each item it gives with its content, after its bounds — an
item of the case block or a case update under its heading, whole, or the lines a digest kept; the lines an excerpt,
an `evidence_read` or an `evidence_grep`'s matches with their context give under their numbers; the lines a reload
carries and a verdict quotes; a checker's quoted lines in its own conversation — and each request sent, past the
gate and redaction, records the messages it carried. A name alone shows nothing: an older comment the case block
names on its line, an item too large to give, named with its size, an evidence listing, the comments the ledger's
items name and the engineer's words. Nothing a model wrote — an answer, a correction, a handoff, a review, a
consult's question or the answer a consult returned — shows anything, so a citation never shows its own item
(`engine.TestCiteOfUnshownItemStaysFlagged`, `TestCiteOfNamedOnlyItemFlagged`, `companion.TestExposureRecordsWhatWasSent`).
A citation resolves on the lines the conversation's requests carried, those of the conversations it was compacted
from, and those its consults read, said to be the consult's read (`engine.TestCiteOutsideAPartialReadFlagged`,
`TestCompactionCarriesExposure`, `TestCiteOfAConsultsReadSaysSo`); an item cited whole resolves on any of its lines.
The lines a reload carries and those a verdict quotes are shown as a tool's are (`engine.TestReloadCarriesRankedLines`,
`TestVerdictRestsOnLinesShown`).
What an editor or a reviewer was quoted is recorded in its own conversation and resolves no citation of the answer it
checks (`engine.TestCheckersReadShowsTheAnswerNothing`). A conversation stored before the record was kept has its
exposure unknown: nothing is inferred for it, its items are judged by the names it holds — an item given under its
heading, a name in the engineer's words or a tool's result — and its citations say so
(`engine.TestUnrecordedConversationExposureUnknown`). The chat shows a citation resolved on a consult's read, or by
names where the exposure is unknown, resolved, not flagged, and beneath the answer one note, a line for all of them,
names them by the showing each rests on, three of each named and the rest counted, live and on reopening
(`chat.TestCitationOfAConsultsReadNoted`, `TestViaLineSummarises`). The span search and the engineer's verdicts read
the same lines (§Answer checks). Nothing is dropped, and nothing the model cites is stored as a finding.

Beside them, the checks find a citation written in a form the chat does not read — a name and lines without
brackets, a name in parentheses, a bracket that is not a citation's — where the name is one the conversation saw, and
an inline code span, which the prompt keeps for paths, resources and error strings, found word for word in no
evidence the conversation saw. These and the flagged citations are counted in one line beneath the answer and listed
in the browser's checks tab (§Answer checks).

## Answer checks

The checks run without a model, on the text of every answer, live and
reopened; then, where a deployment names an `editor` profile, the editor, before the answer is printed; and on the
engineer's request, where it names a `reviewer` profile, the reviewer.

A draft the customer will read is asked with the certainty review
(`internal/companion/customer.md`): each claim reviewed silently for its evidence, its strongest alternative and the
evidence that would tell them apart; the wording no stronger than the evidence, "the evidence strongly suggests" the
most certain in the model's own voice; certainty words and causal proof never in its own voice; a root cause only as
attributed; and the escalations of time, recovery, analogy and absence rejected. It goes with the request, not the
system prompt, so a conversation begun before it has it; an internal note is asked without it.

- **Statements.** For the checkers' requests only, an answer is split into numbered statements, `s1`, `s2`, …, so a
  finding can name one: each list item and each sentence of a paragraph, with its citations as the chat reads them;
  fenced blocks, headings and rules are not statements. The answer shown and stored is unchanged.
- **Found without a model, counted beneath the answer:** the citation forms and the code spans of §Citations, and
  the lines of a fenced block
  presented as output, each searched for as a span under the statement presenting the block: a block with no info
  string after a statement that cites evidence, or one naming plain text or a log, none of whose lines is a command
  line; a line number a tool gave a line is taken off, a line is cut at an ellipsis, and a part of fewer than a few
  characters is not searched for. A bare block after a statement citing nothing, or a block of a shell, YAML, JSON or
  any other language, may be a query or a change to make, and is not searched (`check.TestExcerpts`,
  `engine.TestCheckExcerptLines`). A span is found where an item
  holds it as written, or once the model's rewording is undone: its words adjacent and in order on one line, case,
  spacing, punctuation and digit grouping aside (`MemTotal: 1,048,576 kB` finds `MemTotal:    1048576 kB`), an
  ellipsis a cut; a different value, key or order is not found. A span that is a command line — a program with its
  arguments, the program one of the engine's own general-purpose ones (`check.Commands`) or one the deployment names
  in `checks.commands`, such as its product's CLI — is an instruction, not a claim, and is not searched for; a
  product's program the configuration does not name is a span like any other. A
  span that is only arithmetic is evaluated, not searched for: numbers (digit grouping allowed, decimals, none with a
  leading zero), `+ - * /`, at least one of them between two operands, parentheses, spaces and the comparisons
  `= != < > <= >=`, at least one of them, with at most one unit word, the same after every number that has one
  (`16 GiB / 4 = 4 GiB`); units are not converted, and `=` and `!=` compare to the precision written, the sides equal
  within half the last decimal place of the number written with the most (`10 / 3 = 3.33` holds). One that holds is
  not flagged; one that does not is flagged `incorrect: the arithmetic does not hold`, the engine's own verdict.
  Anything else — a comparison with no operator, such as versions (`4.9 < 4.14`), a number with a leading zero, such
  as a date (`2026-11-06 < 2026-11-08`), a division by zero or a second unit — is searched for as any span. The span
  search reads the lines the conversation was shown, as a citation resolves on them (§Citations' Exposure): an item
  carried whole, or named where the exposure is unknown, whole, any other only at the lines carried, so a span only in
  lines never shown is in no evidence the conversation saw (`engine.TestSpanSearchReadsOnlyLinesShown`). Those the
  engineer left out of what models read are excepted; it reads up to a bound for one item and one for the check, stops
  when every span is found, and says when a bound cut its reading of lines shown. A
  cancelled check ends. In the same pass, with no second read, each span not found is
  given its closest line: of the whole lines shown read, the line sharing the most of its words (words as the search makes
  them, each counted once), and at least half of them, rounded up, so one word in common, such as a node's name, is no
  closeness; a line of an item its statement cites preferred on a tie, then the earliest item's, then the earliest
  line. A span no line is that close to is shown as found nowhere in the evidence, unsupported.
- **The engineer's review of the findings.** Beneath an answer the chat prints no finding itself. One yellow line
  counts them all — each citation flagged, each citation not in the form read, each span flag, arithmetic included,
  each finding the editor flagged and did not correct, and each citation the revision added — as
  `⚠ 8 findings to review · f opens the checks tab` after the turn, taken as the review's offer is (`f` the first key
  on the empty input, any other input ending it; after a draft both are offered), and as
  `⚠ 8 findings, 1 judged · /check a1 opens them` otherwise, reopened included; there is none where there are none
  (`chat.TestFindingsInChecksTab`, `chat.TestAnswerFlagsShown`, `chat.TestChecksSummary`). Warnings, the editor's
  notes — a revision or follow-up not taken — and the footer stay inline. The anchors a revision was asked to fulfil,
  revised or failing, are no finding and not counted: one line sums them up by kind, most first,
  `asked of the revision: 13 anchors: 9 misattributed, 4 unsupported · f opens the checks tab`, or
  `· /check a1 lists them` where `f` is not offered, the anchors themselves listed in the checks tab, live and
  reopened (`chat.TestEditorAnchorShown`, `TestAsksLineSumsUp`); `compa ask`, which has no checks tab, lists each beneath
  the answer (`cli_test.TestAskPrintsTheCheck`). `f`, or `/check [aN]` for
  any answer, live or reopened, opens the browser's checks tab (`docs/ARCHITECTURE.md` §The evidence browser) at that
  answer's first span not judged, else its first finding; nothing is printed into the chat while it is open. It lists
  every finding of the answers printed this run, by answer: first the span flags, those with no close line and the
  arithmetic among them, then the findings for reading, then the anchors asked of the revision, each with its state: `?`
  unjudged, `✓` supported, `~` inferred, `⚠` unsupported, `✗` incorrect, `·` for reading — arithmetic that does not hold `✗` by the
  engine until the engineer judges it. The value under the cursor shows its claim, the statement holding the span as
  the answer shows it; the complaint; its verdict; and its evidence: the lines marked for it, else its verdict's, else
  its closest line, each with two lines before and five after a single line, one before and two after a range,
  numbered, the lines themselves shown as cited, as the browser shows the item (a one-file search's file name left
  off). `y` or enter: supported. `i`: inferred, which needs lines marked, and is refused saying so where none are.
  `u`: unsupported, resting on no lines. `x`: incorrect. `m` opens the item shown at its lines, and `e` the evidence
  list, as the evidence tab lists it, to open any item; in either, `v` starts a mark and enter keeps it for the value,
  ranges of several items allowed up to as many as a verdict may rest on, a mark past them refused saying so, `d`
  drops the last mark, there and in the tab, and `esc` returns to the list or the same value. The lines marked are the
  verdict's evidence, replacing the closest line; `y` and `x` with none marked rest on the closest line, or on none
  where there is no close line. ↑↓ move between values, tab switches tabs, `esc` returns to the chat, waiting for a
  verdict being kept, and the chat then prints one line for each answer judged while it was open: ``a1: 3 supported,
  1 inferred, 1 incorrect, 2 unsupported; the model reads the incorrect and unsupported at its next call``. No model
  is called. Every verdict is
  kept with its kind and its ranges, by the answer as the model first wrote it and the span, its statement read again
  from the answer and its lines from the stored evidence, never taken from the chat: lines past an item's end or the
  span search's bound for one item, a closest line not the stored line whole, lines the conversation was not shown, as
  the span search reads them, said with those it was (`engine.TestVerdictRestsOnLinesShown`), an item the engineer
  left out, and more than a few ranges are refused; one span is judged once. A verdict that the claim is
  supported or inferred is the engineer's record only and never sent. One that it is unsupported or incorrect is also
  appended to the main conversation as a message of its own, after every earlier message, which the main model reads
  at its next call, redacted as every message is: ``The engineer found no evidence for a1's `X`: treat it as
  unsupported.``, or ``The engineer checked a1's `X` against e57:3-5: it is incorrect.`` followed by each range's
  lines as stored, numbered, a few lines of each and a few KiB in all. A reload before that call carries it into the
  new conversation. It is the engineer's assertion, not evidence, and no answer: answer numbering, `/pin`, the ranked
  evidence and a reload's last exchange leave it out. Reopened, the checks tab shows each verdict. A finding for
  reading — the citation as written, or the editor's statement, kind and words at fault — shows why, the most the
  evidence supports, or for an anchor what was asked to fulfil it, and the lines it cites in context, marked `·`; the
  verdict keys there only say it takes no verdict, and `m` opens the cited item at its lines
  (`chat.TestFindingsInChecksTab`, `TestEditorAnchorShown`). A verdict
  kept before the four kinds reads as supported where the claim stood and incorrect where it was wrong; the messages
  the model read are as they were.
- **Candidates for the editor, never shown:** a statement whose every citation is a file listing (`find`,
  `archive.list`); a statement with a code span, a time or a count and no citation; a statement citing lines an
  earlier statement cited with a hedge (`likely`, `may`, `either`, …) that it no longer has; a statement with a
  certainty term or causal phrasing of the certainty review's banned list not directly after a named source.
- **Quoted lines.** One engine function quotes the lines a text cites from the stored evidence, byte for byte under
  their stored numbers, each item headed by what produced it, bounded for one citation and for all, a cut said where
  it falls; items the engineer left out are not quoted, and artifact paths are named, not read. The editor's and
  reviewer's requests quote by item: each evidence item the text cites, one cited at lines past its end included, is
  quoted whole, every line under its number, where it fits a bound of its own and what the request has left, the
  lines cited marked `▸` before their numbers and the citations naming it said; past either bound, the lines cited
  first, then up to a few lines either side, nearest first, while the window's bytes last, so context never crowds
  out a line cited, windows that meet merged only within the window's line bound, the heading saying which lines are
  quoted and why. A citation naming a whole item, a valid form for any item quoted, is said to be, and one naming
  lines past an item's end said so. Both requests stay within one bound, so the editor and the reviewer judge the
  same evidence, and the same inputs make the same bytes; the citation footnotes will use the quoting too.
- **Findings.** A checker answers through one tool, `check_findings`, whose arguments are its findings
  (`compa.lowmag.cc/check/findings/v1alpha1`): each finding its statement, kind (`certainty` among them), the words at
  fault, why, the lines it rests on and the most the lines would support; the reviewer's strongest material
  alternative and missing discriminator; the editor's mark that a finding is an anchor, needing the conversation; the
  statements supported and skipped; the editor's corrected text. Validation is the ledger's: arguments the schema
  refuses are refused whole; then each element is checked against the request — its statement given, its words in
  that statement, its lines among those quoted, a listing finding on a statement that cites a listing or rests on
  listings alone, what its role may not send, an anchor saying what fulfils it — and dropped with its reason; a
  finding whose suggestion keeps the statement as it is ("supported", "supported as worded") is dropped and its
  statement counted supported; every statement is accounted for or reported unaccounted. The editor's corrected text
  is dropped where no finding but an anchor calls for it, unless it differs from the answer only by citations in
  brackets, each checked against the lines quoted as any corrected citation is (`check.TestValidateRoles`,
  `check.TestValidateCitationOnlyCorrection`).
- **The editor.** A task of its own (conversation kind `editor`, task `answers.edit`, profile class `editor`), given
  the answer as written, its statements, the deterministic results and candidates, and the items cited, quoted whole
  (§Quoted lines), never the conversation; a draft with a statement citing nothing is also given, after its own, the
  items cited by the answers it was written from, those since the last draft, said so, so it can cite their lines for
  such a statement rather than anchor it (`engine.TestEditorRequestDraftSources`); its prompt includes `customer.md`'s
  rules. It judges each statement against
  the whole item and fixes what needs only those — certainty wording to the ladder, a cause in the model's own voice,
  a mechanism read beyond its lines, a citation whose claim other quoted lines hold, rewritten to those lines with a
  `citation_mismatch` finding naming the change — and returns the corrected text; what no quoted item can settle,
  needing the conversation, it leaves as anchors, each naming its statement, its kind and what fulfils it. The engine
  checks every citation the corrected text gives a statement that it did not have, the two texts' statements paired
  in order: one naming an item the request did not quote, lines past its end or lines not quoted is refused, the
  finding dropped with why and that statement printed as the answer has it; where the statements do not pair, a
  citation refused drops the correction whole. Where anchors remain, the main model is asked once, with no tools, to
  revise, given the corrected text and the anchors and told that it reads no evidence in that call: to keep each
  citation, cite only lines the conversation has shown, or qualify the claim. The deterministic checks run again on
  what it returns; a citation with lines neither the answer as written nor the editor's correction had is shown
  flagged as added by a call that read no evidence, and counted in the footer; anything else still failing is shown
  flagged. There is no second round. The editor makes at most two calls in all, whatever it answers, within a time
  bound; past it, or failing, the answer is printed as written, said unchecked. Findings that name wording to correct
  and give no corrected text would leave the answer as written, so while a call is left they are sent back once, the
  editor asked for the same findings with the correction; where that call gives nothing valid the first findings
  stand, shown flagged. Where its first valid findings leave
  statements unaccounted for and a call is left, the editor is asked once more in its conversation about those
  statements only, naming each, its answer validated as against a request holding only them; what it gives joins the
  first call's, the first call's account kept for a statement both give, its correction of those statements spliced
  into the first call's corrected text, or into the answer as written, statement by statement — dropped, with why,
  its findings shown flagged, where the statements do not pair or it changed any other — and its anchors go to the one
  revision with the first's. A follow-up that fails, is refused or passes the bound leaves the first call's result,
  said beneath the answer. What is printed, and what a draft keeps, is the answer as corrected or revised; the footer
  says how many statements the check accounted for where it did not account for all, `checked 11 of 22 statements`,
  read from the check as kept, and counts the statements the correction changed and the citations it corrected,
  `corrected by the editor: 2 statements, 3 citations`, and the original stays in the browser's turns tab. A
  revision not taken — failed, cut short, calling a tool, or cancelled — leaves what was shown recorded, its text
  never printed. A cancelled check or revision still records its state and prints the answer as
  shown before saying cancelled. A warning raised during the check or revision is printed after the answer, and the
  live view keeps the answer's rows until that print, so nothing of an answer not taken stays on screen. One
  projection, each answer as shown, serves the chat, reopening, `/pin`, `compa ask`, a reload's last exchange and the
  ranked evidence. Every check is kept in the `checks` table with its findings and outcome, written under a bound of
  its own, and its calls pass the gate, redaction and barred-accounts rule and count in the case's usage. A deployment
  with no `editor` profile runs no editor.
- **The reviewer.** A second opinion on request, from the model the `reviewer` profile names (conversation kind
  `review`, task `answers.review`, profile class `reviewer`): `/review` reviews the last answer as shown, `/review iN`
  an item, and after every draft kept the chat offers it on `r`, taken only as the first key on the empty input and
  ended by any other input. Nothing is called until the engineer asks. It is given the text under review, its
  statements and the items cited, quoted as the editor's are (§Quoted lines), never the conversation; its prompt
  includes `customer.md`'s four questions, and each finding gives the claim, why, its lines, the strongest material
  alternative, the missing discriminator and the most the lines would support; it corrects nothing, and a claim held
  by lines other than those cited is its `citation_mismatch`. `/review --ranked [iN]`, an experiment and never the
  default, also gives it, after the items cited, the top of the conversation's ranked evidence that the text does not
  cite, of the items disclosed to the companion, bounded as a suggested reload bounds what it carries, each item
  headed as disclosed to the companion and not cited by the text; the items cited are quoted within what the request
  leaves after it, and its lines are lines quoted, so a finding may rest on them (`engine.TestReviewRequestRanked`,
  `chat.TestReviewRanked`). At most two calls in all, within a time bound; failing,
  cancelled or past it, it is said and nothing is kept. Its review is appended to the main conversation as a message
  of its own, headed as another model's reading, not verified and never evidence, which the main model reads at its
  next call; nothing reviewed is rewritten, and a review is not an answer: answer numbering, `/pin`, the ranked
  evidence and a reload's last exchange leave it out. Its calls pass the gate, redaction and barred-accounts rule, are
  kept in `checks`, and count in the case's usage. A deployment with no `reviewer` profile has none, and `/review`
  says so.

## The case ledger

What each case comment asks, commits to, reports or provides, and what later comments made of it: a model's reading of
the comments, kept beside the conversation, shown as such, and never evidence or a finding.

**Calls.** The ledger reads the comments it has not, oldest first, at most 25 a call, until none is left; with none
unread it makes no call. A comment is read once in each text: a version saved with its body unchanged is not read
again, and an edited one is. Comments the engineer's rules leave out of what models read are not read. A comment too
long for a request to show whole is given to no call: the engine marks it not shown, never read, and it is not
offered again until an edit makes a new version. Each call is a
conversation of its own, kind `ledger`, apart from the main conversation and its consults, through the companion's
loop: the same gate (barred accounts, egress class), the same redaction, and an invocation record of task
`comments.ledger`, on the profile class `ledger`. Its system prompt is the ledger's own; its one message holds the
ledger so far (each item's ID, kind, who, status and comment), the edited comments with the items their earlier
versions gave, and the comments to read, each under its short name, digested as the case block's are.

**The update.** The model is given one tool, `ledger_update`, and answers by calling it with the update as its
arguments (`api/ledger/v1alpha2/update.schema.json`): `items`, each a kind (`asks`, `commits`, `reports`,
`provides`), who (`customer` or `support`), one line of text, the comment by name, a commitment's `due`, a provided
item's `attachment` as the comment names it, and a status; `updates` to held items by ID; `none`, the comments with
nothing to record and why (`courtesy`, `automated`, `repeat`, `nothing_new`). An ask is `open` until a later comment
has it `answered` or `ruled_out`; a commitment `pending` until `done` or `dropped`; what was provided stands until a
later comment has it `superseded` — a newer capture asked for or sent, or the state it captured reported gone — which
is the model's reading of the comments, shown beside whatever the engine finds in the attachments and never changing
it; any item is `withdrawn` by an edited version of its own comment that no longer says it. Each status cites the
later comment that sets it.

**Validation.** Arguments the schema refuses are refused whole, the refusal sent back as the result, and the model may
call once more; a model that answers in text is asked once to call the tool. Each element of an accepted update is
then checked: it cites a comment the call gave; a status settles or supersedes only as its kind allows, citing a given
comment written after the item's; a change names a held item not settled yet, or withdraws one by an edit of its own
comment. An element failing is dropped with its reason and the rest kept. The accepted call ends the conversation: the
model is not called again.

**Keeping it.** In one transaction, the new items are numbered `l1`, `l2`, … after the highest number the
transaction sees, in the thread's order; held items move; and every comment read is marked with what the update made
of it: items, a change, none with why, or `unaccounted` where the update said nothing of it. A batch with no update
accepted marks nothing read. Items from a comment the engineer later leaves out of what models read are left out of
what the ledger shows and of its next calls.

**Shown.** `/ledger` reads what is unread, then shows the open asks by who owes them, the pending commitments, and
every comment of the case, oldest first, numbered as the case numbers them, `c#1` for the oldest by when it was
written, an edited or excluded comment keeping its number. A comment with items is headed by its number, its author's
name, the configured `companion.supportRole`, `Customer` or `Automated`, `Public`, `Private` or `Partner`, when it was written in the engineer's
zone, and its evidence name, `### c#8 · Jo Synthetic · Customer · Public · Oct 29, 2026, 11:27 AM EDT · e8`, its items
under it with their statuses, a settled one naming the comment that settled it by number. A comment that gave none is
one line saying why: `nothing: courtesy` and the model's other reasons, not accounted for by the model, not read
for being too long to show the ledger's model, not read yet, left out by an exclusion rule, or read as an earlier
version. What the source does not say is left out
(`engine.TestRenderLedgerByComment`, `TestLedgerAccountsForEveryComment`). The conversation's own `# Case ledger`
message keeps its earlier form, the open asks, the pending commitments, what was reported and provided by comment and
the settled items one line each, bounded in size, the oldest left out first and counted, each comment headed by its
evidence name, then its number and header, times in UTC, since its lines are what later updates are compared with;
after them, every comment not shown for its length, by name and number, saying the ledger says nothing of it, which
an update names once when one is newly so (`engine.TestLedgerMarksTooLongCommentNotShown`).
The ledger's calls count in the status bar's total and in `/usage` with every other call of the case.

The ledger reads only its current schema, and files each item under one of two sides, `support` or `customer`: a
comment by an author of another kind is read like any other, and the model says which side its item is on
(`ledger.TestValidateRejectsUnknownWho`, `schema.TestLedgerVersionCutover`). The configured support name is used
in the ledger's owed lists and item lines, the case block, draft and review audiences and compaction handoffs; a
comment's header, and `compa case comments`' kind column, use the configured role instead
(`engine.TestRenderLedgerByComment`, `TestConfiguredSupportLedgerMessages`,
`TestConfiguredSupportCaseBlockAndHandoff`, `TestCustomerDraftCarriesCertaintyReview`).

## Egress

Every message and tool result is redacted by the provider's egress policy before it leaves the host, the case block,
expert guidance and a consult's messages included. A call still holding what its policy forbids is not sent, and is
recorded `egress_refused`.

**Redaction policies.** Two apply (`redact.TestPolicies`): `none-1` for `local_only`, which changes nothing, and
`secrets-5` for every egress leaving the host, `internal_network` included. `secrets-5` replaces each secret with a
typed placeholder such as `[REDACTED:token]`: private keys; tokens — JWTs, `sha256~` tokens and bearer tokens; a
password in a URL; a pull secret's `auth`; kubeconfig and Docker credentials (`client-key-data`,
`client-certificate-data`, `.dockerconfigjson`, `.dockercfg`); AWS access key IDs; and the value under a key naming a
secret, quoted, in escaped quotes or unquoted. A value under a key ending in the secret's own noun, such as
`DB_PASSWORD`, is redacted whatever its shape, a number included; a secret-named YAML block scalar is replaced, also as
grep prints it; and an Ignition-style data URL whose decoded payload holds a secret is replaced whole
(`redact.TestEachSecretIsRedacted`, `TestBlockValueEnds`). A key that only names a secret or where one is had, such as
`secretName` or `token_endpoint`, is kept, as are hostnames, addresses and names (`redact.TestNonSecretsAreKept`).
Every replacement keeps the line breaks it covers — a private key block becomes its placeholder and empty lines, a
secret-named block scalar its placeholder line and the block's other lines empty, grep's prefixes kept — so redacted
text has the stored text's lines and a line number written after redaction is the stored line's
(`redact.TestRedactionKeepsLines`). `none-1`, `secrets-5` and `secrets-4`, the policy before replacements kept line
breaks, are known by version; no other is. The version is recorded on each invocation.

**Barred accounts.** No model call is made for a case whose account is on the deployment's list of accounts barred
from AI processing: the main conversation's calls, every consult's, and every lookup. Where a list is configured, a
case that cannot be checked is refused too, until the engineer overrides it for that case after the deployment's
instructions; a list match cannot be overridden (`engine.TestBarredCaseRefusesEveryModelCall`,
`TestBarredRuleFailsClosed`, `TestListMatchCannotBeOverridden`). A refused call is recorded `barred_account` with the
reason and no account number; a call the override let through names it (`override`). Everything without a model —
comments, attachments, `compa case …` — still works.

**Egress class.** Each provider declares where its content travels: `local_only`, `internal_network` or
`external_provider`. The class selects the policy. The Vertex providers declare `external_provider`; the `openai`
provider declares the class the deployment gives its endpoint (`reasoning.openai.egressClass`), `external_provider`
where it gives none, so a deployment's own inference server may be declared nearer the host and redacted by that
class's policy, `local_only` redacting nothing. A call whose provider's class exceeds `reasoning.maxEgressClass`,
`local_only` where it is unset, is refused by the gate and recorded `egress_refused`; the gate answers before a
provider is made, so no credential is looked up for it (`engine.TestEgressRefusedBeforeTheProviderIsMade`,
`TestOpenAIEndpointUnderTheGate`).

## Providers

`model.Provider` carries one model call:

- the conversation as assembled, with its cache breaks, the tool declarations, the model and its effort, and
  cancellation;
- the answer as free text and tool calls, streamed as it arrives;
- the usage, normalized into the canonical categories (§Usage accounting), with its normalization mode;
- its egress class and the context limit of each model it serves.

A provider keeps nothing between calls but its own caches, makes one call per invocation and retries nothing. SDK-level
retries are disabled — the Anthropic SDK retries by default, and the genai SDK is set to one attempt explicitly; the
`openai` provider uses the standard library's HTTP client, which retries nothing — and no SDK feature that changes
what the model sees (automatic function calling, caching the engine did not request) is enabled. An SDK default that
cannot be disabled is a contract violation. The engine passes the model; a provider never chooses one. Neither Vertex
API reports a price, so cost is estimated (§Cost); token counts are exact. An OpenAI-compatible endpoint that reports
the call's cost, as OpenRouter does in US dollars, has it recorded as reported.

| Provider | Library | Notes | Usage normalization |
|---|---|---|---|
| `vertex-gemini` | `google.golang.org/genai`, Vertex backend, streamed | Effort sent as `thinkingLevel`; thought summaries requested (`ThinkingConfig.IncludeThoughts`); explicit cache (§Provider caching); a model's function calls sent back with their thought signatures; tool results sent as one content and the engineer's text after them as another, since Vertex refuses a content holding both ("Requests ending with a model turn are not supported", live) | `derived`: `promptTokenCount` includes `cachedContentTokenCount`, so input = prompt − cached; `candidatesTokenCount` is output; `thoughtsTokenCount` is reasoning |
| `vertex-anthropic` | `github.com/anthropics/anthropic-sdk-go`, `vertex` package, streamed | Tool use alone, never the structured-output feature; `cache_control` breakpoints; consecutive messages of one role sent as one | `native_disjoint`: input, cache read, cache creation and output are separate; reasoning `unavailable` |
| `openai` | none: Chat Completions over `net/http`, streamed as server-sent events | One OpenAI-compatible endpoint, `reasoning.openai`: its base URL (OpenRouter's by default), the environment variable holding its API key, sent as a bearer token (none sent where none is named), its egress class, and `cacheMarks`, which sends the engine's cache breaks as Anthropic-style `cache_control` marks on the system prompt and the content ending each of the last three breaks, as OpenRouter passes them to the models that take them, for an endpoint that accepts the field. Effort sent as `reasoning_effort`; `NoCalls` as `tool_choice: none`, after which a live call through OpenRouter to Claude still read its marked prefix from cache; a model message's calls sent with their arguments as JSON text, each result as a `tool` message; a streamed `reasoning` kept as the thinking | `derived`: `prompt_tokens` includes the cache reads and writes `prompt_tokens_details` reports, so input = prompt − cached − cache writes; `completion_tokens` includes `reasoning_tokens`, so output = completion − reasoning. With no prompt details, input and both cache categories are unavailable and the normalization `ambiguous`; with cache reads and no writes, writes are unavailable and any made are counted once, in input; reasoning not reported is unavailable |
| `fixture` | none | Answers from a function, for tests and offline runs. Selected in configuration as `provider: fixture`, it answers from the JSON script its `model` names: for each turn, chosen by the newest engineer or engine message containing the turn's `when`, its answers in order, text or tool calls (`internal/model/fixture/script.go`); how a deployment's binary is driven with no model, for the chat's end-to-end check. Egress class `local_only` | as given |

**Tool parameter schemas.** A provider refuses a whole request over one keyword it does not support, as Vertex
Gemini refused `maxItems` in a response schema. Each provider lists the keywords a live call with two tools accepted
(`vertexgemini.Keywords`, `vertexanthropic.Keywords`, `openai.Keywords`, held by each provider's `TestLiveTwoToolTurn`),
and the tools' schemas use no other, so a keyword new to a tool's parameters is probed live before it is added. The engine
validates a call's parameters against the full schema before it runs.

**No calls.** When the engine wants an answer rather than more calls, the request says so (`NoCalls`). Gemini switches
function calling off only on a call that reads no cache, since Vertex refuses tool configuration beside a cache;
Claude never changes its tool choice, since that invalidates its cached messages. Either way the engine's own
instruction and its refusal of any call made anyway carry it.

**Thought summaries.** The Gemini provider records the model's thought summary, `thinking`, where the response carries
one, and the `openai` provider the `reasoning` an endpoint streams; Claude on Vertex returns them only with extended
thinking, which is not enabled, so a Claude call records none. It is kept
only where the raw response is, redacted on storage by the same policy, shown in `/debug` labelled a model's thinking,
and never enters a conversation.

Both Vertex providers authenticate with Google Application Default Credentials and take project and region from
configuration, falling back to the environment. Their egress class is `external_provider`. The derived Gemini
arithmetic is documented in the provider and covered by a test. Library versions and the exact field mappings are
re-verified when a pinned version changes.

Later providers: the Anthropic Messages API directly.

## Models and effort

A model is named in configuration (`reasoning.models`), with its provider, its ID there and the efforts it takes. The
profile class `companion` names the model and effort every conversation and consult uses, the class `ledger` those
of the case ledger's calls, the class `editor` those of the editor's and the class `reviewer` those of the reviewer's
(`config.TestProfile`). The engine ships no default model: a deployment's configuration names the models and each
class's profile, and a class with no profile is not run (`docs/TECH_STACK.md`).
Experts never name a provider, a model ID or a profile; the expert loader rejects manifests that do.

An **effort** is `minimal`, `low`, `medium` or `high`, one the model lists, as checked live, since models differ:
`gemini-3.8-flash` refuses `minimal`. There is no default: a profile on a model that takes efforts must name one, and
one on a model that takes none names none; either fails when configuration loads otherwise (`config.TestEffortChecked`).
The Gemini provider sends it as its `thinkingLevel` (`vertexgemini.TestThinkingLevelSent`). "Thinking" names only a
model's thought summary.

`/model [model] [effort]` chooses another model and effort for the session's conversation, checked against the
configured models and their efforts, held in memory alone, and recorded on each call as the engineer's choice, the
profile's source `engineer`. The ledger keeps its own profile.

## The invocation record

One record per model call, successful or not, in `invocations` (`docs/ARCHITECTURE.md`).

- **Identity:** invocation ID, case or chat, conversation ID, the consult it belongs to if any, the turn and the call's
  number within it; for a resent call, the call it resent (`resent_from`) and no conversation.
- **Model:** provider, model ID, the effort sent, and the profile it was resolved from with where that choice came
  from (`config` or `engineer`).
- **Egress:** redaction policy version; `override` where the barred-accounts override let the call go.
- **Usage and cost:** §Usage accounting and §Cost.
- **Cache:** where the provider makes caches of its own, as the Gemini provider does, what it did for the call: made
  one, reused one, a make refused with the provider's error, or none and why, with the messages the cache read holds
  and their estimated tokens (§Provider caching).
- **Timing:** started, first token, completed, wall-clock duration, measured by the engine.
- **Outcome:** `answered`, `called_tools`, `egress_refused`, `barred_account`, `cancelled`, `transport_error`,
  `output_loop`, `budget_exceeded`.
  - `called_tools`: the response holds tool calls, recorded with their names and the evidence each result became.
  - `output_loop`: an answer stopped at its output cap whose tail repeats one fragment, at least 8 times over at least
    256 bytes, the fragment up to 1,024 bytes, on the last 64 KiB of the text. An answer stopped at the cap without
    such a tail is `budget_exceeded`. Its problems give the fragment's length and repeats, never the fragment.
- **Payloads:** the request as sent and the raw response, kept for debugging, with the thought summary beside the
  response; redacted on storage by the same policy as on egress.

**Never fabricate a metric a provider does not report.** `unavailable` is a correct and expected value.

## Usage accounting

The purpose is to see where tokens are consumed, compare models, and find model usage that is not earning its cost.
This is not an accounting subsystem: usage is a field group on the invocation record, and aggregation is a query over
those records. There is no separate store, write path, or telemetry pipeline.

### Canonical categories

Categories are **disjoint by definition**, so no token is counted twice. Every field is a count or `unavailable`. Zero
means measured as zero; it never means unknown.

| Field | Definition |
|---|---|
| `input_tokens` | Prompt tokens processed, excluding cache reads and cache writes |
| `cache_read_tokens` | Prompt tokens served from cache |
| `cache_write_tokens` | Prompt tokens written to cache |
| `output_tokens` | Generated tokens, excluding separately reported reasoning tokens |
| `reasoning_tokens` | Reasoning or thinking tokens, where the provider reports them apart from output |

No total is stored. Token totals are derived at aggregation from the five token categories. A record is one model
call, so an aggregate's count of invocations is its count of calls.

### Normalization

Providers disagree about composition: some report input excluding cache reads and some including, some fold reasoning
tokens into output and some report them separately. The provider normalizes into the categories above and declares
which of three things it did:

- `native_disjoint` — the underlying categories already match; values pass through unchanged.
- `derived` — the provider subtracted to make them disjoint, with the arithmetic documented in the provider and
  covered by a test.
- `ambiguous` — composition cannot be determined from what the API reports. The affected categories are
  `unavailable`.

A value belongs to exactly one category. A provider that cannot separate reasoning from output records `output_tokens`
as reported and `reasoning_tokens` as `unavailable`; it does not split them by estimate. Subtracting on an assumption
about a provider's composition is fabrication, not normalization.

### Scope

Usage is per invocation: every invocation is one model call. A provider that reports usage only for a whole session is
not supported, and a session total is never divided across invocations or presented as one invocation's.

### Fidelity and duration

Each recorded value carries a measurement fidelity: `reported`, or `estimated` with the estimator version. An estimated
value is never compared against a reported one without stating the mismatch, and never aggregated into the same figure
as reported values. The estimate of a call's input before it is sent, for the compaction threshold, is labelled
(`words1.3+symbols-v1`) and never recorded as usage.

Wall-clock duration is measured by the engine, so it is available even when every token count is not.

Provider-internal retries are disabled (§Providers) and flagged where they occur.

### Aggregation

A read-only query over invocation records, grouped by case, conversation, consult and model. It needs no separate
persistence.

Every aggregate carries a completeness marker: how many invocations contributed, and how many had an unavailable value
for each summed category. A sum over partially unknown inputs is presented as a floor with the gap named, never as a
total. An aggregate that mixes models states so, since efficiency comparisons across them are the point.

### Exclusion from context

Usage records are engine-side only. No model is told its own consumption or any prior call's. Measuring token use must
not itself consume tokens or change model behavior.

### Cost

compa records the cost a provider reports. Where a provider reports none, the engine estimates one from a price
table it holds: list prices per million tokens for each model it can select, with the source, retrieval date and a
version. The bundled table is Vertex's global-endpoint standard tier, version `vertex-global-2026-10-31`, retrieved
2026-10-31 (`internal/model/pricing`).
It is maintained by hand and will lag.

An estimate is made only from reported token counts at per-invocation scope, for a model in the table, at the rates in
effect on the call's date and the model's long-context rates above 200,000 prompt tokens. Reasoning tokens are priced
at the output rate. If a category the model bills is unavailable, or a category it does not bill carries tokens,
nothing is estimated and cost stays `unavailable`. Batch and priority tiers, discounts and Claude's one-hour
cache-write rate are not modelled. Gemini's explicit cache storage is in the table, per million tokens held per hour
(`pricing.TestCacheStorage`), but no call's estimate includes it: no invocation reports it, and the engine does not
record its caches' lifetimes.

Alongside the token categories:

| Field | Definition |
|---|---|
| `cost_amount` | The provider's own figure for this invocation, or the engine's estimate |
| `cost_currency` | Explicit: from the provider's report or, where the report omits it, declared by the provider from its documentation. Never inferred by the engine |
| `cost_fidelity` | `reported`, `estimated` (from the engine's price table, with `cost_estimator` naming the table's version) or `unavailable` |
| `cost_estimator` | The price table version an estimated cost came from |
| `cost_by_category` | Per-category breakdown where the provider supplies one, following the same disjoint categories |

Mixed priced and unpriced usage is the normal case. **Never impute.** A rate observed on priced invocations is not
applied to unpriced ones; that silently converts a measurement into an estimate.

Aggregates therefore carry two token figures and one cost:

```text
82,000 tokens ($0.41), 15,000 tokens (unpriced)
```

Priced tokens are those from invocations with a known cost, and their costs sum; estimated and reported costs are
summed and shown separately, never as one figure. Unpriced tokens are a separate count with no money attached. When
everything is priced the second clause is omitted; when nothing is, only tokens are shown. Mixed currencies are
reported per currency rather than converted.

Fidelity here is deliberately loose. The purpose is trend visibility and a rough sense of spend, not billing. The
engine's estimates are labelled `estimated` with the table's version and are never aggregated into the same figure as
reported costs.

### Trajectory

`compa case trajectory <case> [--json]` says what each turn of the case's main conversation and its compaction
ancestors did, oldest first, read from the store with no model call and nothing written: the model's tool calls, how
many were refused, whether the turn's budget was spent, and the calls repeating an earlier call's tool and the
arguments it ran with, its question aside; each call's question (§Tools) as written, and per turn and in all the
calls to a tool that takes one, those with none, those whose question repeats one an earlier response asked, in any
turn, compared with case, spacing and punctuation aside, the vague: under four words, or every word a function
word or a generic one such as file, log, error or output; and those restating the action: no question mark, or
opening with an imperative such as find, read, check, search or list; each reported, never corrected
(`engine.TestTrajectoryQuestions`, `TestRestatesAction`); each call's answers (§Tools) with what checking them
flagged, and how many were given and flagged (`engine.TestTrajectoryAnswers`);
each evidence item a tool returned, counted once in the turn that first returned it, as read on with
`evidence_read` by its name or ID, cited in a later answer, both, or neither, with the names of those neither: cited
by its name, or by the path of a file it read where it read that file alone or its result names it, so a search is
not credited with every file it opened (`engine.TestTrajectoryCitedByPath`); beside these, how many a later call
answered, its spans' flags aside, and how many, named, had no answer, citation or read-on, which the text lists in
place of those neither, the JSON keeping both; the experts loaded, the
engine's preload marked, and each consult with its own call count and how many of its calls repeat one the main
conversation made (`engine.TestTrajectoryCountsTheOpening`). Its token figures are each model call's input and
cache-read counts as its record holds them, never invented: a sum over calls with no count is a floor naming how many
were unavailable. Beside them each call's cache outcome as its record holds it: made, reused, a make refused with the
first line of the provider's error, under the minimum, no break, or none recorded (`engine.TestTrajectoryCacheOutcomes`).
Each turn also gives the expert guidance its conversation held — each `load_expert` result and the
preload, by expert ID and version, with its tokens by the engine's estimator, labelled with the estimator's version —
what its model calls in that conversation sent of it, each counting what was loaded before it, and that as a share of
those calls' whole input as the provider reported it, uncached, cache read and cache write where the provider has it;
the share is `unavailable` where any of those calls' input was not reported, never a share of the rest. A consult's
guidance, its ask's and any it loads, is given apart, and a conversation a compaction starts holds none until it loads
it again (`engine.TestTrajectoryGuidance`, `TestTrajectoryGuidanceUnreported`, `TestTrajectoryGuidanceAfterCompaction`).
The editor's kept check of a turn's answer gives its anchors, whether it was revised, and the citations with lines and
of whole items the revision added (`engine.TestTrajectoryKeepsTheCheck`). A case with no
conversation has a trajectory with no turns (`engine.TestTrajectoryOfNoConversation`,
`cli_test.TestCaseTrajectory`). A turn's kind is read from the engine's ask that began it. Each check's model
calls are given with their outcome and time as recorded, one a line beneath the check — the first, a retry after an
answer in text or a refusal, the editor's follow-up on statements left unaccounted, and the revision for the
anchors — and each review the engineer asked for, by what it reviewed, its calls likewise; the totals count the calls
of each kind and the longest (`engine.TestTrajectoryCheckCalls`, `TestTrajectoryReviews`). A retry gives why it was
asked (`reason`), read from the check's conversation, so a store kept before needs nothing more: the refusal its
previous call was sent back, the findings schema's problems of findings refused whole or the loop's word on a call
it did not run, such as a tool there is none of, or that the previous call answered in text; the text cuts it to a
line. Elements validation drops never cause a retry: findings accepted end the check
(`engine.TestTrajectoryRetryReasons`, `TestTrajectoryReviewRetryReasons`).

## Contract cases

Each case is tagged with the group it belongs to: **[C2]** providers, **[C3]** the loop, **[C5]** the chat,
**[C8]** the case ledger, **[C10]** the conversation's start, **[C11]** reopening, **[E1]** to **[E4]** the answer
checks.

- **[C2]** Each provider client is built with SDK retries disabled and no automatic function calling; a fixture
  round trip returns free text and tool calls, streamed.
- **[C2]** Each provider's usage mapping holds against recorded responses, the derived Gemini arithmetic included; an
  `ambiguous` normalization leaves the affected categories `unavailable`.
- **[C2]** The Gemini explicit cache is reused for the longest live matching prefix and regrown past a quarter, ends
  short of results that end its break, and each call's outcome says what was made, read or refused
  (`vertexgemini.TestExplicitCacheGrows`, `TestExplicitCacheSkipped`, `TestExplicitCacheInToolLoop`); Claude's
  breakpoints are placed on the system prompt and the last three breaks (`vertexanthropic.TestCacheBreakpoints`).
- **[C2]** Each provider streams free text and tool calls, sends results back with what the provider requires, retries
  nothing, and reports an answer cut at its output budget or stopped otherwise as an error
  (`vertexgemini.TestStreamedAnswer`, `TestNoRetry`, `TestStopReasons`, and the same in `vertexanthropic`;
  `openai.TestStreamedAnswer`, `TestStopReasons`; `model.TestFixtureRoundTrip`).
- **[C2]** The `openai` provider's usage is made disjoint from the prompt and completion details an endpoint reports,
  unreported categories unavailable, never zero, and a reported cost kept with its currency
  (`openai.TestStreamedAnswer`, `TestUsageUnreported`); its cache marks fall on the system prompt and the last three
  breaks, and none is sent without them (`openai.TestCacheMarks`); it declares the egress class configured for its
  endpoint (`openai.TestDescribe`, `config.TestOpenAI`).
- **[C2]** A two-tool turn runs live on each provider, the second call reading from cache (`TestLiveTwoToolTurn` in
  each provider, opt-in, `openai`'s through OpenRouter with cache marks); a turn shaped as the opening grows its
  Gemini cache within the tool loop, no make refused (`vertexgemini.TestLiveOpeningCache`, opt-in).
- **[C2]** Effort is checked at configuration load and sent as Gemini's thinking level (`config.TestEffortChecked`,
  `vertexgemini.TestThinkingLevelSent`).
- **[C2]** Each provider's tool parameter schemas use only keywords a live call accepted (`vertexgemini.Keywords`,
  `vertexanthropic.Keywords`, `openai.Keywords`; the tool table's schemas are held to them by
  `engine.TestCompanionToolKeywords`).
- **[C2]** Gemini's cache storage is priced in the table and in no call's estimate (`pricing.TestCacheStorage`).
- **[C3]** Redaction by the egress class's policy applies to every message and tool result before a call, and a call
  still holding what its policy forbids is not sent (`companion.TestRedactedBeforeEachCall`; the policies:
  `redact.TestPolicies`, `redact.TestEachSecretIsRedacted`, `TestBlockValueEnds`, `redact.TestNonSecretsAreKept`,
  `redact.TestRedactionKeepsLines`).
- **[C3]** A barred or unchecked case makes no model call, the companion's included, and a list match cannot be
  overridden (`engine.TestBarredCaseRefusesEveryModelCall`, `TestBarredRuleFailsClosed`,
  `TestListMatchCannotBeOverridden`); a gate's refusal is recorded as `barred_account` or `egress_refused` and sends
  nothing (`companion.TestGateRefuses`). A consult's calls pass the same gate, which runs before every call.
- **[C3]** Two consecutive calls in a conversation share a byte-identical prefix — system prompt, tools and every
  earlier message — with the primary expert of the case's routing key preloaded; a later comment is appended as a case update and
  leaves the block unchanged (`engine.TestCompanionConversation`, `TestCompanionCaseUpdate`).
- **[C3]** A configuration's house text is in the system prompt once, as its last section, the same bytes on each
  call and in no message, so it lies before the first cache break; with none, or white space alone, the prompt has no
  section without a body, and configuration refuses white space alone (`engine.TestHouseSectionInTheCachedPrefix`,
  `companion.TestSystemHouseSection`, `config.TestHouse`); no prompt file the engine embeds names an organization
  (`engine.TestPromptsNameNoOrganization`).
- **[C3]** A tool call naming no tool, with invalid parameters, or with a command the allowlist refuses is not run,
  stores nothing, and its result is the refusal with its reason (`engine.TestCompanionRefusesCalls`).
- **[C3]** A collection's result is stored as evidence and returned under its short name, which resolves to the
  stored item (`engine.TestCompanionConversation`); every tool's parameter schema uses only keywords both providers
  accepted live (`engine.TestCompanionToolKeywords`).
- **[C3]** Every tool returning evidence declares a required question, `load_expert` and `describe_tool` none, and
  `consult` only its own; the engine takes it out before the call is validated and run, the stored call keeping
  it as written, a call without one run (`engine.TestToolsDeclareTheQuestion`, `TestQuestionTakenOutBeforeTheCallRuns`);
  the trajectory reports each, the calls with none, repeated, vague or restating the action, and compares calls by
  what ran (`engine.TestTrajectoryQuestions`, `TestRestatesAction`); the chat's tool line gives each call's own, in the
  order made, a row for each call where the batch is wider than the terminal, each cut to the width, live and
  reopened, and `/debug` whole (`chat.TestToolLineShowsTheQuestion`, `TestBatchedToolLineGivesEachCallItsQuestion`).
- **[C3]** Any tool declares optional answers, taken out before the call runs and kept in the stored call
  (`engine.TestAnswersDeclaredAndTakenOut`); an answer naming no item a tool returned before it, or no item's name, is
  flagged, and its code spans are searched for in that item's lines shown, flagged where none holds them, arithmetic
  evaluated; the trajectory gives each answer and its flags and counts the items answered beside those with no answer,
  citation or read-on (`engine.TestTrajectoryAnswers`); `/debug` gives each whole with its flags
  (`chat.TestDebugShowsTheAnswers`). An excerpt of a call that asked a question gives it back under its head with the
  ask for what the item settled, its head still naming the item and the line no line shown; a call without one, a
  result that is no excerpt and a refusal are unchanged (`engine.TestExcerptAsksForTheAnswer`).
- **[C3]** The soft budget refuses calls past it, asks for an answer, and stops a model that keeps calling; the next
  turn has a budget of its own (`companion.TestBudget`, `TestBudgetIgnored`).
- **[C3]** A consult runs in a conversation of its own under the main one, beginning with the same case block and the
  case updates given since, and returns its answer as the tool's result with its transcript kept
  (`engine.TestCompanionConsult`, `TestConsultGetsCaseUpdates`).
- **[C3]** `/compact` starts a linked conversation from the case block as it is now and the handoff, the old one kept;
  above the context limit a turn compacts on its own and says so, a conversation with no answer never
  (`companion.TestCompact`, `TestContextLimitCompacts`).
- **[C3]** An answer stopped at its output cap is `budget_exceeded`, or `output_loop` where its tail repeats one
  fragment (`companion.TestOutputLimit`).
- **[C3]** Each model call records its own usage with conversation and turn (`engine.TestCompanionConversation`);
  unknown values persist as `unavailable`, never zero, and a measured zero survives a round trip as zero
  (`model.TestCountRoundTrip`, `invocations.TestUnknownStaysUnknown`).
- **[C3]** Aggregation carries a completeness marker; estimated and reported values are never combined into one
  figure; priced and unpriced tokens are reported apart with no rate imputed; mixed currencies are reported per
  currency (`invocations.TestAggregateCompleteness`, `TestAggregateCost`, `TestAggregateGrouping`).
- **[C3]** A turn of the companion runs live on each Vertex provider over the synthetic case, the preloaded expert's
  call and the tools' results accepted (`engine.TestLiveCompanionTurn`, opt-in).
- **[C3]** No model call carries usage or cost: of the invocation records, the loop reads only the request a resent
  call sends again, which holds neither.
- **[C3]** A stored call resent on its model sends its request as it was sent, is recorded as a call of its own of
  task `calls.resend` naming the original and counted in the case's usage, and changes nothing else in the case's
  store, the conversations, their messages and what they were shown included (`engine.TestResendRecordsACallOfItsOwn`);
  it passes the gate, refused and recorded `barred_account` or `egress_refused` with nothing sent and no provider
  made (`engine.TestResendRefusedOnABarredCase`, `TestResendRefusedPastTheEgressClass`); resent to another
  configured model, it is sent at that model and effort, without the provider data the original's model wrote, and
  redacted by the target provider's policy (`engine.TestResendToAnotherModel`).
- **[C5]** `/compact` carries the pinned items into the new conversation (`engine.TestCompanionCompactCarriesItems`).
- **[C5]** A citation to unknown evidence, to evidence the conversation was never shown, an unknown path or lines past
  the end is flagged with its reason (`engine.TestCheckCites`), a citation of an item never shown staying flagged
  once the answer holding it is stored (`engine.TestCiteOfUnshownItemStaysFlagged`), one of an item the case block
  only names flagged until read (`engine.TestCiteOfNamedOnlyItemFlagged`), and one of lines outside those the
  conversation's requests carried flagged (`engine.TestCiteOutsideAPartialReadFlagged`); a consult's read and a
  compaction's ancestors count, a checker's never (`engine.TestCiteOfAConsultsReadSaysSo`,
  `TestCompactionCarriesExposure`, `TestCheckersReadShowsTheAnswerNothing`), and a conversation stored before the
  record says its exposure is unknown (`engine.TestUnrecordedConversationExposureUnknown`); the chat renders a
  flagged citation flagged, never dropped (`chat.TestCitationsFlagged`), and notes beneath the answer, in one line,
  those resolved on a consult's read or by names (`chat.TestCitationOfAConsultsReadNoted`); a case-ledger item cited,
  alone or beside evidence names, is flagged as a model's reading of its comment, the evidence names beside it
  checked (`engine.TestLedgerItemCitedFlagged`, `chat.TestLedgerItemCitedFlagged`).
- **[C5]** `/model` switches model and effort for the session, records the engineer's choice on each call, and keeps
  the conversation (`chat.TestModelSwitch`).
- **[C5]** Reopening a case or named troubleshooting chat restores its visible conversation, citation navigation
  and original answers without model calls when the opening turn is skipped (`chat.TestReopenRestoresConversation`,
  `TestReopenChatRestoresConversation`, `TestReopenRestoresEvidenceNavigation`); main history follows compaction
  ancestry without consulting or modifying a conversation (`engine.TestConversationHistoryAcrossCompactions`,
  `TestConversationHistoryExcludesConsultMessages`, `TestConversationHistoryEmptyIsReadOnly`).
- **[C5]** Long streamed answers enter native scrollback once, without committing transient input/status rows;
  the inline view and each insertion fit the physical terminal, and main-chat wheel events do not move the input
  (`chat.TestLongStreamCommitsOnceInBoundedRows`, `TestInlineViewReservesInsertionRow`,
  `TestMainChatLeavesWheelToTerminal`).
- **[C5]** The status bar's tokens, cost and cache reads are the case's or chat's stored model calls, consults,
  compacted conversations and the case ledger's calls included: they survive leaving and reopening, a later call adds
  itself once, and an unavailable count is never shown as zero or turned into a cache share
  (`chat.TestUsageSurvivesReopenAndCompaction`, `TestUsageSnapshotsNeverRegress`,
  `TestIncompletePromptCountsDoNotInventCacheShare`, `invocations.TestTotalTaskUsage`,
  `engine.TestLedgerCountsInTotalUsage`).
- **[C5]** While a turn runs, the bar above the input animates and says how to stop it, and stops when the turn ends;
  `/` lists the commands without calling a model or printing into the scrollback, and clears every row it drew
  (`chat.TestActivityShowsCancellationAndStopsWhenIdle`, `TestStatusBarAboveEntry`,
  `TestCompletionDoesNotInvokeModel`, `TestCompletionEnterWaitsForArguments`, `TestPopoverClearsItsWholeRenderArea`).
- **[C8]** The ledger reads only the comments it has not, oldest first, in a conversation of its own with its own
  prompt and one tool, recorded as `comments.ledger`; numbers its items in the thread's order; settles an ask a later
  comment answers; marks what was provided `superseded` by a newer capture, its attachment kept; and makes no call
  with nothing new (`engine.TestLedgerItemsAndSettling`, `companion.TestTaskEndsOnItsToolsResult`).
- **[C8]** A comment saved unchanged is not read again; an edit is, and withdraws the items it no longer says
  (`engine.TestLedgerWithdrawsEditedItems`).
- **[C8]** Every comment read is accounted for: by items, a change, none with why, or `unaccounted`
  (`engine.TestLedgerAccountsForEveryComment`); a comment too long to show is given to no call and marked not shown,
  in the engineer's view and the conversation's ledger (`engine.TestLedgerMarksTooLongCommentNotShown`).
- **[C8]** Items are numbered in the transaction that keeps them, after any added meanwhile
  (`engine.TestLedgerNumbersAfterAnItemAddedMeanwhile`).
- **[C8]** An update's elements are validated against the call's comments and the ledger, the failing ones dropped
  with their reason; arguments the schema refuses are refused whole, and the model gets one retry, a text answer one
  ask to call the tool (`ledger.TestValidate`, `TestValidateRefusesWhole`, `engine.TestLedgerRetriesOnce`);
  `ledger_update`'s parameters use only keywords both providers accepted live and offer the schema's values
  (`ledger.TestDeclMatchesSchema`).
- **[C8]** A barred or unchecked case makes no ledger call, the refusal recorded as `barred_account`, and reads nothing
  (`engine.TestLedgerBarredMakesNoCall`, `TestBarredCaseRefusesEveryModelCall`).
- **[C8]** Items from a comment the engineer leaves out of what models read are left out of the ledger shown, its
  rendering and its next calls (`engine.TestLedgerLeavesOutExcludedComments`); the rendering is bounded, the oldest
  left out first and counted (`engine.TestRenderLedgerBounded`).
- **[C8]** `/ledger` reads what is unread, its call counted in the status bar's total, and shows the ledger; with
  nothing unread it calls no model (`chat.TestLedgerCommand`).
- **[C10]** The case block holds the newest five comments in full and each older one on a line of its name, date and
  author, exclusions applied; it is the same bytes made twice, and a later turn has no case update for what it named
  (`engine.TestCaseBlockNamesOlderComments`); with an artifact inventory, its text replaces the attachment overview,
  identities by short name (`engine.TestCaseBlockShowsTheInventory`).
- **[C10]** Preparation reads the ledger beside the rest, saying its progress, and the opening waits for it: the
  `# Case ledger` message follows the case block and the preload before the opening's first call; a later run's new
  items and changed statuses are appended as a `# Case ledger update` at the next turn's start, after the case update,
  everything earlier unchanged, and a turn with nothing new is given none (`engine.TestLedgerInTheConversation`).
  Past the wait the opening orients without it, telling the model, and the ledger reaches the next turn
  (`engine.TestOpeningGoesOnWithoutASlowLedger`); cancelling preparation cancels the ledger
  (`engine.TestPrepareCancelsTheLedger`); a compacted conversation's block is followed by the whole ledger
  (`engine.TestCompactionGivesTheLedger`, `companion.TestCompact`).
- **[C10]** The opening turn stops at its 8 tool calls (`engine.TestOpeningBudget`); a turn stops at its token budget,
  the next with one of its own (`companion.TestTokenBudget`), cache reads counted at a tenth
  (`companion.TestTokenBudgetCountsCacheReadsAtATenth`).
- **[C10]** Preparing without a model call, as `--no-open` and a reopened case do, calls none, the ledger's included
  (`engine.TestPrepareWithoutCall`, `chat.TestNoOpen`, `chat.TestReopenMakesNoCall`); a barred case starts no ledger
  and its opening is refused, the provider never called (`engine.TestPrepareBarredMakesNoCall`); `compa ask` prepares the
  case as the opening does (`cli_test.TestAskPreparesTheCase`).
- **[C10]** A reopened conversation's state says what it has yet to be shown, and that the provider's cache expired
  only past its lifetime — Gemini's configured or default, Claude's five minutes, none for the fixture — and only for
  a conversation past its size (`engine.TestReopenState`, `chat.TestReopenLines`).
- **[C11]** The ranked evidence orders lines pointed at above a pinned item's citation, above an answer's, above a
  tool's excerpt, decays with the turns since, leaves out the case block's items and follows compaction, not consults;
  reading it calls no model and changes nothing in the store (`engine.TestRankedEvidenceWeights`,
  `TestRankedEvidenceDecays`, `TestRankedEvidenceLeavesOutCaseBlock`, `TestRankedEvidenceMergesRanges`,
  `TestRankedEvidenceLineage`).
- **[C11]** A suggested reload calls no model and starts a linked main conversation holding the case block as it is
  now, the whole ledger, the pinned items, the ranked items' lines with their numbers and the last exchange, the same
  bytes for the same inputs; the next turn continues in it, and the history keeps the earlier answers
  (`engine.TestReloadSuggested`); what is carried is bounded (`engine.TestReloadCarriesBounded`). A summary reload
  makes exactly one call, the handoff (`engine.TestReloadSummary`); a full reload changes nothing and calls none
  (`engine.TestReloadFull`).
- **[C11]** A catch-up with nothing new calls no model; with a new comment the ledger reads it first, then one turn
  begins with the case update and the ledger, the engine naming the comment (`engine.TestCatchUp`); a conversation a
  reload began is judged by the one it continues until its first turn (`engine.TestCatchUpAfterReload`).
- **[C11]** The reopened case's choices show in a panel above the status bar whose rows never enter the scrollback and
  whose area is cleared when it closes; nothing calls a model until a choice, its keys choose, and esc puts each off;
  after a suggested reload the history still shows the earlier answers (`chat.TestReopenChoicePanel`,
  `TestReopenChoicePanelLater`, `TestReopenChoiceSummary`); `/reload`, `/reload full`, `/ranked` and `/catchup` with
  nothing new call no model (`chat.TestReloadCommands`).
- **[E1]** An answer shows a citation in a form the chat does not read, and an inline code span in no evidence the
  conversation saw, once each, after its citation flags, live and reopened, and nothing for a clean answer
  (`chat.TestAnswerFlagsShown`); look-alikes — a MAC or IPv6 address, a link's text, `(e1000)`, a span holding
  citation-like text — are not flagged (`engine.TestCheckMalformedCitation`); a span is flagged when found in no item
  the conversation saw, the engineer's left-out comments not counted (`engine.TestCheckSpanNoted`,
  `TestCheckSpanLeftOut`); the search's bounds are said and a cancelled search ends (`engine.TestCheckSpanBudget`).
- **[E1]** Statements split as given (`check.TestSplit`, `TestStatementParts`); a statement resting only on a
  listing, one with a specific and no citation, and one that dropped an earlier hedge are the editor's candidates
  (`engine.TestCheckListingIsLabelled`, `TestCheckHedgeDropped`), over the synthetic replay
  (`testdata/replay/`).
- **[E1]** Quoted lines are the stored lines byte for byte, bounded for a citation and a request with each cut said
  (`engine.TestQuoteByteForByte`, `TestQuoteBounds`, `TestQuoteLongLastLine`); the editor's and reviewer's requests
  stay within their bounds and are the same bytes for the same inputs (`engine.TestCheckRequestBound`,
  `TestCheckRequestsDeterministic`, `TestReviewRequest`, `TestCheckNothingYet`).
- **[E1]** `check_findings` is refused whole against its schema, its elements checked against the request and
  dropped with their reasons, a statement accounted for once (`check.TestValidate`, `TestValidateRefusesWhole`,
  `TestValidateDuplicatesAndEmptyQuotes`); its declaration uses only keywords both providers accepted and matches the
  schema (`check.TestDeclMatchesSchema`).
- **[E1]** A draft for the customer is asked with the certainty review, an internal note without it
  (`engine.TestCustomerDraftCarriesCertaintyReview`).
- **[E2]** The findings schema's roles hold: an anchor and a correction are the editor's, an alternative and a
  discriminator the reviewer's, an anchor says what fulfils it (`check.TestValidateRoles`); the certainty candidate
  takes own-voice certainty and causal wording, not a named source's (`check.TestCertaintyTerms`,
  `engine.TestCheckCertaintyCandidate`); the editor's prompt holds `customer.md` whole
  (`check.TestEditorPromptCarriesTheRules`).
- **[E2]** The editor corrects wording with no call of the main model, the answer as written kept byte for byte and
  the correction read at the next call (`engine.TestEditorCorrectsWording`); anchors bring exactly one revision call
  with no tools (`engine.TestEditorAnchorRevises`, `companion.TestAskOneCall`); no second round, a still-failing
  revision shown flagged (`engine.TestEditorNoSecondRound`); a clean answer is printed as written
  (`engine.TestEditorCleanAnswer`); the editor makes at most two calls in all (`engine.TestEditorTwoCallsInAll`).
- **[E2]** A failing or slow editor leaves the answer as written, said unchecked (`engine.TestEditorFailsOpen`,
  `chat.TestEditorUncheckedShown`); a revision not taken records what was shown, which reopening, `/pin` and the next
  call read (`engine.TestEditorRevisionNotTakenIsKept`, `chat.TestEditorRevisionNotTakenShown`); a cancelled check or
  revision keeps its state and numbering (`engine.TestEditorCancelled`, `chat.TestEditorCancelledShown`).
- **[E2]** The editor's calls are refused for a barred case, redacted, never quote a left-out comment, and count in the
  case's usage (`engine.TestEditorBarredMakesNoCall`, `TestEditorRequestRedacted`, `TestEditorLeavesOutExcludedComment`,
  `TestEditorCountsInTotalUsage`); with no `editor` profile there is none (`engine.TestEditorNotConfigured`).
- **[E2]** A draft keeps the corrected text (`engine.TestEditorDraftKeepsCorrection`, `chat.TestEditorDraftCorrected`);
  `compa ask` prints the check (`cli_test.TestAskPrintsTheCheck`); a reload's last exchange and the ranked evidence read
  the answer as shown (`engine.TestEditorCorrectionInReloadAndRanking`); the chat shows the answer being checked,
  replaces it before printing, and says the correction or revision beneath it, live and reopened, the turns tab
  listing each step (`chat.TestEditorCorrectionShown`, `TestEditorAnchorShown`).
- **[E3]** The reviewer reviews the last answer as shown or an item, in a conversation of its own, its review
  appended once to the main conversation, earlier messages byte for byte, read at the main model's next call and on
  reopening, nothing reviewed rewritten and the review no answer (`engine.TestReviewLastAnswer`, `TestReviewItem`);
  its findings keep the alternative and discriminator, an anchor or correction dropped (`engine.TestReviewKeepsItsFields`);
  its prompt holds `customer.md` (`check.TestReviewerPromptCarriesTheRules`); at most two calls in all
  (`engine.TestReviewTwoCallsInAll`); a failure is said and nothing kept (`engine.TestReviewFailsAppendsNothing`,
  `chat.TestReviewFailureShown`).
- **[E3]** The reviewer's calls are refused for a barred case, redacted, never quote a left-out comment, and count in
  the case's usage (`engine.TestReviewBarredMakesNoCall`, `TestReviewRequestRedacted`,
  `TestReviewLeavesOutExcludedComment`, `TestReviewCountsInTotalUsage`); with no `reviewer` profile there is none and
  `/review` says so (`engine.TestReviewNotConfigured`, `chat.TestReviewNotConfigured`); migration 42 keeps every row
  (`store.TestMigration42KeepsRows`).
- **[E3]** After a draft the chat offers the review on `r`, nothing called until it is taken, `r` the first key on the
  empty input, any other key or a paste ending the offer (`chat.TestReviewOfferedAfterDraft`,
  `TestReviewOfferEndsOnOtherInput`); `/review` and `/review iN` print the review beneath, listed in the turns tab and
  reopened the same (`chat.TestReviewCommand`), refused while a turn runs (`chat.TestReviewRefusedWhileTurnRuns`).
- **[E3]** `/review --ranked` gives the reviewer, after the items the text cites, the ranked evidence it does not cite,
  each item headed as disclosed to the companion and not cited, its lines in the request's context, a text citing all
  told there is none, and plain `/review` none of it (`engine.TestReviewRequestRanked`, `chat.TestReviewRanked`); what
  it quotes is the reviewer's read, never the conversation's (`engine.TestRankedReviewShowsTheAnswerNothing`).
- **[E4]** Measured on the live benchmark and held without a model: the reviewer's time bound fits the slowest review
  measured (`engine.TestReviewerBoundHoldsMeasured`); a finding's reasons up to the lengths measured are taken, not the
  whole call refused (`check.TestValidateTakesMeasuredLengths`); a listing finding on a statement resting only on
  listings is kept, one on a statement citing other evidence dropped (`engine.TestCheckListingFindingRestsOnStatement`,
  `check.TestValidateListingOnlyStatement`); the footer counts the statements the correction changed
  (`engine.TestEditorCountsChangedStatements`); a check's conversation stores no note it never sent
  (`engine.TestCheckStoresNoBudgetNote`).
- **[E4]** Found by a live run of a deployment's binary on the synthetic case and held without a model: a whole-item
  citation is quoted and said valid, never anchored for lacking lines (`engine.TestEditorWholeItemCitation`); the
  revision is told it reads no evidence and a citation with lines it added is flagged and counted, live and reopened
  (`engine.TestEditorRevisionAddedCitation`), unless every line it names was shown the conversation numbered before
  the answer, as the record of what each message showed holds it — a tool's excerpt, an `evidence_read`'s lines, an
  `evidence_grep`'s matches and their context, the lines a reload carries and a verdict quotes — the case block and
  case updates giving items unnumbered (`engine.TestEditorRevisionCitesShownLines`,
  `TestEditorRevisionCitesCarriedLines`), a conversation stored before the record read from its tool results' text;
  a revision slower than the record's bound still leaves the check kept
  (`engine.TestEditorKeepsCheckAfterSlowRevision`); a warning during the check prints after the answer, and a
  revision taking a tall answer's place does not shrink the view before a print (`chat.TestEditorNotesWaitForAnswer`,
  `TestRevisionHoldsLiveRows`); a command line is not flagged as a span, a value or expression is, and a product's
  program counts as a command only where `checks.commands` names it (`engine.TestCheckSpanCommand`,
  `check.TestCommand`, `config.TestChecksCommands`); a finding that keeps its statement as it is counts it
  supported (`check.TestValidateNoChangeIsSupported`).
- **[E4]** Found in live use: a span the model reworded from what it read — digit grouping, case,
  spacing, quotes, a file and its value as "key: value", an ellipsis — is found, and a different value, key or order
  is not (`engine.TestCheckSpanReworded`). A span in a statement opening as a condition — if, unless, should,
  whether, in case — is the condition's supposition, not a claim of what was seen, and is not looked for; one opening
  with when or once, a time as often as a condition, is (`engine.TestSpanCheckSkipsConditions`).
- **[E4]** An editor given only the lines cited anchored every claim they could not settle (found live), so the checkers
  are given each item cited whole within its bound, the lines cited marked
  (`engine.TestCheckRequestQuotesItemWhole`), an item past its bound as the lines cited with lines either side, said
  so (`engine.TestCheckRequestItemPastItsBound`), the lines cited kept before context however long the lines
  (`engine.TestCheckRequestWindowKeepsCitedLines`), windows not merged past their line bound
  (`engine.TestCheckRequestWindowsKeepTheirBound`), the heading saying what is quoted
  (`engine.TestCheckRequestHeaderSaysWhatIsQuoted`), and one past what the request leaves the same, the request within
  its bound and the same bytes made again (`engine.TestCheckRequestTotalBound`); a claim an editor would have
  anchored undetermined is corrected to the line that holds it, counted in the footer live and reopened, with no
  revision (`engine.TestEditorCorrectsCitation`);
  a corrected citation naming lines not quoted, past the item's end, an item not quoted or an artifact path is
  refused, its finding dropped with why and its statement kept as written (`check.TestValidateCorrectedCitations`,
  `engine.TestEditorRefusesCitationNotQuoted`); a corrected citation the revision keeps is not flagged as added by it
  (`engine.TestEditorRevisionKeepsCorrectedCitation`).
- **[E4]** An editor that accounts for only some of an answer's statements is not kept clean with nothing said (found
  live): findings leaving statements unaccounted are followed by one more call naming only those,
  its answer merged and its correction spliced in (`engine.TestEditorFollowsUpUnaccounted`), and none where all are
  accounted (`engine.TestEditorNoFollowUpWhenAccounted`); a follow-up's correction of a statement it was not asked
  about is dropped, said, its findings flagged (`engine.TestEditorFollowUpCorrectsOnlyAsked`); a follow-up that fails
  or passes the bound leaves the first result, said, the footer `checked N of M statements` live and reopened
  (`engine.TestEditorFollowUpNotTaken`), and a check kept before the follow-up says so too, from its row
  (`engine.TestEditorFooterFromStoredRow`).
- **[E4]** A span not found carries its statement and its closest line,
  the line it was reworded from rather than an earlier one sharing fewer words, a cited item's on a tie, else the
  earliest item's, and a span sharing no word with the evidence none, said unsupported
  (`engine.TestSpanClosestLine`); a line is close only sharing half the span's words or more, one of five not, three
  of five and two of three so (`engine.TestSpanClosestNeedsHalfItsWords`); the engineer's verdict that a span is
  incorrect is kept and appended once, earlier messages byte for byte, the lines marked quoted as stored, read at the
  main model's next call and in the checks tab on reopening, no answer, the same span not judged twice
  (`engine.TestSpanVerdictIncorrectReadByModel`); a closest line not as stored, or cut short, is refused
  (`engine.TestSpanVerdictLineMustBeWhole`); a suggested reload before that call carries it, once
  (`engine.TestSpanVerdictCarriedByReload`).
- **[E4]** The checks tab: only unsupported and incorrect reach the main
  model's next request, worded as the engineer's, incorrect against the closest line where none is marked, and
  supported and inferred are kept with their lines and never sent (`engine.TestSpanVerdictsToldTheModel`); lines past
  an item's end, of an item never seen, a range ending before it begins, and inferred with nothing marked are refused,
  nothing kept (`engine.TestSpanVerdictMarkedLines`); arithmetic that holds is not flagged, arithmetic that does not
  is flagged incorrect with no closest line, and a span outside the grammar — versions or dates compared, a comparison
  with no operator — is searched (`engine.TestSpanArithmetic`);
  the tab lists every flagged value, those with no close line and the engine's arithmetic among them, with their
  states, `f` opening it printing nothing more, and one with no close line is judged as any
  (`chat.TestChecksTabListsEveryFlag`); the evidence is shown two lines before and five after a line, one before and
  two after a range, as the browser shows the item (`chat.TestChecksContext`); a range marked then `y` rests the
  verdict on that range, and `i` with nothing marked is refused (`chat.TestChecksMarkThenSupported`); `e` opens any
  item to mark in and `esc` returns to the same value, its marks of two items kept (`chat.TestChecksAnyItem`); nothing
  is printed while judging, `esc` prints the summary once, the incorrect and unsupported are kept as the model's
  messages, and reopened each flag shows its verdict (`chat.TestChecksSummary`); a mark past the ranges a verdict may
  rest on is refused and `d` drops the last, on the item and in the tab (`chat.TestChecksMarkLimitAndDrop`); `esc`
  while a verdict is kept waits for it, one summary counting it (`chat.TestChecksEscWhileJudging`); any other key ends
  the offer, and after a draft the review and the check are offered together
  (`chat.TestSpanCheckOfferEndsOnOtherInput`); the migrations keep every message row and rowid
  (`store.TestMigration43KeepsRows`), and every verdict row and rowid, stands as supported and wrong as incorrect
  (`store.TestMigration44MapsVerdicts`).

## Open decisions

- Concurrency and rate-limit policy across conversations sharing provider credentials.
- Whether the price table should be configurable per deployment, for example to reflect negotiated discounts.
- Whether a Gemini cache's storage, held between turns, is recorded and estimated.
