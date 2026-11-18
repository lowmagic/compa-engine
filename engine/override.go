package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The engineer's override. A refusal because the check could not be made,
// an unreadable list or a case whose source gave no account number, can
// be lifted for one case by the engineer, after the deployment's
// instructions, by typing the account's name; a match on the list cannot
// be. It lasts until withdrawn or until the check can be made, when the
// ordinary check decides.

// metaModelsOverride is the store's meta key holding the override, as
// JSON; empty or unset where there is none.
const metaModelsOverride = "models_override"

// ModelsOverride is the engineer's override of a check that could not be
// made: who made it, when, and the reason the check could not be made
// then.
type ModelsOverride struct {
	ID  string    `json:"id"`
	Who string    `json:"who"`
	At  time.Time `json:"at"`
	// Reason is why the check could not be made when the override was
	// made.
	Reason string `json:"reason"`
	// Confirmed says the instructions were shown and the account's name
	// typed.
	Confirmed bool `json:"confirmed"`
}

// Line is what the case's heading and the command line say of the override.
func (o ModelsOverride) Line() string {
	return fmt.Sprintf("models allowed by override (%s, %s)", o.Who, o.At.UTC().Format("2006-01-02 15:04 UTC"))
}

// readOverride reads the case's override, nil where there is none.
func readOverride(ctx context.Context, c *openCase) (*ModelsOverride, error) {
	v, ok, err := c.st.Meta(ctx, metaModelsOverride)
	if err != nil || !ok || v == "" {
		return nil, err
	}
	var o ModelsOverride
	if err := json.Unmarshal([]byte(v), &o); err != nil {
		return nil, fmt.Errorf("the case's override could not be read: %w", err)
	}
	return &o, nil
}

var (
	errNoBarredList     = errors.New("no list of accounts barred from AI processing is configured")
	errModelsNotRefused = errors.New("model calls are not refused for this case")
	errListMatch        = errors.New("this case's account is on the list; a match cannot be overridden")
	errNameMismatch     = errors.New("that is not the account's name; nothing changed")
)

// ErrNoOverride is the refusal to withdraw an override a case does not have.
var ErrNoOverride = errors.New("this case has no override to withdraw")

// overrideNotice is what the engineer reads before an override: why the
// check could not be made, the deployment's instructions, and how to
// confirm. An error says why there is nothing to override.
func (e *Engine) overrideNotice(ctx context.Context, c *openCase) ([]string, error) {
	if e.config.BarredAccountsPath() == "" {
		return nil, errNoBarredList
	}
	d := e.barredDecision(ctx, c)
	switch {
	case d.refused && !d.unchecked:
		return nil, errListMatch
	case !d.refused:
		return nil, errModelsNotRefused
	}
	name := caseFacts(c).Account.Name
	confirm := "type the account's name, " + name
	if name == "" {
		confirm = "the case's source gave no account name; type the case number, " + c.id
	}
	out := []string{"compa could not check this case's account against the list of accounts barred from AI processing: " + d.reason, ""}
	if in := e.config.Reasoning.BarredAccounts.OverrideInstructions; in != "" {
		out = append(out, in, "")
	}
	return append(out, "To allow model calls for this case, "+confirm+"."), nil
}

// allowModels records the engineer's override when typed is the account's
// name as the source gave it, or the case number where it gave none.
func (e *Engine) allowModels(ctx context.Context, c *openCase, typed, who string) (ModelsOverride, error) {
	if _, err := e.overrideNotice(ctx, c); err != nil {
		return ModelsOverride{}, err
	}
	want := caseFacts(c).Account.Name
	if want == "" {
		want = c.id
	}
	if strings.TrimSpace(typed) != want {
		return ModelsOverride{}, errNameMismatch
	}
	d := e.barredDecision(ctx, c)
	o := ModelsOverride{ID: c.newID("override"), Who: who, At: c.now().UTC(), Reason: d.reason, Confirmed: true}
	j, err := json.Marshal(o)
	if err != nil {
		return ModelsOverride{}, err
	}
	if err := c.st.SetMeta(ctx, metaModelsOverride, string(j)); err != nil {
		return ModelsOverride{}, err
	}
	return o, nil
}

// revokeModels withdraws the case's override.
func revokeModels(ctx context.Context, c *openCase) error {
	o, err := readOverride(ctx, c)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrNoOverride
	}
	if err := c.st.SetMeta(ctx, metaModelsOverride, ""); err != nil {
		return err
	}
	return nil
}
