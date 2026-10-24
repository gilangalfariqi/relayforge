package jsonschema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAcceptsAndRejects(t *testing.T) {
	v := New()
	raw := []byte(`{"type":"object","required":["amount"],"properties":{"amount":{"type":"integer","minimum":1}},"additionalProperties":false}`)
	var data any
	if err := json.Unmarshal([]byte(`{"amount":42}`), &data); err != nil {
		t.Fatal(err)
	}
	errs, err := v.Validate(raw, data)
	if err != nil || len(errs) != 0 {
		t.Fatalf("valid payload: err=%v fieldErrors=%v", err, errs)
	}
	data = map[string]any{"amount": "lots", "extra": true}
	errs, err = v.Validate(raw, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) == 0 {
		t.Fatal("invalid payload accepted")
	}
	for _, e := range errs {
		if !strings.HasPrefix(e.Pointer, "/data") {
			t.Fatalf("pointer = %q", e.Pointer)
		}
	}
}

func TestCompileRejectsExternalRef(t *testing.T) {
	if err := Compile([]byte(`{"$ref":"https://example.com/schema.json"}`)); err == nil {
		t.Fatal("external ref accepted")
	}
	if err := Compile([]byte(`{"type":"object","properties":{"a":{"type":"string"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := Compile([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestCompileRejectsInvalidSchema(t *testing.T) {
	if err := Compile([]byte(`{"type":"not-a-type"}`)); err == nil {
		t.Fatal("invalid schema type accepted")
	}
}
