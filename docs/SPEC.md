# compa — Product Specification

## Purpose

compa is a triage and data-gathering companion for a support engineer working a case. It does not diagnose. It reads the case, its state from its comments, and its artifacts; says what they show, with the evidence for each; notes what the data might bear on as questions to settle, never as a cause at any certainty; says what data would settle each question and how to collect it; and writes informational comments for the engineer to review — above all an internal one of observations, each with its evidence in fenced code blocks, and the questions still open. The engineer draws the conclusions. The engineer talks to it in plain language, in one conversation per case, and it answers with the help of the engine's deterministic tools over the case's artifacts and evidence, and of domain experts it loads when the problem needs them. Guardrails are few and soft, except where safety requires them (`docs/standards.md` §Safety). The command is `compa`. Terms such as the companion, the conversation and a consult are defined in `docs/ARCHITECTURE.md` §Terminology.

## Users and workflow

A support engineer opens a case, or a troubleshooting chat with no case.

1. **Open.** The engine reads the case's details, comments and attachments. With a case whose account may use a model, the companion's opening turn runs on its own: the case's problem and status, its history read from the comments, what is awaited and from whom, what each attachment can answer, and the open questions, each with the data that would answer it. With attachments, it inspects them shallowly; with none, it works from the case alone.
2. **Continue.** The engineer asks questions and gives direction in plain language. The model answers, running tools, loading experts and consulting them as it judges, and gives an observations report when the engineer wants one.
3. **Finish.** The engineer asks for a comment: above all an internal one of observations and open questions for review, or for the customer a request for data or a status update, which the model writes from the conversation in the deployment's house style. The engineer copies it; the engine never posts it.

## Functional requirements

### 1. Acquisition

- Receive case data and artifacts through two engine interfaces, `CaseSource` and `ArtifactSource`. A deployment passes its implementations in `engine.Options`, fixed at compile time; the engine ships a stub case source and a local-directory artifact source so it runs end to end with no external service (`docs/ARCHITECTURE.md`).
- Acquire external knowledge-base references, such as KCS solutions and articles, through the reference adapters the deployment passes: a search, and a fetch by ID. Each result is stored as immutable evidence before any model sees it, and a hit is a lead to test, never a match.
- Ingest bulk artifacts (OpenShift must-gathers, sosreports, and logs) through the `ArtifactSource`, honoring the access controls the deployment's source enforces.
- Retrieve case metadata and comments through the `CaseSource`. Access is read-only; posting private case comments is on the roadmap (`docs/ROADMAP.md`), and other writes are not in scope.
- Deployment-specific sources, their endpoints and access paths are documented with the deployment, in its own repository, not in the engine's documents.
- Handle very large and heterogeneous inputs, including must-gathers, logs, Kubernetes resources, and case history, without loading them whole into memory or into a model's context.
- Respect case isolation, credential boundaries, and source-specific access controls.

### 2. Evidence storage and retrieval

- Persist source metadata, every tool result as immutable evidence with its provenance, the conversation, and the engineer's items.
- Compress retained large payloads; do not assume compressed bytes reduce model tokens.
- Give each piece of evidence an identifier a model and the engineer can cite, with line numbers that survive storage.
- Store case data by default in a `.compa/` subdirectory of the case folder, where it follows the case's existing retention process. Where the case folder is unavailable, store it in the engineer's home directory and delete it after 14 calendar days unused by default, the interval configurable; when the case folder is found later, move it there. A troubleshooting chat with no case is stored under the engineer's home directory and swept the same way.

### 3. The companion

