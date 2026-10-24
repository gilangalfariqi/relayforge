package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
)

func FuzzEventEnvelopeValidation(f *testing.F) {
	f.Add([]byte(`{"type":"order.created","schema_version":1,"occurred_at":"2026-10-22T10:14:00Z","data":{"order_id":"ord_1","amount":4200}}`))
	f.Add([]byte(`{"type":"BadType","data":[1,2,3]}`))
	f.Add([]byte(`{"type":"a.b","data":{},"extra":true}`))
	f.Add([]byte(`{"type":"a.b","data":null}`))
	f.Add([]byte(`{`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > 262144 {
			return
		}
		var env envelope
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&env); err != nil {
			return
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return
		}

		fields, _ := validateEnvelope(env, 5*time.Minute, 30*24*time.Hour)
		if len(fields) > 0 {
			return
		}
		if len(env.Data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(env.Data), []byte("{")) {
			return
		}
		if depth(env.Data) > 32 {
			return
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, env.Data); err != nil {
			return
		}
		if compact.Len() == 0 {
			t.Fatal("accepted empty compacted data")
		}
	})
}

func FuzzIdempotencyKeyValidation(f *testing.F) {
	f.Add("order-123-created-v1")
	f.Add("short")
	f.Add("bad key with spaces")
	f.Add("'; DROP TABLE events; --")
	f.Add("valid_key.2026:10:22-abcdef01")

	f.Fuzz(func(t *testing.T, key string) {
		matched := idempotencyKeyPattern.MatchString(key)
		if matched {
			if len(key) < 8 || len(key) > 255 {
				t.Fatalf("accepted key with out-of-bounds length %d", len(key))
			}
			for i := 0; i < len(key); i++ {
				c := key[i]
				valid := (c >= 'A' && c <= 'Z') ||
					(c >= 'a' && c <= 'z') ||
					(c >= '0' && c <= '9') ||
					c == '_' || c == '.' || c == ':' || c == '-'
				if !valid {
					t.Fatalf("accepted key containing invalid byte %q", c)
				}
			}
		}
	})
}

func FuzzCursorDecoder(f *testing.F) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := uuid.MustParse("0199a1c6-3a4b-7c2d-8e1f-5a6b7c8d9e0f")
	fh := filterHash("events", "accepted", "order.created")
	f.Add(encodeCursor(now, id, fh), fh)
	f.Add(encodeAttemptCursor(now, 42, fh), fh)
	f.Add("not-base64!!!", fh)
	f.Add("eyJ0IjoiMjAyNi0xMC0yMlQxMjowMDowMFoiLCJpZCI6bm90LWEtdXVpZCwaiI6ImZoIn0", fh)

	f.Fuzz(func(t *testing.T, raw, filter string) {
		if gotT, gotID, err := decodeCursor(raw, filter); err == nil {
			if gotID == uuid.Nil && gotT.IsZero() {
				t.Fatal("decodeCursor returned nil UUID and zero time without error")
			}
		}
		if _, gotSeq, err := decodeAttemptCursor(raw, filter); err == nil {
			if gotSeq <= 0 {
				t.Fatalf("decodeAttemptCursor accepted non-positive cursor ID %d", gotSeq)
			}
		}
	})
}

func TestFuzzBudgetedPropertySuite(t *testing.T) {
	const budgetPerTarget = 10000
	rng := rand.New(rand.NewPCG(20261023, 99182736))

	seeds := [][]byte{
		[]byte(`{"type":"order.created","schema_version":1,"occurred_at":"2026-10-22T10:14:00Z","data":{"order_id":"ord_1","amount":4200}}`),
		[]byte(`{"type":"BadType","data":[1,2,3]}`),
		[]byte(`{"type":"a.b","data":{},"extra":true}`),
		[]byte(`{"type":"a.b","data":null}`),
		[]byte(`{`),
	}

	for i := 0; i < budgetPerTarget; i++ {
		base := seeds[i%len(seeds)]
		buf := make([]byte, len(base))
		copy(buf, base)
		if len(buf) > 0 {
			mutations := 1 + rng.IntN(4)
			for m := 0; m < mutations; m++ {
				idx := rng.IntN(len(buf))
				buf[idx] = byte(rng.IntN(256))
			}
		}
		var env envelope
		dec := json.NewDecoder(bytes.NewReader(buf))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&env); err == nil {
			if err := dec.Decode(&struct{}{}); errors.Is(err, io.EOF) {
				fields, _ := validateEnvelope(env, 5*time.Minute, 30*24*time.Hour)
				if len(fields) == 0 && len(env.Data) > 0 && bytes.HasPrefix(bytes.TrimSpace(env.Data), []byte("{")) && depth(env.Data) <= 32 {
					var compact bytes.Buffer
					if err := json.Compact(&compact, env.Data); err == nil && compact.Len() == 0 {
						t.Fatal("accepted empty compacted data")
					}
				}
			}
		}
	}

	for i := 0; i < budgetPerTarget; i++ {
		n := rng.IntN(300)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(32 + rng.IntN(95))
		}
		key := string(b)
		if idempotencyKeyPattern.MatchString(key) {
			if len(key) < 8 || len(key) > 255 {
				t.Fatalf("accepted key with out-of-bounds length %d", len(key))
			}
		}
	}

	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := uuid.MustParse("0199a1c6-3a4b-7c2d-8e1f-5a6b7c8d9e0f")
	fh := filterHash("events", "accepted", "order.created")
	validCursor := encodeCursor(now, id, fh)
	for i := 0; i < budgetPerTarget; i++ {
		buf := []byte(validCursor)
		if len(buf) > 0 {
			idx := rng.IntN(len(buf))
			buf[idx] ^= byte(1 + rng.IntN(127))
		}
		raw := string(buf)
		if gotT, gotID, err := decodeCursor(raw, fh); err == nil {
			if gotID == uuid.Nil && gotT.IsZero() {
				t.Fatal("decodeCursor returned nil UUID and zero time without error")
			}
		}
		if _, gotSeq, err := decodeAttemptCursor(raw, fh); err == nil {
			if gotSeq <= 0 {
				t.Fatalf("decodeAttemptCursor accepted non-positive cursor ID %d", gotSeq)
			}
		}
	}
	t.Logf("executed %d budgeted fuzz iterations across 3 targets with 0 panics", budgetPerTarget*3)
}
