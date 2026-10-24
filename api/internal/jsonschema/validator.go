// Package jsonschema validates tenant event schemas and payloads.
package jsonschema

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// FieldError is a client-safe validation failure.
type FieldError struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
}

// Validator compiles and caches schema documents by content hash.
type Validator struct {
	mu    sync.Mutex
	cache map[[32]byte]*jsonschema.Schema
}

// New returns an empty validator.
func New() *Validator {
	return &Validator{cache: make(map[[32]byte]*jsonschema.Schema)}
}

// Compile checks that raw is a usable JSON Schema without external refs.
func Compile(raw []byte) error {
	if len(raw) == 0 || len(raw) > 65536 {
		return errors.New("schema must be between 1 and 65536 bytes")
	}
	if !json.Valid(raw) {
		return errors.New("schema is not valid JSON")
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return errors.New("schema is not valid JSON")
	}
	if hasExternalRef(probe) {
		return errors.New("external $ref is not supported")
	}
	_, err := compile(raw)
	return err
}

// Validate compiles/caches schema and validates decoded data.
func (v *Validator) Validate(schemaRaw []byte, data any) ([]FieldError, error) {
	key := sha256.Sum256(schemaRaw)
	v.mu.Lock()
	sch, ok := v.cache[key]
	v.mu.Unlock()
	if !ok {
		var err error
		sch, err = compile(schemaRaw)
		if err != nil {
			return nil, fmt.Errorf("compile stored schema: %w", err)
		}
		v.mu.Lock()
		v.cache[key] = sch
		v.mu.Unlock()
	}
	err := sch.Validate(data)
	if err == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	var out []FieldError
	flatten(ve, &out)
	if len(out) == 0 {
		out = append(out, FieldError{Pointer: "/data", Code: "schema", Detail: "payload does not match the registered schema"})
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

func compile(raw []byte) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	if err := compiler.AddResource("schema.json", bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return compiler.Compile("schema.json")
}

func flatten(ve *jsonschema.ValidationError, out *[]FieldError) {
	if len(ve.Causes) == 0 {
		ptr := ve.InstanceLocation
		if ptr == "" {
			ptr = "/data"
		} else {
			ptr = "/data" + ptr
		}
		*out = append(*out, FieldError{
			Pointer: ptr,
			Code:    errorCode(ve.KeywordLocation),
			Detail:  sanitize(ve.Message),
		})
		return
	}
	for _, c := range ve.Causes {
		flatten(c, out)
	}
}

func errorCode(keyword string) string {
	parts := strings.Split(keyword, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" && parts[i] != "properties" && parts[i] != "items" && parts[i] != "additionalProperties" {
			return parts[i]
		}
	}
	return "schema"
}

func sanitize(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, s)
}

func hasExternalRef(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "$ref" {
				if s, ok := val.(string); ok && (strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "file:")) {
					return true
				}
			}
			if hasExternalRef(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if hasExternalRef(val) {
				return true
			}
		}
	}
	return false
}
