// Package openapi embeds the API contract served at /api/v1/openapi.yaml.
package openapi

import _ "embed"

// Spec is the OpenAPI 3.1 document. It is the canonical machine-readable
// contract; handlers and tests must not drift from it.
//
//go:embed openapi.yaml
var Spec []byte
