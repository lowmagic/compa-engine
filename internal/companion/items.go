package companion

import (
	"context"
	"fmt"
	"time"
)

// ItemKind says how an item was made.
type ItemKind string

// The kinds of item.
const (
	ItemPin   ItemKind = "pin"   // an answer the engineer pinned
	ItemDraft ItemKind = "draft" // a draft written for an intention
)

// Item is an answer the engineer pinned or a draft written for them, kept
// for them to copy and never posted (docs/ARCHITECTURE.md §Data model).
type Item struct {
	// Seq numbers the item for the case; its name is i<Seq>.
	Seq          int64
	Kind         ItemKind
	Conversation string
	// Intention and Audience are a draft's, from the deployment's
	// configuration.
	Intention string
	Audience  string
	Text      string
	CreatedAt time.Time
}

// Name is the item's short name, i1, i2, ….
func (it Item) Name() string { return fmt.Sprintf("i%d", it.Seq) }

// AddItem keeps it and returns it numbered.
func (s Store) AddItem(ctx context.Context, it Item) (Item, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO companion_items (kind, conversation_id, intention, audience, text, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		string(it.Kind), it.Conversation, it.Intention, it.Audience, it.Text, it.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Item{}, fmt.Errorf("companion: keeping an item: %w", err)
	}
	if it.Seq, err = res.LastInsertId(); err != nil {
		return Item{}, fmt.Errorf("companion: keeping an item: %w", err)
	}
	return it, nil
}

// Items are the case's items, oldest first.
func (s Store) Items(ctx context.Context) ([]Item, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, kind, conversation_id, intention, audience, text, created_at FROM companion_items ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("companion: items: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var created string
		if err := rows.Scan(&it.Seq, &it.Kind, &it.Conversation, &it.Intention, &it.Audience, &it.Text, &created); err != nil {
			return nil, fmt.Errorf("companion: items: %w", err)
		}
		if it.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("companion: item time: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("companion: items: %w", err)
	}
	return out, nil
}

// LastAnswer is the text of a conversation's last model answer as the
// engineer was shown it — the editor's correction of it, or what was
// shown after a revision not taken, where there is one; a revision taken
// being the model's own answer — or empty where the model has not
// answered.
func (s Store) LastAnswer(ctx context.Context, conv string) (string, error) {
	msgs, err := s.Messages(ctx, conv)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if text, ok := Replacement(msgs[i]); ok {
			return text, nil
		}
		if msgs[i].Kind == KindModel && msgs[i].Text != "" {
			return msgs[i].Text, nil
		}
	}
	return "", nil
}
