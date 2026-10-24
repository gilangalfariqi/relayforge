package auth

import (
	"strings"
	"testing"
)

func TestGenerateParseAndVerify(t *testing.T) {
	pepper := []byte("01234567890123456789012345678901")
	k, digest, err := GenerateKey("live", pepper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Plaintext, "rf_live_") {
		t.Fatalf("plaintext = %q", k.Plaintext)
	}
	if len(digest) != 32 {
		t.Fatalf("digest length = %d", len(digest))
	}
	parsed, err := ParseKey(k.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.KeyID != k.KeyID || parsed.Env != "live" {
		t.Fatalf("parsed = %+v, want key id %s", parsed, k.KeyID)
	}
	if !VerifyDigest(pepper, parsed.Secret, digest) {
		t.Fatal("digest did not verify")
	}
	if VerifyDigest(pepper, []byte("wrong"), digest) {
		t.Fatal("wrong secret verified")
	}
}

func TestParseKeyRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"rf_live_short_secret",
		"bearer rf_live_aaaaaaaaaaaa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"rf_live_aaaaaaaaaaaa_short",
		"rf_live_aaaaaaaaaaa!_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"rf_live_productiontoolong_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for _, tc := range cases {
		if _, err := ParseKey(tc); err == nil {
			t.Fatalf("ParseKey(%q) succeeded", tc)
		}
	}
}

func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		role, action string
		want         bool
	}{
		{"owner", "credentials.write", true},
		{"admin", "credentials.write", false},
		{"ingest", "events.write", true},
		{"viewer", "events.write", false},
		{"viewer", "events.read", true},
		{"ingest", "endpoints.write", false},
		{"admin", "secrets.write", true},
		{"viewer", "audit.read", true},
		{"ingest", "audit.read", false},
	}
	for _, tc := range cases {
		if got := Can(tc.role, tc.action); got != tc.want {
			t.Fatalf("Can(%q,%q)=%v want %v", tc.role, tc.action, got, tc.want)
		}
	}
}
