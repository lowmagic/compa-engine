package collect

import (
	"os"
	"testing"
)

// SkipWithoutSandbox skips a test for want of the platform's sandbox, or
// of a part of it, saying why, unless COMPA_REQUIRE_SANDBOX is set, as it
// is where one must exist, and then the test fails. It is exported for
// this directory's external tests.
func SkipWithoutSandbox(t *testing.T, why any) {
	t.Helper()
	if os.Getenv("COMPA_REQUIRE_SANDBOX") != "" {
		t.Fatal(why)
	}
	t.Skip(why)
}
