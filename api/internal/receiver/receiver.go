// Package receiver implements the reference Standard Webhooks v1 receiver used
// for end-to-end verification, failure-injection testing, and the interactive
// portfolio demo.
package receiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	MaxBodyBytes       = 262144
	TimestampTolerance = 300 * time.Second
)

// Behavior controls programmable responses for E2E and failure-injection tests.
type Behavior struct {
	Status      int    `json:"status"`
	FailFirstN  int    `json:"fail_first_n"`
	FailStatus  int    `json:"fail_status"`
	RetryAfter  string `json:"retry_after,omitempty"`
	DelayMS     int    `json:"delay_ms,omitempty"`
	RedirectURL string `json:"redirect_url,omitempty"`
}

// Stats summarizes receiver activity for E2E assertions.
type Stats struct {
	EffectsApplied    int      `json:"effects_applied"`
	DuplicatesIgnored int      `json:"duplicates_ignored"`
	AuthFailures      int      `json:"auth_failures"`
	TotalAttempts     int      `json:"total_attempts"`
	ReceivedIDs       []string `json:"received_ids"`
}

// Server is the reference webhook receiver HTTP handler.
type Server struct {
	mu                sync.Mutex
	secrets           [][32]byte
	behavior          Behavior
	failuresServed    int
	effectsApplied    int
	duplicatesIgnored int
	authFailures      int
	totalAttempts     int
	seenMem           map[string]bool
	receivedIDs       []string
	nowFunc           func() time.Time
	pool              *pgxpool.Pool
}

// New creates a new receiver Server with initial `whsec_...` secrets.
func New(initialSecrets []string, pool *pgxpool.Pool) (*Server, error) {
	s := &Server{
		behavior: Behavior{Status: http.StatusOK},
		seenMem:  make(map[string]bool),
		nowFunc:  time.Now,
		pool:     pool,
	}
	if err := s.SetSecrets(initialSecrets); err != nil {
		return nil, err
	}
	return s, nil
}

