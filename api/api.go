// Package api holds the versioned public schemas of the engine's contracts:
// the expert manifest, scope and tests, the tool catalog and its capture
// identity output, a test's fixture cases, the expert check report, the
// routing library and term dictionary, the engine configuration, the case
// ledger's update and the epistemic checks' findings. They are JSON Schema
// 2020-12 documents, embedded so the engine needs no files at runtime.
package api

import "embed"

// Schemas contains every schema file, at <area>/<version>/<kind>.schema.json.
//
//go:embed expert/v1alpha2/*.schema.json expert/v1alpha3/*.schema.json routing/v1alpha1/*.schema.json config/v1alpha1/*.schema.json ledger/v1alpha2/*.schema.json check/v1alpha1/*.schema.json
var Schemas embed.FS