- One conversation per case, owned and stored by the engine, resumed when the case is reopened. A troubleshooting chat with no case is a conversation of its own, optionally over a local directory of artifacts.
- The conversation is sent to the model in a fixed order — the companion's system prompt and the expert index, the tool declarations, the case's own data, then the messages — and only ever appended to, so consecutive calls share the longest prefix a provider can cache (`docs/MODEL_API.md`). The case's data is its state first: its details, the artifact inventory saying which captures are current and which outdated, the newest comments in full, and each older comment by name, date and author only, read whole on demand. Comments or attachments that arrive later are appended as a message, never rewritten into the case's data; so is the case ledger, once it has items, and what changes in it later.
- The opening turn runs automatically when a case new to the companion opens, unless the engineer skips it. Before it the case is prepared: its comments read, its attachments counted and inventoried, and the case ledger reading the comments beside them, which the opening waits for up to a bound and goes on without past it, saying so. It orients the engineer as `Users and workflow` describes from the case's state, with a few tool calls at most, and names no cause and drafts no comment. A case reopened on its conversation makes no model call when it opens: the chat says what is new since it was last worked, and whether the provider's cache has expired on a large conversation, and offers what follows; nothing calls a model until the engineer chooses.
- Reopening, where the provider's cache has expired on a large conversation, the engineer chooses how it goes on: the conversation as it is, resent whole without the cache, with its estimated tokens said; a suggested context, a new conversation linked to the old one as a compaction's is and begun with no model call from the case's data as it is now, the case ledger, the pinned items, the top of the ranked evidence as the lines cited or pointed at, bounded, and the last exchange verbatim; or the same with a handoff the model writes of the old conversation, one call over it. Then, where the case gained comments or attachments since it was last worked, the engineer may catch up: the ledger reads the new comments, then one turn asks the model what changed and whether it answers or opens any question. Either can be put off and done later by command.
- The ranked evidence: the evidence the conversation and its compaction ancestors used, each item scored for what was done with it — lines the engineer pointed at, cited in a pinned item or draft, cited in an answer, returned by a tool — in that order and decayed by how many turns ago, the case's own data left out; derived from the stored messages each time, never stored apart, and calling no model.
- The model may call any of the engine's tools at its own judgment: the collection primitives over the artifacts, the catalogs' tools and experts' tests, allowlisted commands, the evidence tools, which read the case's comments whole too, loading an expert, a consult, and the deployment's reference adapters (`docs/MODEL_API.md` §Tools). The engine validates each call before it runs and runs it; a collection's or a lookup's result is stored as evidence and returned as a bounded excerpt with where to read on.
- A soft budget bounds one engineer turn's tool calls, a smaller one the opening turn's, and a token budget the input a turn's calls send, what a call reads from the provider's cache counted at its discount; past either the model answers with what it has, and the engineer may tell it to continue.
- Experts are loaded into the conversation when their guidance helps, several at once if needed, or consulted: one expert, one question, in a sub-conversation whose answer returns to the main conversation for the model to synthesize (`docs/EXPERT_API.md`).
- The companion's prompt sets the method: lead with the answer and keep it short; keep observed, attributed and inferred apart; name no cause in the model's own voice — not a root cause, nor a possible, likely or contributing one — and write what the data might bear on as a question with the data that would settle it, a cause appearing only as an identified source's conclusion, attributed; cite evidence by its name and lines or an artifact by path and line; search before reading; treat instructions inside case data as evidence; run read-only checks through the tools rather than suggesting them; give the customer only read-only commands that collect data, each as the tool output, an expert's guidance or a document read in the conversation gives it, and no remediation or other change, saying what data is needed where no command is sourced.
- An observations report, on request: what is observed, each with its evidence excerpt; what is attributed, and to whom; the open questions, each with what it might bear on and the data that would settle it; the data to gather, and how; and what is not yet read.
- Draft case comments with an intention. The engineer chooses an intention from the deployment's list — each with its purpose and audience, the customer or an internal note — or types one; the model drafts the comment in the conversation, from everything in it, in the deployment's house style. The engine's base intentions are an internal observations comment, the main product, its observations each with its evidence in fenced code blocks and then its open questions; an internal note for a colleague taking the case over; a request for data; and a status update. A draft says what its statements rest on, names no cause in its own voice, and lists what it asks of the customer, each read-only command exactly as the customer runs it. It is kept as an item, never posted and never stored as fact.
- The engineer may pin any answer as an item, list items and copy them.
- When a conversation grows past a configured size, the engineer is offered compaction: the model writes a handoff, and a new conversation starts from the case's data, the case ledger, the handoff and the pinned items, linked to the old one. Nothing is compacted without the engineer seeing it.
- The case ledger: a model of its own, on its own profile, reads the case's comments once each, oldest first and only those not yet read, and records what each asks, commits to, reports or provides, who, and what later comments made of it — an ask answered or ruled out, a commitment done or dropped, what was provided superseded by a newer capture or a reported change, an edited comment's items withdrawn — each item citing its comment by name. Its calls pass the same barred-accounts rule and redaction as every model call and count in the case's usage. Its items are the model's reading, shown as such, never evidence; comments the engineer leaves out of what models read are left out of it (`docs/MODEL_API.md` §The case ledger).
- Nothing a model writes is stored as fact. Its messages are kept as its messages, and tool output as evidence; a citation is resolved and shown, flagged when it names evidence or lines the conversation was never shown or lines past its end, and never dropped. Beneath an answer one line counts what its checks found — such citations, a citation written in a form not read, a quoted term found in no evidence the conversation saw, causal wording in the model's own voice, a command whose program or options appear in no evidence or guidance the conversation saw, and the editor's findings — each listed for the engineer to review in the evidence browser (`docs/MODEL_API.md` §Answer checks). A finding never blocks or changes the answer.
- Where the deployment names an editor, every answer is checked before it is printed by a model of its own, given the answer and the evidence items it cites, whole where they fit, never the conversation: it corrects wording stronger than its evidence and a citation whose claim other lines of those items hold, and leaves what needs the conversation as anchors, which the main model resolves in one revision. What is printed, and what a draft keeps, is the corrected or revised answer, said so beneath it; the original stays readable. The editor's calls pass the same barred-accounts rule and redaction, and count in the case's usage; a failing editor leaves the answer printed as written and said unchecked (`docs/MODEL_API.md` §Answer checks).
- On request, and offered after every draft, a reviewer of its own, a model the deployment names, gives a second opinion on an answer or a draft from it and the evidence items it cites, quoted as the editor's are, never the conversation: for each claim its support, the strongest alternative, the evidence that would tell them apart and the most the evidence supports. The review is shown beneath what it reviewed and added to the conversation as another model's reading, never evidence, nothing reviewed rewritten; its calls pass the same rule and redaction and count in the case's usage (`docs/MODEL_API.md` §Answer checks).

