package companion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/store"
)

// Kind says who wrote a message, for the record and the chat; the model
// sees only the role.
type Kind string

// Who wrote a message.
const (
	KindCase     Kind = "case"     // the case block, a conversation's first message
	KindUpdate   Kind = "update"   // what the case gained later: comments, attachments, the case ledger
	KindEngineer Kind = "engineer" // the engineer's words
	KindEngine   Kind = "engine"   // the engine's instruction, such as a spent budget
	KindResults  Kind = "results"  // the results of the model's calls
	KindHandoff  Kind = "handoff"  // what a new conversation carries from the one it continues: a compaction's handoff, a reload's carried items
	KindConsult  Kind = "consult"  // a consult's question, with its expert's guidance
	KindModel    Kind = "model"    // the model's answer and its calls
	// KindCorrection is the editor's correction of the answer before it:
	// the text the engineer was shown in its place, said to be so.
	KindCorrection Kind = "correction"
	// KindRevise is the engine's ask that the model write the answer
	// before it again, for what the editor could not settle: the model
	// message after it is that answer's revision.
	KindRevise Kind = "revise"
	// KindShown records, after a revision that was not taken, the text
	// the engineer was shown in place of the answer before the ask: the
	// editor's correction, or the answer as written.
	KindShown Kind = "shown"
	// KindReview is the reviewer's reading of an answer or an item, on
	// the engineer's request: a second opinion of another model, never
	// evidence and never an answer, which the model reads at its next
	// call.
	KindReview Kind = "review"
	// KindVerdict is the engineer's verdict that a code span of an answer,
	// found in no evidence the conversation saw or arithmetic that does
	// not hold, is unsupported or incorrect: the engineer's assertion,
	// never evidence, which the model reads at its next call. A verdict
	// that the span is supported or inferred is not a message.
	KindVerdict Kind = "verdict"
)

// Message is one stored message: the model's view of it, who wrote it,
// and what of the evidence it gives with its content (Shows).
type Message struct {
	model.Message
	Kind       Kind
	Invocation string
	CreatedAt  time.Time
	// Seq is its place in its conversation, from 1.
	Seq int
	// Shows are the evidence lines the engine's text gives under their
	// stored numbers, written with the message and never changed; nil
	// for text that gives none, such as anything a model wrote. Messages
	// leaves it unread: Shows reads it.
	Shows []Shown
}

// Shown is what an engine-authored message gives of one evidence item's
// content, as the engine wrote it after its bounds: the item by its short
// name, and its lines From to To as stored, or, with both zero, the whole
// item. A name alone, as an inventory or a listing gives one, is no Shown.
type Shown struct {
	Name     string
	From, To int
}

// Conversation is one stored conversation.
type Conversation struct {
	ID            string
	Kind          string // main, consult, or a task's own, such as ledger
	Parent        string // a consult's main conversation
	Expert        string // a consult's expert
	CompactedFrom string
	Pins          []json.RawMessage
	CreatedAt     time.Time
	// Exposure says the store has recorded, since the conversation began,
	// what its requests carried of the evidence (Store.Exposure); a
	// conversation stored before the record was kept has none, its
	// exposure unknown.
	Exposure bool
}

// ErrNoConversation means the case has no conversation yet.
var ErrNoConversation = errors.New("no conversation yet")

// Store keeps conversations in a case's store.
type Store struct {
	DB store.DB
}

func (s Store) create(ctx context.Context, c Conversation) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO companion_conversations (id, kind, parent_id, expert, compacted_from, created_at, exposure) VALUES (?, ?, ?, ?, ?, ?, 1)`,
		c.ID, c.Kind, c.Parent, c.Expert, c.CompactedFrom, c.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("companion: new conversation: %w", err)
	}
	return nil
}

// Latest is the case's newest main conversation, the one a turn continues.
func (s Store) Latest(ctx context.Context) (Conversation, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id FROM companion_conversations WHERE kind = 'main' ORDER BY created_at DESC, rowid DESC LIMIT 1`)
	var id string
	if err := row.Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNoConversation
	} else if err != nil {
		return Conversation{}, fmt.Errorf("companion: latest conversation: %w", err)
	}
	return s.Conversation(ctx, id)
}

