// Package engine is the public entry point to the compa engine: the
// domain-agnostic code that owns the case store, evidence, collection, the
// expert lifecycle and the companion's conversation with a model, whose
// every tool call it validates before it runs.
//
// The engine imports nothing but the standard library and the modules its
// go.mod requires: a deployment and an expert repository depend on it, never
// the other way. TestEngineImportsOnlyDeclaredDependencies enforces this.
package engine