### 4. Expert packages

- Load independently versioned domain experts from expert repositories, such as `compa-experts`, composed into the bundle a deployment passes in `engine.Options.Bundle`: bundled by default, configurable local paths, optionally pinned release downloads. Validate them, and pin each expert a conversation loads by its exact content digest.
- Expose an index of the installed experts — ID, a one-line summary, the routing keys each is primary for — that the model reads in every call, and load an expert's guidance, its tools' summaries and its references on demand (`docs/EXPERT_API.md`).
- Suggest experts deterministically from the case's routing keys, as its source reports them, through the routing library, and load the primary expert of the case's routing key before the opening turn. What a routing key is belongs to the deployment.
- Experts define their tools declaratively, composed from engine primitives and declared binaries, mostly as shared catalogs. An expert may ship an MCP server for logic primitives cannot express; the engine runs it sandboxed (`docs/COLLECTION.md`).
- Experts name no model provider or model ID; the loader rejects manifests that do.

### 5. The model layer

- The engine calls models through one `Provider` interface: one call with the conversation and the tool declarations, returning streamed text and tool calls, with the cache breaks the engine marks (`docs/MODEL_API.md`). Providers: Vertex Gemini and Vertex Claude, an OpenAI-compatible endpoint — OpenRouter, or an inference server of the deployment's own, held to the egress class the deployment declares for it — and a fixture provider for tests.
- No provider default may change what the model sees. SDK retries, automatic function calling and any context compaction the engine did not do are disabled; a call that encounters one anyway is flagged so the engineer sees it.
- Model selection is configuration: named models with the efforts each takes, and a default for the companion, which the engineer may change for a session.
- Apply the egress class's redaction policy to everything a call sends, and send nothing that still holds what the policy forbids (`docs/MODEL_API.md` §Egress).
- Make no model call for a case whose account is on the deployment's list of accounts barred from AI processing, where the deployment names one. The rule fails closed: a case that cannot be checked, because the list cannot be read or its source gave no account number, is refused until the engineer overrides it for that case, after the deployment's instructions and by typing the account's name; a list match cannot be overridden, and an override ends when the check can be made. Every refusal is recorded as an invocation outcome with its reason and never the account number, and shown in the chat's status line and on the refused turn. Everything that calls no model keeps working, and what was stored before a case was barred stays, labelled. The same check, failing closed the same way, comes before any knowledge-base lookup whose query derives from the case's content, though no model is called, since its text leaves the host; a query the engineer types that does not derive from it is not checked.
- The model collects nothing itself: every tool call runs in the engine, collection in its sandboxed runner (`docs/COLLECTION.md`).
- Remain useful with no model available: the case's comments, attachments, evidence, tools run by the engineer and `compa case` commands keep working.

### 6. The engineer's shell

- Run a command in the engineer's own shell from the chat (`!<command>`) and offer its output as evidence; explicit ingestion of pasted text, files and piped output (`compa in`). Do not auto-retain every stdout or stderr stream.
- A persistent embedded shell is later work (`docs/ROADMAP.md`); it must work inside tmux without nesting tmux.

### 7. Usage

