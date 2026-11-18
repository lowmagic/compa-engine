package engine_test

import (
	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/stubcases"
)

func init() {
	engine.StubCases = func(dir string) engine.CaseSource { return stubcases.New(dir) }
	engine.LocalDir = func(base string) engine.ArtifactSource { return localdir.New(base) }
}
