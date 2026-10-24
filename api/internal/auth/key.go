// Package auth implements API key material, authentication principals, and
// role checks. It deliberately has no database dependency so the rules can be
// unit-tested in isolation.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	keyIDBytes  = 8  // base32 without padding -> 13 chars; truncated to 12
	secretBytes = 32 // base32 without padding -> 52 chars
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// Key is a parsed API key. Plaintext is present only while handling the
// request that carried it and must never be logged or persisted.
type Key struct {
	Env       string
	KeyID     string
	Secret    []byte
	Plaintext string
}

// Principal is the authenticated caller attached to a request.
type Principal struct {
	TenantID     uuid.UUID
	CredentialID uuid.UUID
	KeyID        string
	Role         string
	TenantStatus string
}

// GenerateKey returns a new key and the HMAC digest to store. The plaintext
// is returned exactly once to the caller.
func GenerateKey(env string, pepper []byte) (Key, []byte, error) {
	rawID := make([]byte, keyIDBytes)
	rawSecret := make([]byte, secretBytes)
	if _, err := rand.Read(rawID); err != nil {
		return Key{}, nil, fmt.Errorf("generate key id: %w", err)
	}
	if _, err := rand.Read(rawSecret); err != nil {
		return Key{}, nil, fmt.Errorf("generate key secret: %w", err)
	}
	keyID := strings.ToLower(base32NoPad.EncodeToString(rawID))[:12]
	plaintext := fmt.Sprintf("rf_%s_%s_%s", env, keyID, base32NoPad.EncodeToString(rawSecret))
	k := Key{Env: env, KeyID: keyID, Secret: rawSecret, Plaintext: plaintext}
	return k, Digest(pepper, rawSecret), nil
}

// ParseKey validates the wire format and decodes the secret bytes.
func ParseKey(value string) (Key, error) {
	if len(value) < 10 || len(value) > 128 {
		return Key{}, errors.New("invalid api key length")
	}
	parts := strings.Split(value, "_")
	if len(parts) != 4 || parts[0] != "rf" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return Key{}, errors.New("invalid api key format")
	}
	if len(parts[1]) > 8 {
		return Key{}, errors.New("invalid api key environment")
	}
	if len(parts[2]) != 12 {
		return Key{}, errors.New("invalid api key id")
	}
	if len(parts[3]) != 52 {
		return Key{}, errors.New("invalid api key secret")
	}
	for _, r := range parts[2] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return Key{}, errors.New("invalid api key id")
		}
	}
	secret, err := base32NoPad.DecodeString(strings.ToUpper(parts[3]))
	if err != nil || len(secret) != secretBytes {
		return Key{}, errors.New("invalid api key secret")
	}
	return Key{Env: parts[1], KeyID: parts[2], Secret: secret, Plaintext: value}, nil
}

// Digest computes HMAC-SHA256(pepper, secret). The digest is the only key
// material persisted.
func Digest(pepper, secret []byte) []byte {
	m := hmac.New(sha256.New, pepper)
	m.Write(secret)
	return m.Sum(nil)
}

// VerifyDigest compares a candidate secret with a stored digest in constant
// time.
func VerifyDigest(pepper, secret, want []byte) bool {
	got := Digest(pepper, secret)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyDigest is used when a key ID is unknown so that lookup failures and
// digest mismatches take a comparable amount of work in the common case.
func DummyDigest(pepper []byte) []byte {
	return Digest(pepper, make([]byte, secretBytes))
}

// Can reports whether a role may perform an action.
func Can(role, action string) bool {
	switch action {
	case "events.write":
		return role == "owner" || role == "admin" || role == "ingest"
	case "endpoints.write", "secrets.write", "replay":
		return role == "owner" || role == "admin"
	case "endpoints.read", "events.read", "deliveries.read", "schemas.read", "tenant.read":
		return role == "owner" || role == "admin" || role == "ingest" || role == "viewer"
	case "credentials.write", "schemas.write":
		return role == "owner"
	case "tenant.write":
		return role == "owner"
	case "audit.read":
		return role == "owner" || role == "admin" || role == "viewer"
	default:
		return false
	}
}