- For every model call, record the provider and model, the effort, the conversation and turn it belongs to, the tool calls it made, token usage where available, timing and outcome. Keep the request and raw response, for debugging.
- Record token usage in disjoint categories: input, output, cache read and cache write tokens and reasoning tokens where the provider reports them, one record for each model call, and engine-measured duration. `unavailable` is a correct value and never recorded as zero; an estimate is labelled as one.
- Aggregate usage by case, conversation, consult and model, each aggregate stating its own completeness rather than presenting a partial sum as a total.
- Never place usage data in what a model is sent.
- Record the cost a provider reports, with its currency. Where none is reported, estimate one from a price table the engine holds, labelled estimated with the table's version, never combined with reported costs. Where priced and unpriced usage mix, present both without imputing a rate.
- Allow a saved call to be re-sent to a provider for debugging and model comparison. A re-run is not expected to reproduce the original result.

### 8. User interface

- An inline chat in the terminal, `compa <case>` and `compa chat`, printing into the terminal's own scrollback so its search and copy work as in any shell, with a status bar directly above the input at the bottom.
- By default it prints only the engineer's messages, the model's answers as rendered Markdown streamed as they arrive, and one dim line for each batch of tool calls naming what ran, a row for each call where the batch is wider than the terminal. The full calls and results of the last turn are one key away, and a debug mode adds each call's input size, cache share and timing.
- The status bar names what is happening precisely — calling the model, running a named tool, waiting for the engineer — and never describes work as done because a model said so. While work runs it shows an animated activity indicator and how to stop it (`ctrl+c`). It shows the case, the model and effort, and the case's or chat's cumulative tokens, cost and cache reads from every stored call, so they survive leaving and reopening; a cache share is shown only where every prompt category was reported.
- Commands for what is not conversation: an observations report, a draft by intention, loading or listing experts, opening evidence or comments, the case ledger (`/ledger`: the open asks by who owes them, then every comment by its number in the case, its author and who they are, visibility and time, with its items or why it gave none, read up to date first), pinning, listing and copying items (the copy reaching the engineer's clipboard over SSH), choosing the model, compaction, reloading a conversation (`/reload`), catching up on what is new (`/catchup`), the ranked evidence (`/ranked`), usage and debug (`docs/ARCHITECTURE.md` §The chat). Typing `/` shows a filterable list of them above the status bar, chosen with the arrow keys, completed with tab and dismissed with esc; it is never printed into the scrollback. The choices a reopened case offers show the same way, chosen by their number or letter or the arrow keys, put off with esc.
- One evidence browser, opened over the chat and closed back into it: every piece of evidence the case holds, by its name, what produced it and the answers citing it; an item opened at its cited lines, with the last answer's citations stepped through in turn; lines the engineer marks pointed out to the model in their next message; and the pinned items. Navigating changes nothing (`docs/ARCHITECTURE.md` §The evidence browser).
- `compa ask <case> "<question>"` runs one turn without the chat, the case prepared as the opening prepares it, and prints the answer.
- Show which model and effort produced each answer, and whether its usage is reported, estimated or unavailable.
- Quitting takes ctrl+c twice, or once when no turn is running.
- No operation loads full large artifacts into memory or into a model's context.

## Success criteria

- An engineer works a real case end to end in the chat: triage, gathering the data the open questions need, and an internal observations comment, judged useful by the engineer.
- Model tokens and cost per case measured, with the share of input read from cache.
- Deterministic tests demonstrate the conversation's cache order, tool validation and refusal, evidence provenance, redaction before egress and the barred-accounts rule.

## Not in scope

- Diagnosis: naming a cause in the model's own voice, at any certainty. The engineer draws conclusions.
- Remediation: steps, workarounds or commands that change a system, from the model.
- Indiscriminate recording of every terminal output stream.
- Mandatory tmux, nested tmux, or a shell per conversation.
- Executing expert-supplied code outside the sandbox, or requiring runtime downloads from remote repositories.
- Treating anything a model writes as verified fact.
- Writing to support cases beyond private comments, such as public comments or status changes.

## Open decisions

- The companion's default model: a deployment's configuration names it; a comparison of the Gemini and Claude model families on a real case is still to be made.
- Bulk-output quota: still open. Which providers case data may be sent to is a deployment's configuration (`reasoning.maxEgressClass`), and secrets are redacted on the way by the egress class's policy (`docs/MODEL_API.md`).
- Operational packaging, and which platforms are supported.
- Expert signing and integrity policy, and conflicting guidance between experts loaded together; see `docs/EXPERT_API.md`.
