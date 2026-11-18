package invocations

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lowmagic/compa-engine/internal/model"
)

// Summary is the attribution and usage a saved answer displays, without
// loading its potentially large retained request and response bodies.
type Summary struct {
	Model ModelIdentity `json:"model"`
	Usage model.Usage   `json:"usage"`
	Cache *model.Cache  `json:"cache,omitempty"`
	Turn  int           `json:"turn"`
}

// GetSummary reads only the metadata needed to display a saved answer.
func GetSummary(db Queryer, id string) (Summary, error) {
	var summary Summary
	var raw string
	err := db.QueryRow(`SELECT json_object(
		'model', json_extract(record, '$.model'),
		'usage', json_extract(record, '$.usage'),
		'cache', json_extract(record, '$.cache'),
		'turn', json_extract(record, '$.turn'))
		FROM invocations WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return summary, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return summary, fmt.Errorf("invocations: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		return summary, fmt.Errorf("invocations: %s: %w", id, err)
	}
	return summary, nil
}
