package engine

var RoutingFacts = routingFacts

// StubCases is the stub case source over a directory of fixtures, set by
// stubhook_test.go: the engine's own tests cannot import engine/stubcases,
// which imports the engine, so an external test file hands it in, and the
// comment viewer's tests read testdata/cases through the real loader.
var StubCases func(dir string) CaseSource

// LocalDir is the local-directory artifact source over a directory of
// case folders, set by stubhook_test.go for the same reason.
var LocalDir func(base string) ArtifactSource
