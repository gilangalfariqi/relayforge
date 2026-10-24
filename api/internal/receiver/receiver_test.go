package receiver

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	publishedEventID   = "msg_01J9Z6Q3V8T7R2M4N5P6Q7R8S9"
	publishedTimestamp = "1700000000"
	publishedBody      = `{"type":"order.created","data":{"id":"ord_123","amount":4200}}`
	publishedSig       = "v1,zWH/4qANrUYMGmZOIkwS0phhFBRxMelsoEkhyx2G71Q="
)

func publishedVectorKey() string {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(raw[:])
}

func TestPublishedSignatureV1VectorAndTamperRejection(t *testing.T) {
	srv, err := New([]string{publishedVectorKey()}, nil)
	if err != nil {
		t.Fatalf("New receiver: %v", err)
	}
	fixedNow := time.Unix(1700000100, 0)
	srv.nowFunc = func() time.Time { return fixedNow }
	h := srv.Handler()

	// 1. Valid vector succeeds and applies effect once.
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(publishedBody))
	req.Header.Set("webhook-id", publishedEventID)
	req.Header.Set("webhook-timestamp", publishedTimestamp)
	req.Header.Set("webhook-signature", publishedSig)
	req.Header.Set("RelayForge-Delivery", "deliv_1")
	req.Header.Set("RelayForge-Event-Type", "order.created")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d (%s)", rec.Code, rec.Body.String())
	}
	if srv.effectsApplied != 1 || srv.duplicatesIgnored != 0 {
		t.Fatalf("unexpected stats after first delivery: applied=%d dup=%d", srv.effectsApplied, srv.duplicatesIgnored)
	}

	// 2. Duplicate delivery of same webhook-id returns 200 without repeating the effect.
	reqDup := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(publishedBody))
	reqDup.Header.Set("webhook-id", publishedEventID)
	reqDup.Header.Set("webhook-timestamp", publishedTimestamp)
	reqDup.Header.Set("webhook-signature", publishedSig)

	recDup := httptest.NewRecorder()
	h.ServeHTTP(recDup, reqDup)
	if recDup.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on duplicate, got %d", recDup.Code)
	}
	if srv.effectsApplied != 1 || srv.duplicatesIgnored != 1 {
		t.Fatalf("expected duplicate to be ignored without repeating effect: applied=%d dup=%d", srv.effectsApplied, srv.duplicatesIgnored)
	}

	// 3. One-byte body tamper fails with 401.
	tamperedBody := `{"type":"order.created","data":{"id":"ord_123","amount":4201}}`
	reqTamper := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(tamperedBody))
	reqTamper.Header.Set("webhook-id", "msg_tampered")
	reqTamper.Header.Set("webhook-timestamp", publishedTimestamp)
	reqTamper.Header.Set("webhook-signature", publishedSig)

	recTamper := httptest.NewRecorder()
	h.ServeHTTP(recTamper, reqTamper)
	if recTamper.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on tampered body, got %d", recTamper.Code)
	}

	// 4. Stale timestamp (> 300s skew) fails with 401.
	srv.nowFunc = func() time.Time { return time.Unix(1700000305, 0) }
	reqStale := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(publishedBody))
	reqStale.Header.Set("webhook-id", publishedEventID)
	reqStale.Header.Set("webhook-timestamp", publishedTimestamp)
	reqStale.Header.Set("webhook-signature", publishedSig)

	recStale := httptest.NewRecorder()
	h.ServeHTTP(recStale, reqStale)
	if recStale.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on stale timestamp, got %d", recStale.Code)
	}
}
