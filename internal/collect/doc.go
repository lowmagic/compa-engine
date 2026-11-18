// Package collect runs collection: the engine's built-in read-only
// primitives, argv commands in a sandbox, and the pipelines expert tests are
// made of (docs/COLLECTION.md). Only the engine collects; a model asks for
// collection with a tool call and never runs anything itself.
package collect