// EnsureSchema creates the durable idempotency table when backed by PostgreSQL.
func (s *Server) EnsureSchema(ctx context.Context) error {
	if s.pool == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS receiver_processed_webhooks (
			webhook_id     text PRIMARY KEY,
			delivery_id    text NOT NULL,
			event_type     text NOT NULL,
			payload_sha256 text NOT NULL,
			received_at    timestamptz NOT NULL DEFAULT now()
		)
	`)
	return err
}

// DecodeSecret decodes a `whsec_<base64>` secret into its 32-byte raw HMAC key.
func DecodeSecret(encoded string) ([32]byte, error) {
	var out [32]byte
	trimmed := strings.TrimSpace(encoded)
	if !strings.HasPrefix(trimmed, "whsec_") {
		return out, errors.New("secret must start with whsec_")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(trimmed, "whsec_"))
	if err != nil {
		return out, fmt.Errorf("decode base64 secret: %w", err)
	}
	if len(raw) != 32 {
		return out, fmt.Errorf("decoded secret must be 32 bytes, got %d", len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

// SignV1 computes a single Standard Webhooks `v1,<base64>` signature token.
func SignV1(rawSecret [32]byte, webhookID string, timestamp int64, rawBody []byte) string {
	mac := hmac.New(sha256.New, rawSecret[:])
	mac.Write([]byte(fmt.Sprintf("%s.%d.", webhookID, timestamp)))
	mac.Write(rawBody)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySignature verifies a Standard Webhooks `v1` signature header over the raw body
// (`contracts/signature-v1.md` §6).
func VerifySignature(
	secrets [][32]byte,
	webhookID string,
	webhookTimestamp string,
	signatureHeader string,
	rawBody []byte,
	now time.Time,
) error {
	if webhookID == "" || webhookTimestamp == "" || signatureHeader == "" {
		return errors.New("missing required webhook signature headers")
	}
	if strings.Contains(webhookID, ".") || strings.Contains(webhookTimestamp, ".") {
		return errors.New("header contains forbidden delimiter '.'")
	}
	tsSecs, err := strconv.ParseInt(webhookTimestamp, 10, 64)
	if err != nil {
		return errors.New("webhook-timestamp is not a base-10 integer")
	}
	skew := now.Sub(time.Unix(tsSecs, 0))
	if skew < -TimestampTolerance || skew > TimestampTolerance {
		return errors.New("webhook-timestamp outside 300s tolerance")
	}

	var v1CandidateTags [][]byte
	for _, entry := range strings.Fields(signatureHeader) {
		if !strings.HasPrefix(entry, "v1,") {
			continue
		}
		b64 := strings.TrimPrefix(entry, "v1,")
		tag, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(tag) != sha256.Size {
			continue
		}
		v1CandidateTags = append(v1CandidateTags, tag)
	}
	if len(v1CandidateTags) == 0 {
		return errors.New("no valid v1 signature entry present")
	}

	prefix := []byte(webhookID + "." + webhookTimestamp + ".")
	for _, key := range secrets {
		mac := hmac.New(sha256.New, key[:])
		mac.Write(prefix)
		mac.Write(rawBody)
		expected := mac.Sum(nil)
		for _, candidate := range v1CandidateTags {
			if hmac.Equal(expected, candidate) {
				return nil
			}
		}
	}
	return errors.New("signature mismatch")
}

// SetSecrets replaces the active verification secrets.
func (s *Server) SetSecrets(encoded []string) error {
	decoded := make([][32]byte, 0, len(encoded))
	for _, item := range encoded {
		if strings.TrimSpace(item) == "" {
			continue
		}
		k, err := DecodeSecret(item)
		if err != nil {
			return err
		}
		decoded = append(decoded, k)
	}
	s.mu.Lock()
	s.secrets = decoded
	s.mu.Unlock()
	return nil
}

// Handler returns an http.Handler exposing `/webhook`, `/healthz`, and `/control/*`.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/webhook", s.handleWebhook)
	mux.HandleFunc("/control/secrets", s.handleControlSecrets)
	mux.HandleFunc("/control/behavior", s.handleControlBehavior)
	mux.HandleFunc("/control/stats", s.handleControlStats)
	mux.HandleFunc("/control/reset", s.handleControlReset)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read_failed"})
		return
	}
	if len(rawBody) > MaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large"})
		return
	}

	s.mu.Lock()
	secretsCopy := append([][32]byte(nil), s.secrets...)
	now := s.nowFunc()
	s.totalAttempts++
	s.mu.Unlock()

	webhookID := r.Header.Get("webhook-id")
	webhookTS := r.Header.Get("webhook-timestamp")
	webhookSig := r.Header.Get("webhook-signature")

	if err := VerifySignature(secretsCopy, webhookID, webhookTS, webhookSig, rawBody, now); err != nil {
		s.mu.Lock()
		s.authFailures++
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	s.mu.Lock()
	beh := s.behavior
	shouldFailTransient := false
	if beh.FailFirstN > 0 && s.failuresServed < beh.FailFirstN {
		s.failuresServed++
		shouldFailTransient = true
	}
	s.mu.Unlock()

	if beh.DelayMS > 0 {
		select {
		case <-time.After(time.Duration(beh.DelayMS) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}

	if beh.RedirectURL != "" {
		w.Header().Set("Location", beh.RedirectURL)
		status := beh.Status
		if status < 300 || status >= 400 {
			status = http.StatusFound
		}
		w.WriteHeader(status)
		return
	}

	if shouldFailTransient {
		failCode := beh.FailStatus
		if failCode == 0 {
			failCode = http.StatusInternalServerError
		}
		if beh.RetryAfter != "" {
			w.Header().Set("Retry-After", beh.RetryAfter)
		}
		writeJSON(w, failCode, map[string]string{"status": "simulated_transient_failure"})
		return
	}

	targetStatus := beh.Status
	if targetStatus == 0 {
		targetStatus = http.StatusOK
	}
	if targetStatus >= 300 {
		if beh.RetryAfter != "" {
			w.Header().Set("Retry-After", beh.RetryAfter)
		}
		writeJSON(w, targetStatus, map[string]string{"status": "configured_error"})
		return
	}

	// Step 9 (`contracts/signature-v1.md` §6): durable deduplication keyed by `webhook-id`.
	deliveryID := r.Header.Get("RelayForge-Delivery")
	eventType := r.Header.Get("RelayForge-Event-Type")
	sum := sha256.Sum256(rawBody)
	hashHex := hex.EncodeToString(sum[:])

	isNew, err := s.recordIdempotentEffect(r.Context(), webhookID, deliveryID, eventType, hashHex)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_error"})
		return
	}

	s.mu.Lock()
	if isNew {
		s.effectsApplied++
		s.receivedIDs = append(s.receivedIDs, webhookID)
	} else {
		s.duplicatesIgnored++
	}
	s.mu.Unlock()

	if targetStatus == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if isNew {
		writeJSON(w, targetStatus, map[string]string{"status": "processed", "webhook_id": webhookID})
	} else {
		writeJSON(w, targetStatus, map[string]string{"status": "duplicate_ignored", "webhook_id": webhookID})
	}
}

func (s *Server) recordIdempotentEffect(
	ctx context.Context,
	webhookID, deliveryID, eventType, payloadSHA256 string,
) (bool, error) {
	if s.pool != nil {
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO receiver_processed_webhooks (webhook_id, delivery_id, event_type, payload_sha256)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (webhook_id) DO NOTHING
		`, webhookID, deliveryID, eventType, payloadSHA256)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenMem[webhookID] {
		return false, nil
	}
	s.seenMem[webhookID] = true
	return true, nil
}

func (s *Server) handleControlSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req struct {
		Secrets []string `json:"secrets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if err := s.SetSecrets(req.Secrets); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "count": len(req.Secrets)})
}

func (s *Server) handleControlBehavior(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	var b Behavior
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if b.Status == 0 {
		b.Status = http.StatusOK
	}
	s.mu.Lock()
	s.behavior = b
	s.failuresServed = 0
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleControlStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := Stats{
		EffectsApplied:    s.effectsApplied,
		DuplicatesIgnored: s.duplicatesIgnored,
		AuthFailures:      s.authFailures,
		TotalAttempts:     s.totalAttempts,
		ReceivedIDs:       append([]string(nil), s.receivedIDs...),
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleControlReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if s.pool != nil {
		_, _ = s.pool.Exec(r.Context(), `TRUNCATE receiver_processed_webhooks`)
	}
	s.mu.Lock()
	s.behavior = Behavior{Status: http.StatusOK}
	s.failuresServed = 0
	s.effectsApplied = 0
	s.duplicatesIgnored = 0
	s.authFailures = 0
	s.totalAttempts = 0
	s.seenMem = make(map[string]bool)
	s.receivedIDs = nil
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