// Conversation reads one conversation.
func (s Store) Conversation(ctx context.Context, id string) (Conversation, error) {
	var c Conversation
	var pins, created string
	err := s.DB.QueryRowContext(ctx, `SELECT id, kind, parent_id, expert, compacted_from, pins, created_at, exposure FROM companion_conversations WHERE id = ?`, id).
		Scan(&c.ID, &c.Kind, &c.Parent, &c.Expert, &c.CompactedFrom, &pins, &created, &c.Exposure)
	if err != nil {
		return Conversation{}, fmt.Errorf("companion: conversation %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(pins), &c.Pins); err != nil {
		return Conversation{}, fmt.Errorf("companion: conversation %s pins: %w", id, err)
	}
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Conversation{}, fmt.Errorf("companion: conversation %s: %w", id, err)
	}
	return c, nil
}

// Consults are the consults run under a main conversation, oldest first.
func (s Store) Consults(ctx context.Context, parent string) ([]Conversation, error) {
	// The IDs are read first and the rows closed: the store has one
	// connection, which reading each conversation needs.
	ids, err := s.consultIDs(ctx, parent)
	if err != nil {
		return nil, err
	}
	out := make([]Conversation, 0, len(ids))
	for _, id := range ids {
		c, err := s.Conversation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s Store) consultIDs(ctx context.Context, parent string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM companion_conversations WHERE kind = 'consult' AND parent_id = ? ORDER BY created_at, rowid`, parent)
	if err != nil {
		return nil, fmt.Errorf("companion: consults: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("companion: consults: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("companion: consults: %w", err)
	}
	return ids, nil
}

// AddPin records an expert loaded into a conversation.
func (s Store) AddPin(ctx context.Context, conv string, pin json.RawMessage) error {
	c, err := s.Conversation(ctx, conv)
	if err != nil {
		return err
	}
	pins, err := json.Marshal(append(c.Pins, pin))
	if err != nil {
		return fmt.Errorf("companion: pin: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE companion_conversations SET pins = ? WHERE id = ?`, string(pins), conv); err != nil {
		return fmt.Errorf("companion: pin: %w", err)
	}
	return nil
}

