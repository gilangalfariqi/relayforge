package openapi

import (
	"bytes"
	"testing"
)

func TestSpecIsEmbedded(t *testing.T) {
	if len(Spec) == 0 {
		t.Fatal("embedded OpenAPI document is empty")
	}
	if !bytes.HasPrefix(Spec, []byte("openapi: 3.1.0")) {
		t.Fatalf("unexpected document prefix: %q", Spec[:min(32, len(Spec))])
	}
}
