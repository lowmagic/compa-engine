package store

import (
	"database/sql"
	"fmt"
)

// migrations are the store's schema, one numbered step each, applied in
// order and never edited once released: a change is a new step. The
// database's user_version records how many have been applied.
var migrations = []string{
	// 1: the store's own metadata, and the blob index.
	`CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE blobs (
		digest      TEXT PRIMARY KEY, -- sha256 of the content, hex
		size        INTEGER NOT NULL, -- bytes before compression
		stored_size INTEGER NOT NULL, -- bytes on disk
		created_at  TEXT NOT NULL
	);`,
	// 2: evidence, with its provenance and size; its content is a blob.
	`CREATE TABLE evidence (
		id           TEXT PRIMARY KEY,
		type         TEXT NOT NULL,
		scope        TEXT NOT NULL,
		source       TEXT NOT NULL,
		location     TEXT NOT NULL, -- where in the artifacts, one path per line
		collected_by TEXT NOT NULL,
		blob         TEXT NOT NULL REFERENCES blobs (digest),
		bytes        INTEGER NOT NULL,
		lines        INTEGER NOT NULL,
		lower_bound  INTEGER NOT NULL, -- the content was cut before it was measured
		collection   TEXT,             -- the producing collection's record, without output, JSON
		created_at   TEXT NOT NULL
	);`,
	// 3: investigation state, stored directly, and the journal beside it.
	`CREATE TABLE investigations (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE TABLE units (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		question         TEXT NOT NULL,
		expert_id        TEXT NOT NULL,
		expert_version   TEXT NOT NULL,
		opened_by        TEXT NOT NULL, -- engineer or routing
		status           TEXT NOT NULL, -- open, settled or abandoned
		conclusion       TEXT NOT NULL,
		opened_at        TEXT NOT NULL,
		closed_at        TEXT
	);
	CREATE TABLE steps (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		unit_id          TEXT REFERENCES units (id),
		kind             TEXT NOT NULL,
		parent           TEXT NOT NULL,
		round            INTEGER NOT NULL,
		invocation_ids   TEXT NOT NULL, -- JSON array
		action           TEXT,          -- a collection's request, JSON
		evidence_id      TEXT REFERENCES evidence (id),
		seq              INTEGER NOT NULL,
		created_at       TEXT NOT NULL
	);
	CREATE TABLE findings (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		step_id          TEXT NOT NULL REFERENCES steps (id),
		local_id         TEXT NOT NULL, -- the proposal's own ID
		claim            TEXT NOT NULL,
		evidence_refs    TEXT NOT NULL, -- JSON array
		confidence       TEXT NOT NULL,
		author           TEXT NOT NULL,
		created_at       TEXT NOT NULL
	);
	CREATE TABLE hypotheses (
		id                      TEXT PRIMARY KEY,
		investigation_id        TEXT NOT NULL REFERENCES investigations (id),
		step_id                 TEXT NOT NULL REFERENCES steps (id),
		local_id                TEXT NOT NULL,
		claim                   TEXT NOT NULL,
		status                  TEXT NOT NULL,
		author                  TEXT NOT NULL,
		support                 TEXT NOT NULL, -- JSON array
		contradictions          TEXT NOT NULL, -- JSON array
		discriminating_evidence TEXT NOT NULL,
		created_at              TEXT NOT NULL
	);
	CREATE TABLE journal (
		seq              INTEGER PRIMARY KEY AUTOINCREMENT,
		investigation_id TEXT NOT NULL,
		at               TEXT NOT NULL,
		type             TEXT NOT NULL,
		actor            TEXT NOT NULL,
		step_id          TEXT NOT NULL,
		refs             TEXT NOT NULL, -- JSON array
		description      TEXT NOT NULL
	);`,
	// 4: invocation records, with the dimensions usage is grouped by as
	// columns and the whole record, usage and cost included, as JSON.
	`CREATE TABLE invocations (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL,
		unit_id          TEXT NOT NULL, -- '' outside a diagnostic unit
		step_id          TEXT NOT NULL,
		trigger_ref      TEXT NOT NULL, -- background work's trigger, where there is no step
		task_type        TEXT NOT NULL,
		harness_id       TEXT NOT NULL,
		provider         TEXT NOT NULL,
		model_id         TEXT NOT NULL,
		session_scope    TEXT NOT NULL, -- call, unit or investigation
		outcome          TEXT NOT NULL,
		started_at       TEXT NOT NULL,
		record           TEXT NOT NULL
	);
	CREATE INDEX invocations_by_investigation ON invocations (investigation_id);`,
	// 5: engine-owned conversations and their turns.
	`CREATE TABLE conversations (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL,
		scope            TEXT NOT NULL, -- unit or investigation
		unit_id          TEXT NOT NULL, -- for unit scope
		expert_id        TEXT NOT NULL, -- for investigation scope
		provider         TEXT NOT NULL,
		model_id         TEXT NOT NULL,
		rebuilt_from     TEXT NOT NULL,
		status           TEXT NOT NULL, -- open or closed
		created_at       TEXT NOT NULL,
		closed_at        TEXT
	);
	CREATE TABLE turns (
		conversation_id TEXT NOT NULL REFERENCES conversations (id),
		n               INTEGER NOT NULL, -- from 1
		request         TEXT NOT NULL,    -- JSON, as sent
		response        TEXT NOT NULL,    -- as the harness returned it
		invocation_id   TEXT NOT NULL,
		PRIMARY KEY (conversation_id, n)
	);`,
	// 6: sources, as the case and artifact sources report them; for now,
	// a case's attachments. A source is a record of what exists, not
	// evidence: it is updated as the source's report changes.
	`CREATE TABLE sources (
		origin     TEXT NOT NULL, -- the source that reported it
		name       TEXT NOT NULL, -- its name there
		kind       TEXT NOT NULL, -- attachment
		path       TEXT NOT NULL, -- its own file under the artifact root, or ''
		dir        TEXT NOT NULL, -- where its contents are, or ''
		size       INTEGER NOT NULL, -- bytes as reported, -1 when unknown
		state      TEXT NOT NULL,
		reported   TEXT NOT NULL, -- what the source says it is, or ''
		first_seen TEXT NOT NULL,
		last_seen  TEXT NOT NULL,
		PRIMARY KEY (origin, name)
	);`,
	// 7: the engineer's note on evidence they ingested.
	`ALTER TABLE evidence ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	// 8: summaries, apart from the evidence they summarize, and the jobs
	// that make them. A job's key is the evidence, the processing version
	// and the model, as a summary's cache key is.
	`CREATE TABLE summary_jobs (
		evidence_id        TEXT NOT NULL,
		processing_version TEXT NOT NULL,
		provider           TEXT NOT NULL,
		model_id           TEXT NOT NULL,
		trigger_ref        TEXT NOT NULL,
		status             TEXT NOT NULL, -- queued, running, done, failed or skipped
		reason             TEXT NOT NULL,
		summary_id         TEXT NOT NULL,
		invocation_id      TEXT NOT NULL,
		created_at         TEXT NOT NULL,
		updated_at         TEXT NOT NULL,
		PRIMARY KEY (evidence_id, processing_version, provider, model_id)
	);
	CREATE TABLE summaries (
		id                 TEXT PRIMARY KEY,
		evidence_id        TEXT NOT NULL REFERENCES evidence (id),
		processing_version TEXT NOT NULL,
		provider           TEXT NOT NULL,
		model_id           TEXT NOT NULL,
		invocation_id      TEXT NOT NULL,
		summary            TEXT NOT NULL, -- the accepted summary, JSON
		created_at         TEXT NOT NULL
	);`,
	// 9: a unit's pin — the expert, its catalogs, their versions and
	// digests — as JSON. Null for a unit opened before pins were stored.
	`ALTER TABLE units ADD COLUMN pin TEXT`,
	// 10: the frontier, one row per action instance. An action that leaves
	// the frontier stays here with its state and the reason.
	`CREATE TABLE actions (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		unit_id          TEXT REFERENCES units (id),
		step_id          TEXT NOT NULL, -- the step that placed it, if any
		origin           TEXT NOT NULL, -- expert, engineer, model or capture
		test             TEXT NOT NULL, -- empty for a command
		command          TEXT,          -- a command's pipeline, JSON
		params           TEXT NOT NULL, -- JSON object, keys sorted
		scope            TEXT NOT NULL,
		rank             INTEGER NOT NULL,
		state            TEXT NOT NULL, -- available, in_progress, completed, superseded or blocked
		reason           TEXT NOT NULL,
		changed_by       TEXT NOT NULL,
		created_at       TEXT NOT NULL,
		updated_at       TEXT NOT NULL
	);
	CREATE INDEX actions_by_state ON actions (investigation_id, state);`,
	// 11: what a step presented and what was chosen: the frontier as
	// offered (instance IDs, ranks and origins, JSON), the action the
	// engineer ran, and their note.
	`ALTER TABLE steps ADD COLUMN presented TEXT;
	ALTER TABLE steps ADD COLUMN chosen TEXT NOT NULL DEFAULT '';
	ALTER TABLE steps ADD COLUMN note TEXT NOT NULL DEFAULT '';`,
	// 12: why an action was placed and which step's proposal changed it
	// last; a capture's instructions, what to bring back and the evidence
	// it concerns; retirements a proposal suggested, pending until the
	// engineer decides; and the engineer's own entries.
	`ALTER TABLE actions ADD COLUMN why TEXT NOT NULL DEFAULT '';
	ALTER TABLE actions ADD COLUMN changed_step TEXT NOT NULL DEFAULT '';
	ALTER TABLE actions ADD COLUMN instructions TEXT NOT NULL DEFAULT '';
	ALTER TABLE actions ADD COLUMN bring_back TEXT NOT NULL DEFAULT '';
	ALTER TABLE actions ADD COLUMN concerns TEXT NOT NULL DEFAULT '[]'; -- JSON array of evidence IDs
	CREATE TABLE retire_suggestions (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		action_id        TEXT NOT NULL REFERENCES actions (id),
		step_id          TEXT NOT NULL, -- the step whose proposal suggested it
		reason           TEXT NOT NULL,
		status           TEXT NOT NULL, -- pending, confirmed or dismissed
		created_at       TEXT NOT NULL,
		decided_at       TEXT
	);
	CREATE TABLE entries (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		unit_id          TEXT REFERENCES units (id),
		kind             TEXT NOT NULL, -- observation, claim or hypothesis
		text             TEXT NOT NULL,
		evidence_refs    TEXT NOT NULL, -- JSON array
		status           TEXT NOT NULL, -- a hypothesis's: open, supported or rejected
		author           TEXT NOT NULL,
		created_at       TEXT NOT NULL
	);`,
	// 13: a suggestion's kind: a retirement, or a completion the model's own
	// collection of the same work suggests, with the evidence it produced.
	// The table keeps its name.
	`ALTER TABLE retire_suggestions ADD COLUMN kind TEXT NOT NULL DEFAULT 'retire';
	ALTER TABLE retire_suggestions ADD COLUMN evidence_id TEXT NOT NULL DEFAULT '';`,
	// 14: case summaries, every version kept with the step it arrived
	// after; key names the inputs, harness, template and model, so the
	// same ones are summarized once.
	`CREATE TABLE case_summaries (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		key              TEXT NOT NULL,
		step_id          TEXT NOT NULL, -- the latest step when it was made; empty before any
		invocation_id    TEXT NOT NULL,
		provider         TEXT NOT NULL,
		model_id         TEXT NOT NULL,
		summary          TEXT NOT NULL, -- JSON, the validated case summary
		created_at       TEXT NOT NULL
	);
	CREATE INDEX case_summaries_key ON case_summaries (investigation_id, key);`,
	// 15: what each acquisition found current — the case's metadata,
	// comments and artifact overview, and the attachments the artifact
	// source reported, none included — since evidence is immutable and
	// content-addressed, and an earlier version coming back adds no row.
	`CREATE TABLE acquisitions (
		n           INTEGER PRIMARY KEY AUTOINCREMENT,
		evidence    TEXT NOT NULL, -- JSON array of evidence IDs
		origin      TEXT NOT NULL, -- the artifact source
		attachments TEXT NOT NULL, -- JSON array of attachment names
		created_at  TEXT NOT NULL
	);`,
	// 16: avenues, lines of inquiry: each a question, why it matters and
	// who proposed it, open or retired with the reason; a unit may pursue
	// one, and an avenue's next steps are actions on the frontier for the
	// engineer to carry out. A suggestion to retire an avenue, made when a
	// later case summary no longer proposes it, waits for the engineer.
	`CREATE TABLE avenues (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		question         TEXT NOT NULL,
		why              TEXT NOT NULL,
		evidence_refs    TEXT NOT NULL, -- JSON array
		origin           TEXT NOT NULL, -- summary, routing or engineer
		source           TEXT NOT NULL, -- the case summary that proposed it, if one did
		continued_by     TEXT NOT NULL DEFAULT '', -- the latest case summary that continued it
		status           TEXT NOT NULL, -- open or retired
		reason           TEXT NOT NULL DEFAULT '',
		created_at       TEXT NOT NULL,
		closed_at        TEXT
	);
	CREATE TABLE avenue_suggestions (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		avenue_id        TEXT NOT NULL REFERENCES avenues (id),
		source           TEXT NOT NULL, -- the case summary that left it out
		reason           TEXT NOT NULL,
		status           TEXT NOT NULL, -- pending, confirmed or dismissed
		created_at       TEXT NOT NULL,
		decided_at       TEXT
	);
	ALTER TABLE units ADD COLUMN avenue_id TEXT REFERENCES avenues (id);
	ALTER TABLE actions ADD COLUMN avenue_id TEXT REFERENCES avenues (id);`,
	// 17: a model's choices of experts for questions, each routing kept
	// with the key naming its inputs, so the same inputs make no call; the
	// latest choice for a question is the one shown.
	`CREATE TABLE routings (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		key              TEXT NOT NULL,
		invocation_id    TEXT NOT NULL,
		routing          TEXT NOT NULL, -- JSON, the validated routing
		created_at       TEXT NOT NULL
	);
	CREATE INDEX routings_key ON routings (investigation_id, key);`,
	// 18: who can carry out a next step, as the expert proposing it said:
	// the expert or only the engineer; empty when no one said.
	`ALTER TABLE actions ADD COLUMN by_whom TEXT NOT NULL DEFAULT '';`,
	// 19: case comments a model drafted for an intention,
	// never posted; the draft as validated, its intention as it was.
	`CREATE TABLE drafts (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		intention        TEXT NOT NULL, -- JSON, the intention drafted for
		invocation_id    TEXT NOT NULL,
		provider         TEXT NOT NULL,
		model_id         TEXT NOT NULL,
		draft            TEXT NOT NULL, -- JSON, the validated draft
		created_at       TEXT NOT NULL
	);`,
	// 20: the engineer's rules for leaving case comments out of what
	// models read: one comment, an author's, the internal or
	// the external ones, or a comment and all older. Removing a rule
	// deletes its row.
	`CREATE TABLE comment_exclusions (
		id         TEXT PRIMARY KEY,
		kind       TEXT NOT NULL,
		value      TEXT NOT NULL,
		created_at TEXT NOT NULL
	);`,
	// 21: the evidence a case summary was made from, so one
	// that read what the engineer then left out offers no claims; NULL for
	// a summary made before.
	`ALTER TABLE case_summaries ADD COLUMN inputs TEXT;`,
	// 22: what validation dropped from a draft: a statement
	// whose cited support the request did not hold, which the body may
	// still make.
	`ALTER TABLE drafts ADD COLUMN dropped TEXT;`,
	// 23: routing direction survives automatic refresh and reopening; each
	// answer names every question it considered, including abstentions.
	`CREATE TABLE routing_preferences (investigation_id TEXT PRIMARY KEY, weighting TEXT NOT NULL);
	ALTER TABLE routings ADD COLUMN questions TEXT NOT NULL DEFAULT '[]';`,
	// 24: the timeline. Each record it shows takes a number
	// once, by typed ref, and keeps it; each action a choice number, kept
	// for the case's life and never reused. A case holds one investigation,
	// so the numbers are the case's. The entry an ask reaches back to and
	// the entries left out of every ask are stored by ref, never by number.
	// A note is an engineer entry on an avenue, with or without a unit.
	`CREATE TABLE timeline_numbers (ref TEXT PRIMARY KEY, n INTEGER NOT NULL UNIQUE);
	CREATE TABLE choice_numbers (action_id TEXT PRIMARY KEY, n INTEGER NOT NULL UNIQUE);
	CREATE TABLE pins (investigation_id TEXT PRIMARY KEY, ref TEXT NOT NULL, created_at TEXT NOT NULL);
	CREATE TABLE ignored (
		investigation_id TEXT NOT NULL,
		ref              TEXT NOT NULL,
		created_at       TEXT NOT NULL,
		PRIMARY KEY (investigation_id, ref)
	);
	ALTER TABLE entries ADD COLUMN avenue_id TEXT REFERENCES avenues (id);`,
	// 25: asks. A task's answer records everything its model
	// read — the evidence its conversation disclosed and what the window of
	// its ask rested on — so an answer resting on what the engineer later
	// leaves out is withheld from later windows; and what its ask's window
	// carried, for the timeline to say. A revision is the engineer's text
	// for an answer, a note or an entry, by typed ref, the original kept.
	// The input line's history and the case's own settings — the session
	// scope, the timeline's density and width — are the case's.
	`ALTER TABLE steps ADD COLUMN inputs TEXT; -- JSON array; NULL for a step recorded before
	ALTER TABLE steps ADD COLUMN window TEXT; -- JSON, what an ask's window carried
	CREATE TABLE revisions (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		ref              TEXT NOT NULL,
		text             TEXT NOT NULL,
		author           TEXT NOT NULL,
		created_at       TEXT NOT NULL
	);
	CREATE TABLE input_history (
		n    INTEGER PRIMARY KEY AUTOINCREMENT,
		text TEXT NOT NULL,
		at   TEXT NOT NULL
	);
	CREATE TABLE case_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	// 26: comments read, per engineer, by the host login name,
	// the only identity the engine has; the store is its maker's alone, so
	// the marks are that engineer's. A mark is kept per version of a
	// comment, so one edited at the source is unread again.
	`CREATE TABLE comment_seen (
		evidence_id TEXT NOT NULL,
		engineer    TEXT NOT NULL,
		at          TEXT NOT NULL,
		PRIMARY KEY (evidence_id, engineer)
	);`,
	// 27: annotations: a note may name, by typed timeline
	// ref, the record it is about. Reversible: the column is dropped, the
	// notes kept as notes about nothing.
	`ALTER TABLE entries ADD COLUMN about_ref TEXT;`,
	// 28: tool calling: a turn may answer with function
	// calls, and the turn after it send their results in place of its
	// request's text, as JSON {calls, results}. Reversible: the column is
	// dropped, and a conversation holding such turns begun again.
	`ALTER TABLE turns ADD COLUMN exchange TEXT;`,
	// 29: the comment ledger: what each comment asks, commits
	// to, reports or provides, and what became of each ask and commitment;
	// and which comments the ledger has read. Reversible: the tables are
	// dropped, and the ledger made again from the comments.
	`CREATE TABLE ledger_items (
		investigation_id TEXT NOT NULL,
		id               TEXT NOT NULL, -- l1, l2, … within the investigation
		n                INTEGER NOT NULL,
		kind             TEXT NOT NULL,
		who              TEXT NOT NULL,
		text             TEXT NOT NULL,
		comment_id       TEXT NOT NULL, -- the comment's evidence ID
		due              TEXT NOT NULL,
		status           TEXT NOT NULL,
		status_by        TEXT NOT NULL, -- the settling comment's evidence ID, or engineer
		status_note      TEXT NOT NULL,
		corrected        TEXT NOT NULL, -- the engineer's correction, if any: wrong, or the note
		made_by          TEXT NOT NULL, -- the invocation, or engineer
		created_at       TEXT NOT NULL,
		PRIMARY KEY (investigation_id, id)
	);
	CREATE TABLE ledger_read (
		investigation_id TEXT NOT NULL,
		evidence_id      TEXT NOT NULL,
		PRIMARY KEY (investigation_id, evidence_id)
	);`,
	// 30: questions about the case itself, each with a model's answer as
	// validated, changing nothing else. Reversible: the table
	// is dropped, and the invocation records keep each answer.
	`CREATE TABLE case_answers (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		question         TEXT NOT NULL,
		focus            INTEGER NOT NULL, -- the timeline entry asked about, 0 for none
		invocation_id    TEXT NOT NULL,
		provider         TEXT NOT NULL,
		model_id         TEXT NOT NULL,
		answer           TEXT NOT NULL, -- JSON, the answer as validated
		dropped          TEXT NOT NULL, -- JSON, what validation took out
		not_shown        TEXT NOT NULL, -- JSON, what the request did not show the model
		created_at       TEXT NOT NULL
	);`,
	// 31: what the ledger made of each comment it read: items,
	// a settling of earlier ones, none with why, or unaccounted when its
	// answer said nothing of the comment; empty for a comment read before.
	// Reversible: the column is dropped.
	`ALTER TABLE ledger_read ADD COLUMN outcome TEXT NOT NULL DEFAULT ''`,
	// 32: the case chat's turns: the engineer's message and the
	// orchestrator's reply as validated, or why there is none. Reversible:
	// the table is dropped, and the invocation records keep each reply.
	`CREATE TABLE chat_turns (
		id               TEXT PRIMARY KEY,
		investigation_id TEXT NOT NULL REFERENCES investigations (id),
		message          TEXT NOT NULL,
		invocation_id    TEXT NOT NULL,
		provider         TEXT NOT NULL,
		model_id         TEXT NOT NULL,
		effort           TEXT NOT NULL,
		reply            TEXT NOT NULL, -- JSON, the reply as validated; empty when there is none
		dropped          TEXT NOT NULL, -- JSON, what validation took out
		error            TEXT NOT NULL, -- why there is no reply
		created_at       TEXT NOT NULL
	);`,
	// 33: the lines of the orchestrator's reply the engine ran in the case
	// chat's budgeted mode, as JSON, on the turn that read
	// their answers, whose message is then empty. Reversible: the column is
	// dropped.
	`ALTER TABLE chat_turns ADD COLUMN ran TEXT NOT NULL DEFAULT ''`,
	// 34: a knowledge-base lookup on the frontier:
	// its service and operation as JSON, its parameters in
	// params; empty for a test, a command or the engineer's instructions.
	// Reversible: the column is dropped.
	`ALTER TABLE actions ADD COLUMN lookup TEXT NOT NULL DEFAULT ''`,
	// 35: the initial knowledge-base lookups a case summary proposed and
	// the engine ran, by the lookup's service,
	// operation and parameters, so one already run for the case is not run
	// again; evidence_id is its stored result, empty for one refused before
	// anything was sent. The summary reads every stored result. Reversible:
	// the table is dropped.
	`CREATE TABLE case_lookups (
		key         TEXT PRIMARY KEY,
		evidence_id TEXT NOT NULL,
		created_at  TEXT NOT NULL
	);`,
	// 36: the companion's conversations (docs/MODEL_API.md §The
	// conversation): one per case and each compaction of it, kind main,
	// and one per consult, kind consult, under its main conversation
	// (parent_id) and naming its expert. compacted_from is the conversation
	// a compaction replaced; pins the experts loaded into it, as JSON.
	// Named apart from migration 5's conversations, the previous design's
	// sessions. Reversible: the table is dropped.
	`CREATE TABLE companion_conversations (
		id             TEXT PRIMARY KEY,
		kind           TEXT NOT NULL CHECK (kind IN ('main', 'consult')),
		parent_id      TEXT NOT NULL DEFAULT '',
		expert         TEXT NOT NULL DEFAULT '',
		compacted_from TEXT NOT NULL DEFAULT '',
		pins           TEXT NOT NULL DEFAULT '[]',
		created_at     TEXT NOT NULL
	);`,
	// 37: a conversation's messages in order, never rewritten: who wrote
	// each (kind), its text, a model message's tool calls and the results
	// a user message carries, as JSON, whether a cache break follows it,
	// and the model call that wrote a model message. Reversible: the table
	// is dropped.
	`CREATE TABLE companion_messages (
		conversation_id TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		kind            TEXT NOT NULL CHECK (kind IN ('case', 'update', 'engineer', 'engine', 'results', 'handoff', 'consult', 'model')),
		text            TEXT NOT NULL,
		calls           TEXT NOT NULL DEFAULT '',
		results         TEXT NOT NULL DEFAULT '',
		cache_break     INTEGER NOT NULL DEFAULT 0 CHECK (cache_break IN (0, 1)),
		invocation_id   TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL,
		PRIMARY KEY (conversation_id, seq)
	);`,
	// 38: evidence's short names, e1, e2, …, given once per case in the
	// order the companion names them and never reused, so a citation means
	// the same item in every conversation. Reversible: the table is
	// dropped.
	`CREATE TABLE evidence_names (
		seq         INTEGER PRIMARY KEY AUTOINCREMENT,
		evidence_id TEXT NOT NULL UNIQUE
	);`,
	// 39: the engineer's items: an answer pinned (kind pin) or a draft
	// written for an intention (kind draft, with its intention and
	// audience), from the conversation that wrote it, named i1, i2, … by
	// seq. Kept for the engineer to copy, never posted. Reversible: the
	// table is dropped.
	`CREATE TABLE companion_items (
		seq             INTEGER PRIMARY KEY AUTOINCREMENT,
		kind            TEXT NOT NULL CHECK (kind IN ('pin', 'draft')),
		conversation_id TEXT NOT NULL,
		intention       TEXT NOT NULL DEFAULT '',
		audience        TEXT NOT NULL DEFAULT '',
		text            TEXT NOT NULL,
		created_at      TEXT NOT NULL
	);`,
	// 40: the case ledger, kept in migration 29's tables keyed by the
	// case's ID in investigation_id, so rows the previous design kept under
	// its investigations' IDs are ignored: a provides item's attachment as
	// its comment names it; and a ledger call's own conversation, kind
	// ledger, which needs companion_conversations made again with the kind
	// allowed, its rows and their rowids kept. Reversible: the column is
	// dropped, and the table made again without the kind once the ledger's
	// conversations are deleted.
	`ALTER TABLE ledger_items ADD COLUMN attachment TEXT NOT NULL DEFAULT '';
	CREATE TABLE companion_conversations_40 (
		id             TEXT PRIMARY KEY,
		kind           TEXT NOT NULL CHECK (kind IN ('main', 'consult', 'ledger')),
		parent_id      TEXT NOT NULL DEFAULT '',
		expert         TEXT NOT NULL DEFAULT '',
		compacted_from TEXT NOT NULL DEFAULT '',
		pins           TEXT NOT NULL DEFAULT '[]',
		created_at     TEXT NOT NULL
	);
	INSERT INTO companion_conversations_40 (rowid, id, kind, parent_id, expert, compacted_from, pins, created_at)
		SELECT rowid, id, kind, parent_id, expert, compacted_from, pins, created_at FROM companion_conversations;
	DROP TABLE companion_conversations;
	ALTER TABLE companion_conversations_40 RENAME TO companion_conversations;`,
	// 41: the editor's check of each answer:
	// a check's own conversation, kind editor; the
	// editor's correction of an answer, the engine's ask for its revision
	// and, after a revision not taken, what the engineer was shown, as
	// messages of their own, kinds correction, revise and shown, which
	// need both tables made again with the kinds allowed, their rows
	// and rowids kept; and checks, one row a check: the role that made it,
	// the main conversation and the model call whose answer it checked,
	// its own conversation and the call whose findings it accepted, the
	// findings as JSON under their schema, what validation dropped, how it
	// ended and why. Reversible: checks is dropped, and both tables made
	// again without the kinds once the editor's conversations and those
	// messages are deleted.
	`CREATE TABLE companion_conversations_41 (
		id             TEXT PRIMARY KEY,
		kind           TEXT NOT NULL CHECK (kind IN ('main', 'consult', 'ledger', 'editor')),
		parent_id      TEXT NOT NULL DEFAULT '',
		expert         TEXT NOT NULL DEFAULT '',
		compacted_from TEXT NOT NULL DEFAULT '',
		pins           TEXT NOT NULL DEFAULT '[]',
		created_at     TEXT NOT NULL
	);
	INSERT INTO companion_conversations_41 (rowid, id, kind, parent_id, expert, compacted_from, pins, created_at)
		SELECT rowid, id, kind, parent_id, expert, compacted_from, pins, created_at FROM companion_conversations;
	DROP TABLE companion_conversations;
	ALTER TABLE companion_conversations_41 RENAME TO companion_conversations;
	CREATE TABLE companion_messages_41 (
		conversation_id TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		kind            TEXT NOT NULL CHECK (kind IN ('case', 'update', 'engineer', 'engine', 'results', 'handoff', 'consult', 'model', 'correction', 'revise', 'shown')),
		text            TEXT NOT NULL,
		calls           TEXT NOT NULL DEFAULT '',
		results         TEXT NOT NULL DEFAULT '',
		cache_break     INTEGER NOT NULL DEFAULT 0 CHECK (cache_break IN (0, 1)),
		invocation_id   TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL,
		PRIMARY KEY (conversation_id, seq)
	);
	INSERT INTO companion_messages_41 (rowid, conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at)
		SELECT rowid, conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at FROM companion_messages;
	DROP TABLE companion_messages;
	ALTER TABLE companion_messages_41 RENAME TO companion_messages;
	CREATE TABLE checks (
		id                    TEXT PRIMARY KEY,
		investigation_id      TEXT NOT NULL,
		role                  TEXT NOT NULL CHECK (role IN ('editor', 'reviewer')),
		conversation_id       TEXT NOT NULL,
		checked_invocation_id TEXT NOT NULL,
		check_conversation_id TEXT NOT NULL,
		invocation_id         TEXT NOT NULL,
		schema_id             TEXT NOT NULL,
		findings              TEXT NOT NULL,
		dropped               TEXT NOT NULL,
		outcome               TEXT NOT NULL CHECK (outcome IN ('clean', 'corrected', 'flagged', 'revised', 'revision_failed', 'unchecked')),
		error                 TEXT NOT NULL,
		created_at            TEXT NOT NULL
	);
	CREATE INDEX checks_by_checked ON checks (checked_invocation_id);`,
	// 42: the reviewer's review of an answer or an item on request:
	// a review's own conversation,
	// kind review, and the review as a message of the main conversation,
	// kind review, which need both tables made again with the kind
	// allowed, their rows and rowids kept; and checks made again likewise,
	// adding what was checked by the name the chat gives it (a7, i3) and
	// how many statements the text had, and the review's outcomes, reviewed
	// and failed. Reversible: the review conversations and messages, and
	// the reviewer's checks, are deleted, and the three tables made again
	// as 41 left them.
	`CREATE TABLE companion_conversations_42 (
		id             TEXT PRIMARY KEY,
		kind           TEXT NOT NULL CHECK (kind IN ('main', 'consult', 'ledger', 'editor', 'review')),
		parent_id      TEXT NOT NULL DEFAULT '',
		expert         TEXT NOT NULL DEFAULT '',
		compacted_from TEXT NOT NULL DEFAULT '',
		pins           TEXT NOT NULL DEFAULT '[]',
		created_at     TEXT NOT NULL
	);
	INSERT INTO companion_conversations_42 (rowid, id, kind, parent_id, expert, compacted_from, pins, created_at)
		SELECT rowid, id, kind, parent_id, expert, compacted_from, pins, created_at FROM companion_conversations;
	DROP TABLE companion_conversations;
	ALTER TABLE companion_conversations_42 RENAME TO companion_conversations;
	CREATE TABLE companion_messages_42 (
		conversation_id TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		kind            TEXT NOT NULL CHECK (kind IN ('case', 'update', 'engineer', 'engine', 'results', 'handoff', 'consult', 'model', 'correction', 'revise', 'shown', 'review')),
		text            TEXT NOT NULL,
		calls           TEXT NOT NULL DEFAULT '',
		results         TEXT NOT NULL DEFAULT '',
		cache_break     INTEGER NOT NULL DEFAULT 0 CHECK (cache_break IN (0, 1)),
		invocation_id   TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL,
		PRIMARY KEY (conversation_id, seq)
	);
	INSERT INTO companion_messages_42 (rowid, conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at)
		SELECT rowid, conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at FROM companion_messages;
	DROP TABLE companion_messages;
	ALTER TABLE companion_messages_42 RENAME TO companion_messages;
	CREATE TABLE checks_42 (
		id                    TEXT PRIMARY KEY,
		investigation_id      TEXT NOT NULL,
		role                  TEXT NOT NULL CHECK (role IN ('editor', 'reviewer')),
		conversation_id       TEXT NOT NULL,
		checked_invocation_id TEXT NOT NULL,
		checked_name          TEXT NOT NULL DEFAULT '',
		statements            INTEGER NOT NULL DEFAULT 0,
		check_conversation_id TEXT NOT NULL,
		invocation_id         TEXT NOT NULL,
		schema_id             TEXT NOT NULL,
		findings              TEXT NOT NULL,
		dropped               TEXT NOT NULL,
		outcome               TEXT NOT NULL CHECK (outcome IN ('clean', 'corrected', 'flagged', 'revised', 'revision_failed', 'unchecked', 'reviewed', 'failed')),
		error                 TEXT NOT NULL,
		created_at            TEXT NOT NULL
	);
	INSERT INTO checks_42 (rowid, id, investigation_id, role, conversation_id, checked_invocation_id, check_conversation_id, invocation_id, schema_id, findings, dropped, outcome, error, created_at)
		SELECT rowid, id, investigation_id, role, conversation_id, checked_invocation_id, check_conversation_id, invocation_id, schema_id, findings, dropped, outcome, error, created_at FROM checks;
	DROP TABLE checks;
	ALTER TABLE checks_42 RENAME TO checks;
	CREATE INDEX checks_by_checked ON checks (checked_invocation_id);`,
	// 43: the engineer's verdicts on the span flags: span_verdicts keeps
	// each, whatever it is, by the answer as the model first wrote it (its
	// conversation and seq) and the span; and a verdict that the claim is
	// wrong is also a message of the main conversation, kind verdict, which
	// needs companion_messages made again with the kind allowed, its rows
	// and rowids kept. Reversible: span_verdicts is dropped, the verdict
	// messages deleted, and companion_messages made again as 42 left it.
	`CREATE TABLE companion_messages_43 (
		conversation_id TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		kind            TEXT NOT NULL CHECK (kind IN ('case', 'update', 'engineer', 'engine', 'results', 'handoff', 'consult', 'model', 'correction', 'revise', 'shown', 'review', 'verdict')),
		text            TEXT NOT NULL,
		calls           TEXT NOT NULL DEFAULT '',
		results         TEXT NOT NULL DEFAULT '',
		cache_break     INTEGER NOT NULL DEFAULT 0 CHECK (cache_break IN (0, 1)),
		invocation_id   TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL,
		PRIMARY KEY (conversation_id, seq)
	);
	INSERT INTO companion_messages_43 (rowid, conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at)
		SELECT rowid, conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at FROM companion_messages;
	DROP TABLE companion_messages;
	ALTER TABLE companion_messages_43 RENAME TO companion_messages;
	CREATE TABLE span_verdicts (
		conversation_id TEXT NOT NULL,
		answer_seq      INTEGER NOT NULL,
		answer_name     TEXT NOT NULL,
		span            TEXT NOT NULL,
		statement       TEXT NOT NULL,
		evidence_id     TEXT NOT NULL,
		evidence_name   TEXT NOT NULL,
		line            INTEGER NOT NULL CHECK (line >= 1),
		line_text       TEXT NOT NULL,
		verdict         TEXT NOT NULL CHECK (verdict IN ('stands', 'wrong')),
		created_at      TEXT NOT NULL,
		PRIMARY KEY (conversation_id, answer_seq, span)
	);`,
	// 44: the engineer's verdicts take four kinds and rest on several
	// ranges of lines or none: span_verdicts made again with the kinds
	// supported, inferred, unsupported and incorrect, stands kept as
	// supported and wrong as incorrect, every row and rowid kept, its line
	// moved to span_verdict_lines, which holds each verdict's ranges in
	// order. The messages the model read are not touched. Not reversible:
	// an inferred or unsupported verdict, and one resting on no range or
	// several, has no form in 43.
	`CREATE TABLE span_verdict_lines (
		conversation_id TEXT NOT NULL,
		answer_seq      INTEGER NOT NULL,
		span            TEXT NOT NULL,
		ord             INTEGER NOT NULL CHECK (ord >= 1),
		evidence_id     TEXT NOT NULL,
		evidence_name   TEXT NOT NULL,
		from_line       INTEGER NOT NULL CHECK (from_line >= 1),
		to_line         INTEGER NOT NULL CHECK (to_line >= from_line),
		text            TEXT NOT NULL,
		PRIMARY KEY (conversation_id, answer_seq, span, ord)
	);
	INSERT INTO span_verdict_lines (conversation_id, answer_seq, span, ord, evidence_id, evidence_name, from_line, to_line, text)
		SELECT conversation_id, answer_seq, span, 1, evidence_id, evidence_name, line, line, line_text FROM span_verdicts ORDER BY rowid;
	CREATE TABLE span_verdicts_44 (
		conversation_id TEXT NOT NULL,
		answer_seq      INTEGER NOT NULL,
		answer_name     TEXT NOT NULL,
		span            TEXT NOT NULL,
		statement       TEXT NOT NULL,
		verdict         TEXT NOT NULL CHECK (verdict IN ('supported', 'inferred', 'unsupported', 'incorrect')),
		created_at      TEXT NOT NULL,
		PRIMARY KEY (conversation_id, answer_seq, span)
	);
	INSERT INTO span_verdicts_44 (rowid, conversation_id, answer_seq, answer_name, span, statement, verdict, created_at)
		SELECT rowid, conversation_id, answer_seq, answer_name, span, statement,
			CASE verdict WHEN 'stands' THEN 'supported' WHEN 'wrong' THEN 'incorrect' END, created_at FROM span_verdicts;
	DROP TABLE span_verdicts;
	ALTER TABLE span_verdicts_44 RENAME TO span_verdicts;`,
	// 45: what each model call carried of the evidence (docs/MODEL_API.md
	// §Citations): companion_exposures, written with an engine-authored
	// message and never changed, holds the lines of each item it gives
	// with their content, by short name, from_line and to_line both 0 for
	// the whole item; companion_requests, one row a call as it is sent,
	// the messages it carried, through through_seq. A conversation's
	// exposure is what its messages carried by a request show. exposure
	// marks a conversation whose record is kept from its start: one made
	// before this step has 0, its exposure unknown, and nothing is
	// inferred for it; no message is touched. Reversible: both tables and
	// the column are dropped, and every conversation is then read as one
	// made before it.
	`ALTER TABLE companion_conversations ADD COLUMN exposure INTEGER NOT NULL DEFAULT 0 CHECK (exposure IN (0, 1));
	CREATE TABLE companion_exposures (
		conversation_id TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		evidence_name   TEXT NOT NULL,
		from_line       INTEGER NOT NULL,
		to_line         INTEGER NOT NULL,
		CHECK ((from_line = 0 AND to_line = 0) OR (from_line >= 1 AND to_line >= from_line))
	);
	CREATE INDEX companion_exposures_message ON companion_exposures (conversation_id, seq);
	CREATE TABLE companion_requests (
		invocation_id   TEXT NOT NULL,
		conversation_id TEXT NOT NULL,
		through_seq     INTEGER NOT NULL CHECK (through_seq >= 0)
	);
	CREATE INDEX companion_requests_conversation ON companion_requests (conversation_id);`,
}

// migrate applies the steps a database has not had, each in its own
// transaction with its version, so a failed step leaves the database at
// the last good version. A database newer than the steps given is refused
// rather than used by an engine that does not know its schema.
func migrate(db *sql.DB, steps []string) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("store: reading schema version: %w", err)
	}
	if v > len(steps) {
		return fmt.Errorf("store: schema version %d is newer than this engine's %d", v, len(steps))
	}
	for i := v; i < len(steps); i++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if _, err := tx.Exec(steps[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}