// Messages are a conversation's messages in order.
func (s Store) Messages(ctx context.Context, conv string) ([]Message, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, kind, text, calls, results, cache_break, invocation_id, created_at FROM companion_messages WHERE conversation_id = ? ORDER BY seq`, conv)
	if err != nil {
		return nil, fmt.Errorf("companion: messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var calls, results, created string
		var brk int
		if err := rows.Scan(&m.Seq, &m.Kind, &m.Text, &calls, &results, &brk, &m.Invocation, &created); err != nil {
			return nil, fmt.Errorf("companion: messages: %w", err)
		}
		m.Role = model.RoleUser
		if m.Kind == KindModel {
			m.Role = model.RoleModel
		}
		m.Break = brk == 1
		if calls != "" {
			if err := json.Unmarshal([]byte(calls), &m.Calls); err != nil {
				return nil, fmt.Errorf("companion: message calls: %w", err)
			}
		}
		if results != "" {
			if err := json.Unmarshal([]byte(results), &m.Results); err != nil {
				return nil, fmt.Errorf("companion: message results: %w", err)
			}
		}
		if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("companion: message time: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("companion: messages: %w", err)
	}
	return out, nil
}

// append stores m as the conversation's next message, and what it shows
// of the evidence with it, in one transaction.
func (s Store) append(ctx context.Context, conv string, m Message) error {
	var calls, results []byte
	var err error
	if len(m.Calls) > 0 {
		if calls, err = json.Marshal(m.Calls); err != nil {
			return fmt.Errorf("companion: message calls: %w", err)
		}
	}
	if len(m.Results) > 0 {
		if results, err = json.Marshal(m.Results); err != nil {
			return fmt.Errorf("companion: message results: %w", err)
		}
	}
	brk := 0
	if m.Break {
		brk = 1
	}
	const insert = `INSERT INTO companion_messages (conversation_id, seq, kind, text, calls, results, cache_break, invocation_id, created_at)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM companion_messages WHERE conversation_id = ?), ?, ?, ?, ?, ?, ?, ?)`
	args := []any{conv, conv, string(m.Kind), m.Text, string(calls), string(results), brk, m.Invocation, m.CreatedAt.UTC().Format(time.RFC3339Nano)}
	if len(m.Shows) == 0 {
		if _, err := s.DB.ExecContext(ctx, insert, args...); err != nil {
			return fmt.Errorf("companion: append message: %w", err)
		}
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("companion: append message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var seq int
	if err := tx.QueryRowContext(ctx, insert+` RETURNING seq`, args...).Scan(&seq); err != nil {
		return fmt.Errorf("companion: append message: %w", err)
	}
	q := `INSERT INTO companion_exposures (conversation_id, seq, evidence_name, from_line, to_line) VALUES ` +
		strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?), ", len(m.Shows)), ", ")
	rows := make([]any, 0, 5*len(m.Shows))
	for _, sh := range m.Shows {
		rows = append(rows, conv, seq, sh.Name, sh.From, sh.To)
	}
	if _, err := tx.ExecContext(ctx, q, rows...); err != nil {
		return fmt.Errorf("companion: append message: what it shows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("companion: append message: %w", err)
	}
	return nil
}

// request records that a model call, invocation, is sent on conv: it
// carries the conversation's messages through its last, and so what
// they show of the evidence. One write a call.
func (s Store) request(ctx context.Context, conv, invocation string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO companion_requests (invocation_id, conversation_id, through_seq)
		VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) FROM companion_messages WHERE conversation_id = ?))`, invocation, conv, conv)
	if err != nil {
		return fmt.Errorf("companion: the request's record: %w", err)
	}
	return nil
}

// Exposure is what the requests sent on conv carried of the evidence: what
// each message any of them carried shows, in the messages' order. For a
// conversation whose Exposure is false it is not the whole, and reads
// nothing of what came before the record.
func (s Store) Exposure(ctx context.Context, conv string) ([]Shown, error) {
	var out []Shown
	err := s.shown(ctx, func(_ int, sh Shown) { out = append(out, sh) }, `SELECT seq, evidence_name, from_line, to_line FROM companion_exposures
		WHERE conversation_id = ? AND seq <= (SELECT COALESCE(MAX(through_seq), 0) FROM companion_requests WHERE conversation_id = ?)
		ORDER BY seq, rowid`, conv, conv)
	return out, err
}

// Shows are what each message of conv shows of the evidence, by its Seq,
// whether or not a request has carried it yet.
func (s Store) Shows(ctx context.Context, conv string) (map[int][]Shown, error) {
	out := map[int][]Shown{}
	err := s.shown(ctx, func(seq int, sh Shown) { out[seq] = append(out[seq], sh) },
		`SELECT seq, evidence_name, from_line, to_line FROM companion_exposures WHERE conversation_id = ? ORDER BY seq, rowid`, conv)
	return out, err
}

// shown runs a query of companion_exposures' seq, name and lines, giving
// each row to each in order.
func (s Store) shown(ctx context.Context, each func(int, Shown), query string, args ...any) error {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("companion: exposure: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var sh Shown
		if err := rows.Scan(&seq, &sh.Name, &sh.From, &sh.To); err != nil {
			return fmt.Errorf("companion: exposure: %w", err)
		}
		each(seq, sh)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("companion: exposure: %w", err)
	}
	return nil
}

// markBreak makes a conversation's last message a cache break: it ended a
// model call, so everything through it is sent unchanged from now on.
func (s Store) markBreak(ctx context.Context, conv string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE companion_messages SET cache_break = 1
		WHERE conversation_id = ? AND seq = (SELECT MAX(seq) FROM companion_messages WHERE conversation_id = ?)`, conv, conv)
	if err != nil {
		return fmt.Errorf("companion: cache break: %w", err)
	}
	return nil
}
