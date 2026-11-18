package engine

import (
	"slices"
	"testing"
)

// The deployment's credential file names reach the runner every
// collection goes through.
func TestCredentialFilesReachTheRunner(t *testing.T) {
	names := []string{"auth/kubeconfig"}
	e, err := New(Options{Cases: StubCases("../testdata/cases"), Artifacts: &snapshotArtifacts{root: t.TempDir()}, Home: t.TempDir(), CredentialFiles: names})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.runner().CredentialFiles; !slices.Equal(got, names) {
		t.Errorf("runner's credential files %v", got)
	}
}
