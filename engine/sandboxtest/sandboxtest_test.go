package sandboxtest

import (
	"errors"
	"os"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
)

// The platform's own sandbox keeps the contract. Where the platform has
// none, the case is skipped, unless COMPA_REQUIRE_SANDBOX is set, as it is
// where one must exist.
func TestDefaultSandbox(t *testing.T) {
	sb, err := engine.DefaultSandbox()
	if errors.Is(err, engine.ErrNoSandbox) && os.Getenv("COMPA_REQUIRE_SANDBOX") == "" {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	TestSandbox(t, sb)
}
