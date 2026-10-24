//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/relayforge/relayforge/api/internal/config"
	"github.com/relayforge/relayforge/api/internal/db"
	"github.com/relayforge/relayforge/api/internal/jsonschema"
	"github.com/relayforge/relayforge/api/internal/observability"
	"github.com/relayforge/relayforge/api/internal/receiver"
	"github.com/relayforge/relayforge/api/internal/secrets"
	"github.com/relayforge/relayforge/api/internal/ssrf"
	"github.com/relayforge/relayforge/api/internal/store"
)

const (
	testPepperB64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	testKEKB64    = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

type testEnv struct {
	t      *testing.T
	router *gin.Engine
	admin  *gin.Engine
	pool   *pgxpool.Pool
	lock   *pgxpool.Conn
	cfg    config.Config
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := os.Getenv("RELAYFORGE_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://relayforge:relayforge_local_only@localhost:55432/relayforge?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn, 30)
	if err != nil {
		t.Skipf("integration database unavailable: %v", err)
	}
	lock, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(918273645)`); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `TRUNCATE audit_logs, delivery_attempts, deliveries,
		idempotency_records, event_schemas, endpoint_secrets, endpoints,
		api_credentials, rate_limit_counters, events, tenants RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	pepper, _ := base64.StdEncoding.DecodeString(testPepperB64)
	kek, _ := base64.StdEncoding.DecodeString(testKEKB64)
	cfg := config.Config{
		Env: "test", HTTPAddr: ":18080", AdminAddr: ":19090", DatabaseURL: dsn,
		LogLevel: "error", ShutdownTimeout: time.Second, ReadinessTimeout: 2 * time.Second,
		ExpectedSchemaVersion: 5, MigrationsDir: "migrations", AdminToken: "test-admin-token",
		DBMaxConns: 30, APIKeyPepper: pepper, KeyEnv: "live", KEK: kek, KEKVersion: 1,
		SignupMode: "open", MaxEventBytes: 262144, EndpointLimit: 50,
		IdempotencyTTL: 24 * time.Hour, FutureSkew: 5 * time.Minute, OccurredPast: 30 * 24 * time.Hour,
		RateLimitIngestPerSec: 500, RateLimitManagementPerSec: 500, RateLimitAdminPerSec: 500,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	box, err := secrets.New(kek, 1)
	if err != nil {
		t.Fatal(err)
	}
	st := &store.Store{
		Pool: pool, ExpectedSchemaVersion: 5, Box: box, EndpointLimit: 50,
		IdempotencyTTL: 24 * time.Hour, EventFutureSkew: 5 * time.Minute, EventPastWindow: 30 * 24 * time.Hour,
	}
	deps := Deps{
		Config: cfg, Checker: &db.Store{Pool: pool, ExpectedSchemaVersion: 5},
		Metrics: observability.NewMetrics("test", "test"),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Store:   st, Schemas: jsonschema.New(), SSRF: ssrf.New(),
	}
	return &testEnv{t: t, router: NewRouter(deps), admin: NewAdminRouter(deps), pool: pool, lock: lock, cfg: cfg}
}

func (e *testEnv) close() {
	e.lock.Release()
	e.pool.Close()
}

func (e *testEnv) request(method, path string, body any, key string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *testEnv) adminRequest(method, path string, body any) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", e.cfg.AdminToken)
	w := httptest.NewRecorder()
	e.admin.ServeHTTP(w, req)
	return w
}

func (e *testEnv) signup(name string) (store.Tenant, string) {
	e.t.Helper()
	w := e.request(http.MethodPost, "/api/v1/tenants", map[string]any{"name": name}, "", nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("signup status = %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Tenant store.Tenant `json:"tenant"`
		APIKey string       `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	if out.APIKey == "" || !strings.HasPrefix(out.APIKey, "rf_live_") {
		e.t.Fatalf("bad api key %q", out.APIKey)
	}
	return out.Tenant, out.APIKey
}

func (e *testEnv) createEndpoint(key, name, url string) store.NewEndpoint {
	e.t.Helper()
	w := e.request(http.MethodPost, "/api/v1/endpoints", map[string]any{
		"name": name, "url": url, "event_types": []string{"order.created"},
	}, key, nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create endpoint status = %d body=%s", w.Code, w.Body.String())
	}
	var out store.NewEndpoint
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *testEnv) ingest(key, idem, body string) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/v1/events", body, key, map[string]string{"Idempotency-Key": idem})
}

func eventBody(id string) string {
	return fmt.Sprintf(`{"type":"order.created","schema_version":1,"data":{"order_id":%q,"amount":4200}}`, id)
}

func TestSignupAuthAndKeyStorage(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	tenant, key := e.signup("Acme Integration")
	if tenant.Slug == "" {
		t.Fatal("empty slug")
	}
	w := e.request(http.MethodGet, "/api/v1/tenant", nil, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant status = %d body=%s", w.Code, w.Body.String())
	}
	w = e.request(http.MethodGet, "/api/v1/tenant", nil, "rf_live_aaaaaaaaaaaa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key status = %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
	var digest []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT secret_hmac FROM api_credentials WHERE tenant_id=$1`, tenant.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest) != 32 {
		t.Fatalf("digest length = %d", len(digest))
	}
	var leaked int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%'||$1||'%'`, key).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("api key leaked into audit metadata")
	}
}

func TestTenantIsolationMatrix(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, keyA := e.signup("Tenant A")
	_, keyB := e.signup("Tenant B")
	ep := e.createEndpoint(keyA, "A endpoint", "https://1.1.1.1/hook")
	ev := e.ingest(keyA, "iso-event-0001", eventBody("a-1"))
	if ev.Code != http.StatusCreated {
		t.Fatalf("ingest = %d body=%s", ev.Code, ev.Body.String())
	}
	var evResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(ev.Body.Bytes(), &evResp)
	del := e.request(http.MethodGet, "/api/v1/events/"+evResp.ID, nil, keyA, nil)
	var detail store.EventDetail
	_ = json.Unmarshal(del.Body.Bytes(), &detail)
	if len(detail.Deliveries) != 1 {
		t.Fatalf("deliveries = %d", len(detail.Deliveries))
	}
	deliveryID := detail.Deliveries[0].ID.String()

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/endpoints/" + ep.ID.String(), nil},
		{http.MethodPatch, "/api/v1/endpoints/" + ep.ID.String(), map[string]any{"name": "stolen"}},
		{http.MethodDelete, "/api/v1/endpoints/" + ep.ID.String(), nil},
		{http.MethodGet, "/api/v1/endpoints/" + ep.ID.String() + "/secrets", nil},
		{http.MethodPost, "/api/v1/endpoints/" + ep.ID.String() + "/secrets", map[string]any{"overlap_seconds": 0}},
		{http.MethodDelete, "/api/v1/endpoints/" + ep.ID.String() + "/secrets/" + ep.SigningSecretID.String(), nil},
		{http.MethodGet, "/api/v1/events/" + evResp.ID, nil},
		{http.MethodPost, "/api/v1/events/" + evResp.ID + "/replay", map[string]any{"reason": "cross-tenant"}},
		{http.MethodGet, "/api/v1/deliveries/" + deliveryID, nil},
		{http.MethodGet, "/api/v1/deliveries/" + deliveryID + "/attempts", nil},
		{http.MethodPost, "/api/v1/credentials/" + ep.ID.String() + "/rotate", map[string]any{"overlap_seconds": 0}},
		{http.MethodDelete, "/api/v1/credentials/" + ep.ID.String(), nil},
	}
	for _, tc := range cases {
		w := e.request(tc.method, tc.path, tc.body, keyB, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status = %d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	// Lists are scoped.
	w := e.request(http.MethodGet, "/api/v1/endpoints", nil, keyB, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), ep.ID.String()) {
		t.Fatalf("B endpoint list leaked A: %s", w.Body.String())
	}
	// Schema isolation.
	w = e.request(http.MethodPost, "/api/v1/schemas", map[string]any{
		"event_type": "isolated.event", "version": 1,
		"schema": map[string]any{"type": "object"},
	}, keyA, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("schema create = %d %s", w.Code, w.Body.String())
	}
	var schema struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &schema)
	w = e.request(http.MethodDelete, "/api/v1/schemas/"+schema.ID, nil, keyB, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant schema delete = %d", w.Code)
	}
}

func TestConcurrentIdempotentIngest(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("Concurrent")
	e.createEndpoint(key, "endpoint", "https://1.1.1.1/concurrent")
	body := eventBody("concurrent-1")
	const n = 100
	statuses := make([]int, n)
	bodies := make([]string, n)
	replayed := make([]string, n)
	start := make(chan struct{})
	var ready, wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ready.Add(1)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			w := e.ingest(key, "concurrent-key-0001", body)
			statuses[i] = w.Code
			bodies[i] = w.Body.String()
			replayed[i] = w.Header().Get("Idempotency-Replayed")
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()
	replayCount := 0
	for i := 0; i < n; i++ {
		if statuses[i] != http.StatusCreated {
			t.Fatalf("request %d status = %d body=%s", i, statuses[i], bodies[i])
		}
		if bodies[i] != bodies[0] {
			t.Fatalf("response %d differs:\n%s\n%s", i, bodies[i], bodies[0])
		}
		if replayed[i] == "true" {
			replayCount++
		}
	}
	if replayCount != n-1 {
		t.Fatalf("replayed responses = %d, want %d", replayCount, n-1)
	}
	var events, deliveries, idem int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&events)
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries`).Scan(&deliveries)
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_records`).Scan(&idem)
	if events != 1 || deliveries != 1 || idem != 1 {
		t.Fatalf("counts events=%d deliveries=%d idempotency=%d", events, deliveries, idem)
	}
	w := e.ingest(key, "concurrent-key-0001", eventBody("different"))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "idempotency_key_reused") {
		t.Fatalf("different body status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestEventValidationWritesNoRows(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("Validation")
	e.createEndpoint(key, "endpoint", "https://1.1.1.1/validation")
	cases := []struct {
		name string
		body string
		code int
		want string
	}{
		{"bad type", `{"type":"BadType","data":{"a":1}}`, 422, "validation_failed"},
		{"array data", `{"type":"order.created","data":[1,2]}`, 422, "validation_failed"},
		{"future", `{"type":"order.created","occurred_at":"2099-01-01T00:00:00Z","data":{}}`, 422, "validation_failed"},
		{"unknown envelope", `{"type":"order.created","data":{},"extra":1}`, 400, "invalid_request"},
		{"malformed", `{"type":"order.created","data":`, 400, "invalid_request"},
		{"oversized", `{"type":"order.created","data":{"blob":"` + strings.Repeat("a", 270000) + `"}}`, 413, "payload_too_large"},
		{"deep", `{"type":"order.created","data":{"a":` + strings.Repeat(`{"a":`, 33) + `1` + strings.Repeat(`}`, 33) + `}}`, 422, "validation_failed"},
	}
	for i, tc := range cases {
		w := e.ingest(key, fmt.Sprintf("validation-key-%04d", i), tc.body)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
	var events, deliveries, idem int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&events)
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries`).Scan(&deliveries)
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_records`).Scan(&idem)
	if events != 0 || deliveries != 0 || idem != 0 {
		t.Fatalf("invalid ingest wrote rows: events=%d deliveries=%d idem=%d", events, deliveries, idem)
	}
}

func TestEndpointSSRFAndPauseFanout(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("SSRF")
	for _, u := range []string{"http://1.1.1.1/hook", "https://127.0.0.1/hook", "https://10.1.2.3/hook", "https://localhost/hook"} {
		w := e.request(http.MethodPost, "/api/v1/endpoints", map[string]any{"name": "bad", "url": u}, key, nil)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "destination_rejected") {
			t.Fatalf("%s: status=%d body=%s", u, w.Code, w.Body.String())
		}
	}
	ep := e.createEndpoint(key, "good", "https://1.1.1.1/pause-test")
	w := e.request(http.MethodPatch, "/api/v1/endpoints/"+ep.ID.String(), map[string]any{"status": "paused"}, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pause = %d %s", w.Code, w.Body.String())
	}
	ev := e.ingest(key, "pause-event-0001", eventBody("paused"))
	if ev.Code != http.StatusCreated || !strings.Contains(ev.Body.String(), `"no_subscribers"`) {
		t.Fatalf("paused ingest = %d %s", ev.Code, ev.Body.String())
	}
	w = e.request(http.MethodPatch, "/api/v1/endpoints/"+ep.ID.String(), map[string]any{"status": "active"}, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resume = %d", w.Code)
	}
	ev = e.ingest(key, "pause-event-0002", eventBody("active"))
	if ev.Code != http.StatusCreated || !strings.Contains(ev.Body.String(), `"delivery_count": 1`) {
		t.Fatalf("active ingest = %d %s", ev.Code, ev.Body.String())
	}
}

func TestSecretsAndCredentialGuards(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	tenant, key := e.signup("Secrets")
	ep := e.createEndpoint(key, "endpoint", "https://1.1.1.1/secrets")
	if !strings.HasPrefix(ep.SigningSecret, "whsec_") {
		t.Fatalf("signing secret = %q", ep.SigningSecret)
	}
	w := e.request(http.MethodGet, "/api/v1/endpoints/"+ep.ID.String()+"/secrets", nil, key, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), ep.SigningSecret) {
		t.Fatalf("secret metadata leaked plaintext: %s", w.Body.String())
	}
	// Ciphertext decrypts only with the correct AAD.
	var ct, nonce []byte
	var version int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT ciphertext, nonce, key_version FROM endpoint_secrets
		WHERE tenant_id=$1 AND id=$2`, tenant.ID, ep.SigningSecretID).Scan(&ct, &nonce, &version); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(e.cfg.KEK, version)
	plain, err := box.Open(ct, nonce, secrets.AAD(tenant.ID.String(), ep.ID.String(), ep.SigningSecretID.String(), version))
	if err != nil || string(plain) != ep.SigningSecret {
		t.Fatalf("decrypt failed: %v", err)
	}
	if _, err := box.Open(ct, nonce, secrets.AAD(tenant.ID.String(), ep.ID.String(), uuid.NewString(), version)); err == nil {
		t.Fatal("ciphertext opened with wrong AAD")
	}

	w = e.request(http.MethodPost, "/api/v1/endpoints/"+ep.ID.String()+"/secrets", map[string]any{"overlap_seconds": 60}, key, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("second secret = %d %s", w.Code, w.Body.String())
	}
	var second struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	w = e.request(http.MethodPost, "/api/v1/endpoints/"+ep.ID.String()+"/secrets", map[string]any{"overlap_seconds": 60}, key, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "secret_overlap_limit") {
		t.Fatalf("third secret = %d %s", w.Code, w.Body.String())
	}
	w = e.request(http.MethodDelete, "/api/v1/endpoints/"+ep.ID.String()+"/secrets/"+ep.SigningSecretID.String(), nil, key, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("retire first = %d %s", w.Code, w.Body.String())
	}
	w = e.request(http.MethodDelete, "/api/v1/endpoints/"+ep.ID.String()+"/secrets/"+second.ID, nil, key, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "last_active_secret") {
		t.Fatalf("retire last = %d %s", w.Code, w.Body.String())
	}

	// Last owner credential cannot be revoked.
	w = e.request(http.MethodGet, "/api/v1/credentials", nil, key, nil)
	var creds struct {
		Data []store.Credential `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &creds)
	ownerID := ""
	for _, c := range creds.Data {
		if c.Role == "owner" {
			ownerID = c.ID.String()
		}
	}
	w = e.request(http.MethodDelete, "/api/v1/credentials/"+ownerID, nil, key, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "last_owner_credential") {
		t.Fatalf("last owner revoke = %d %s", w.Code, w.Body.String())
	}

	// Ingest-only credential can write events but cannot manage endpoints.
	w = e.request(http.MethodPost, "/api/v1/credentials", map[string]any{"name": "ci", "role": "ingest"}, key, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create ingest key = %d %s", w.Code, w.Body.String())
	}
	var ingestCred store.NewCredential
	_ = json.Unmarshal(w.Body.Bytes(), &ingestCred)
	w = e.request(http.MethodPost, "/api/v1/endpoints", map[string]any{"name": "forbidden", "url": "https://1.1.1.1/forbidden"}, ingestCred.APIKey, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("ingest endpoint write = %d", w.Code)
	}
	w = e.request(http.MethodGet, "/api/v1/events", nil, ingestCred.APIKey, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest read = %d", w.Code)
	}

	// Rotation overlap 0 invalidates the old key immediately.
	w = e.request(http.MethodPost, "/api/v1/credentials/"+ingestCred.ID.String()+"/rotate", map[string]any{"overlap_seconds": 0}, key, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("rotate = %d %s", w.Code, w.Body.String())
	}
	var rotated store.NewCredential
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	if rotated.APIKey == "" {
		t.Fatal("rotate did not return key")
	}
	time.Sleep(10 * time.Millisecond)
	w = e.request(http.MethodGet, "/api/v1/events", nil, ingestCred.APIKey, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old key after rotate = %d", w.Code)
	}
	w = e.request(http.MethodGet, "/api/v1/events", nil, rotated.APIKey, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("new key after rotate = %d", w.Code)
	}
}

func TestAuditAndAdminSuspension(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	tenant, key := e.signup("Audit")
	w := e.request(http.MethodPost, "/api/v1/endpoints", map[string]any{"name": "e", "url": "https://1.1.1.1/audit"}, key, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	w = e.request(http.MethodGet, "/api/v1/audit-logs", nil, key, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "endpoint.create") {
		t.Fatalf("audit = %d %s", w.Code, w.Body.String())
	}
	w = e.adminRequest(http.MethodPost, "/internal/v1/admin/tenants/"+tenant.ID.String()+"/suspend", map[string]any{"reason": "abuse"})
	if w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	w = e.request(http.MethodGet, "/api/v1/tenant", nil, key, nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "tenant_suspended") {
		t.Fatalf("suspended auth = %d %s", w.Code, w.Body.String())
	}
	w = e.adminRequest(http.MethodPost, "/internal/v1/admin/tenants/"+tenant.ID.String()+"/resume", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resume = %d", w.Code)
	}
	w = e.request(http.MethodGet, "/api/v1/tenant", nil, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resumed auth = %d", w.Code)
	}
}

func TestReadAPIsAndCursor(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("Read APIs")
	e.createEndpoint(key, "reader", "https://1.1.1.1/read")
	for i := 0; i < 3; i++ {
		w := e.ingest(key, fmt.Sprintf("read-event-%04d", i), eventBody(fmt.Sprintf("read-%d", i)))
		if w.Code != http.StatusCreated {
			t.Fatalf("ingest %d = %d %s", i, w.Code, w.Body.String())
		}
	}
	w := e.request(http.MethodGet, "/api/v1/events?limit=1", nil, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("events page 1 = %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data       []store.Event `json:"data"`
		NextCursor *string       `json:"next_cursor"`
		HasMore    bool          `json:"has_more"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Data) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("bad first page: %+v", page)
	}
	w = e.request(http.MethodGet, "/api/v1/events?limit=1&cursor="+*page.NextCursor, nil, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("events page 2 = %d %s", w.Code, w.Body.String())
	}
	var page2 struct {
		Data []store.Event `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2.Data) != 1 || page2.Data[0].ID == page.Data[0].ID {
		t.Fatalf("cursor did not advance: %+v", page2)
	}
	w = e.request(http.MethodGet, "/api/v1/events?limit=1&cursor=not-a-cursor", nil, key, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_cursor") {
		t.Fatalf("invalid cursor = %d %s", w.Code, w.Body.String())
	}
	// A cursor is bound to its filter set.
	w = e.request(http.MethodGet, "/api/v1/events?limit=1&status=accepted&cursor="+*page.NextCursor, nil, key, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("filter-mismatched cursor = %d", w.Code)
	}
	w = e.request(http.MethodGet, "/api/v1/deliveries?limit=2", nil, key, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"has_more":true`) {
		t.Fatalf("deliveries = %d %s", w.Code, w.Body.String())
	}
	w = e.request(http.MethodGet, "/api/v1/audit-logs?action=endpoint.create&resource_type=endpoint", nil, key, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "endpoint.create") {
		t.Fatalf("audit filter = %d %s", w.Code, w.Body.String())
	}
}

func TestSchemaValidationAtIngest(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("Schemas")
	e.createEndpoint(key, "e", "https://1.1.1.1/schema")
	w := e.request(http.MethodPost, "/api/v1/schemas", map[string]any{
		"event_type": "order.created", "version": 1,
		"schema": map[string]any{
			"type": "object", "required": []string{"amount"},
			"properties": map[string]any{"amount": map[string]any{"type": "integer"}},
		},
	}, key, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("schema create = %d %s", w.Code, w.Body.String())
	}
	w = e.ingest(key, "schema-valid-0001", `{"type":"order.created","data":{"amount":42}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid schema ingest = %d %s", w.Code, w.Body.String())
	}
	w = e.ingest(key, "schema-invalid-0001", `{"type":"order.created","data":{"amount":"many"}}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "schema_validation_failed") {
		t.Fatalf("invalid schema ingest = %d %s", w.Code, w.Body.String())
	}
}

func TestTenantAndAdminRateLimits(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("RateLimited")

	cfg := e.cfg
	cfg.RateLimitIngestPerSec = 3
	cfg.RateLimitManagementPerSec = 2
	cfg.RateLimitAdminPerSec = 2
	box, _ := secrets.New(cfg.KEK, 1)
	st := &store.Store{
		Pool: e.pool, ExpectedSchemaVersion: 5, Box: box, EndpointLimit: 50,
		IdempotencyTTL: 24 * time.Hour, EventFutureSkew: 5 * time.Minute, EventPastWindow: 30 * 24 * time.Hour,
	}
	metrics := observability.NewMetrics("test", "test")
	deps := Deps{
		Config: cfg, Checker: &db.Store{Pool: e.pool, ExpectedSchemaVersion: 5},
		Metrics: metrics, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Store: st, Schemas: jsonschema.New(), SSRF: ssrf.New(),
	}
	pub := NewRouter(deps)
	adm := NewAdminRouter(deps)

	// Clear counters and test tenant_management limit (2 req/s).
	_, _ = e.pool.Exec(context.Background(), `TRUNCATE rate_limit_counters`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tenant", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		pub.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("mgmt req %d status = %d body=%s", i, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	pub.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("expected 429 on 3rd mgmt req, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After: 1, got %q", w.Header().Get("Retry-After"))
	}

	// Test tenant_ingest limit (3 req/s).
	_, _ = e.pool.Exec(context.Background(), `TRUNCATE rate_limit_counters`)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(eventBody(fmt.Sprintf("rl-%d", i))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", fmt.Sprintf("rl-idem-%04d", i))
		w := httptest.NewRecorder()
		pub.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("ingest req %d status = %d body=%s", i, w.Code, w.Body.String())
		}
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(eventBody("rl-overflow")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", "rl-idem-overflow")
	w = httptest.NewRecorder()
	pub.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected 429 with Retry-After: 1 on 4th ingest req, got %d body=%s", w.Code, w.Body.String())
	}

	// Test admin rate limit (2 req/s).
	_, _ = e.pool.Exec(context.Background(), `TRUNCATE rate_limit_counters`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/admin/queue", nil)
		req.Header.Set("X-Admin-Token", cfg.AdminToken)
		w := httptest.NewRecorder()
		adm.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("admin req %d status = %d body=%s", i, w.Code, w.Body.String())
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/internal/v1/admin/queue", nil)
	req.Header.Set("X-Admin-Token", cfg.AdminToken)
	w = httptest.NewRecorder()
	adm.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected 429 on 3rd admin req, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestEventReplayAdminReplayAndConcurrency(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	tenant, key := e.signup("Replay Tenant")
	ep1 := e.createEndpoint(key, "ep-1", "https://1.1.1.1/replay-1")
	ep2 := e.createEndpoint(key, "ep-2", "https://1.1.1.1/replay-2")

	evW := e.ingest(key, "replay-event-0001", eventBody("replay-1"))
	if evW.Code != http.StatusCreated {
		t.Fatalf("ingest = %d %s", evW.Code, evW.Body.String())
	}
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(evW.Body.Bytes(), &ev)

	// Mark ep1 delivery failed (with 1 attempt in cycle 1) and ep2 delivery delivered.
	var d1ID, d2ID uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM deliveries WHERE event_id=$1 AND endpoint_id=$2`, ev.ID, ep1.ID).Scan(&d1ID); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM deliveries WHERE event_id=$1 AND endpoint_id=$2`, ev.ID, ep2.ID).Scan(&d2ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.pool.Exec(context.Background(), `
		INSERT INTO delivery_attempts (tenant_id, delivery_id, cycle, attempt_no, started_at, finished_at, outcome, http_status, error_code, duration_ms)
		VALUES ($1, $2, 1, 1, now() - interval '5 seconds', now() - interval '4 seconds', 'permanent_failure', 404, 'http_404', 42)`,
		tenant.ID, d1ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Exec(context.Background(), `
		UPDATE deliveries
		SET status = 'failed', attempt_count = 1, attempt_seq = 1,
		    last_error_code = 'http_404', last_http_status = 404, completed_at = now()
		WHERE id = $1`, d1ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Exec(context.Background(), `
		UPDATE deliveries
		SET status = 'delivered', attempt_count = 1, attempt_seq = 1,
		    last_http_status = 200, completed_at = now()
		WHERE id = $1`, d2ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.pool.Exec(context.Background(), `UPDATE events SET status = 'partially_delivered' WHERE id = $1`, ev.ID)

	// Concurrent replay: 10 goroutines concurrently replay the event. Exactly 1 must win with 202, 9 get 409.
	const n = 10
	codes := make([]int, n)
	bodies := make([]string, n)
	start := make(chan struct{})
	var ready, wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ready.Add(1)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			w := e.request(http.MethodPost, "/api/v1/events/"+ev.ID+"/replay", map[string]any{
				"reason": "receiver route deployed",
			}, key, nil)
			codes[i] = w.Code
			bodies[i] = w.Body.String()
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	winners := 0
	conflicts := 0
	for i, c := range codes {
		switch c {
		case http.StatusAccepted:
			winners++
			var resp store.ReplayEventResult
			if err := json.Unmarshal([]byte(bodies[i]), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Replayed != 1 || resp.Skipped != 1 {
				t.Fatalf("unexpected replay counts: %+v", resp)
			}
		case http.StatusConflict:
			conflicts++
			if !strings.Contains(bodies[i], "replay_not_allowed") {
				t.Fatalf("expected replay_not_allowed, got %s", bodies[i])
			}
		default:
			t.Fatalf("unexpected replay status %d: %s", c, bodies[i])
		}
	}
	if winners != 1 || conflicts != n-1 {
		t.Fatalf("concurrent replay winners=%d conflicts=%d", winners, conflicts)
	}

	// Verify cycle incremented to 2, attempt_count reset to 0, cycle-1 attempt preserved, event status is delivering, audit written.
	var cycle, attemptCount, attemptSeq int
	var status, evStatus string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT d.cycle, d.attempt_count, d.attempt_seq, d.status, ev.status
		 FROM deliveries d JOIN events ev ON ev.id = d.event_id WHERE d.id = $1`, d1ID).
		Scan(&cycle, &attemptCount, &attemptSeq, &status, &evStatus); err != nil {
		t.Fatal(err)
	}
	if cycle != 2 || attemptCount != 0 || attemptSeq != 0 || status != "pending" || evStatus != "delivering" {
		t.Fatalf("after replay: cycle=%d attempts=%d seq=%d status=%s evStatus=%s",
			cycle, attemptCount, attemptSeq, status, evStatus)
	}
	var priorAttempts int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1 AND cycle = 1`, d1ID).Scan(&priorAttempts)
	if priorAttempts != 1 {
		t.Fatalf("cycle 1 attempts lost: %d", priorAttempts)
	}
	var auditCount int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = 'event.replay' AND resource_id = $1`, d1ID).Scan(&auditCount)
	if auditCount != 1 {
		t.Fatalf("event.replay audit count = %d", auditCount)
	}

	// Test Admin Replay and Admin Inspection routes (tenants, queue, stuck deliveries).
	_, err = e.pool.Exec(context.Background(), `
		UPDATE deliveries
		SET status = 'dead_lettered', attempt_count = 8, attempt_seq = 8,
		    completed_at = now(), dead_lettered_at = now()
		WHERE id = $1`, d1ID)
	if err != nil {
		t.Fatal(err)
	}
	w := e.adminRequest(http.MethodPost, "/internal/v1/admin/deliveries/"+d1ID.String()+"/replay", map[string]any{
		"reason": "operator console replay",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("admin replay = %d %s", w.Code, w.Body.String())
	}
	_ = e.pool.QueryRow(context.Background(), `SELECT cycle FROM deliveries WHERE id = $1`, d1ID).Scan(&cycle)
	if cycle != 3 {
		t.Fatalf("expected cycle 3 after admin replay, got %d", cycle)
	}
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = 'admin.replay' AND resource_id = $1`, d1ID).Scan(&auditCount)
	if auditCount != 1 {
		t.Fatalf("admin.replay audit count = %d", auditCount)
	}

	w = e.adminRequest(http.MethodGet, "/internal/v1/admin/tenants", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tenant.ID.String()) {
		t.Fatalf("admin tenants = %d %s", w.Code, w.Body.String())
	}
	w = e.adminRequest(http.MethodGet, "/internal/v1/admin/queue", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"by_status"`) {
		t.Fatalf("admin queue = %d %s", w.Code, w.Body.String())
	}
	w = e.adminRequest(http.MethodGet, "/internal/v1/admin/deliveries/stuck", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data"`) {
		t.Fatalf("admin stuck = %d %s", w.Code, w.Body.String())
	}
}

func TestSecretRotationOverlapAndRetirementWithReceiver(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	_, key := e.signup("Rotation Overlap")
	ep := e.createEndpoint(key, "rot-ep", "https://1.1.1.1/rot")

	// Create second secret with 3600s overlap.
	w := e.request(http.MethodPost, "/api/v1/endpoints/"+ep.ID.String()+"/secrets", map[string]any{"overlap_seconds": 3600}, key, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create second secret = %d %s", w.Code, w.Body.String())
	}
	var sec2 store.NewSecret
	if err := json.Unmarshal(w.Body.Bytes(), &sec2); err != nil {
		t.Fatal(err)
	}

	raw1, err := receiver.DecodeSecret(ep.SigningSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := receiver.DecodeSecret(sec2.SigningSecret)
	if err != nil {
		t.Fatal(err)
	}

	msgID := uuid.NewString()
	now := time.Now().UTC()
	body := []byte(`{"type":"order.created","data":{"order_id":"rot-1"}}`)
	dualHeader := receiver.SignV1(raw2, msgID, now.Unix(), body) + " " + receiver.SignV1(raw1, msgID, now.Unix(), body)

	// Receiver configured ONLY with old secret1 accepts during overlap.
	recOld, err := receiver.New([]string{ep.SigningSecret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req1 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req1.Header.Set("webhook-id", msgID)
	req1.Header.Set("webhook-timestamp", fmt.Sprintf("%d", now.Unix()))
	req1.Header.Set("webhook-signature", dualHeader)
	w1 := httptest.NewRecorder()
	recOld.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("old receiver during overlap = %d %s", w1.Code, w1.Body.String())
	}

	// Receiver configured ONLY with new secret2 also accepts during overlap.
	recNew, err := receiver.New([]string{sec2.SigningSecret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req2.Header.Set("webhook-id", msgID)
	req2.Header.Set("webhook-timestamp", fmt.Sprintf("%d", now.Unix()))
	req2.Header.Set("webhook-signature", dualHeader)
	w2 := httptest.NewRecorder()
	recNew.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("new receiver during overlap = %d %s", w2.Code, w2.Body.String())
	}

	// Retire old secret1 and verify only secret2 remains active in DB, and signing only with secret2 fails on recOld.
	w = e.request(http.MethodDelete, "/api/v1/endpoints/"+ep.ID.String()+"/secrets/"+ep.SigningSecretID.String(), nil, key, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("retire secret1 = %d %s", w.Code, w.Body.String())
	}
	msgID3 := uuid.NewString()
	postRetireHeader := receiver.SignV1(raw2, msgID3, now.Unix(), body)
	req3 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req3.Header.Set("webhook-id", msgID3)
	req3.Header.Set("webhook-timestamp", fmt.Sprintf("%d", now.Unix()))
	req3.Header.Set("webhook-signature", postRetireHeader)
	w3 := httptest.NewRecorder()
	recOld.Handler().ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("expected retired secret receiver to reject with 401, got %d", w3.Code)
	}
}

func TestRetentionCleanupAndDataIntegrityQueries(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	tenant, key := e.signup("Retention")
	e.createEndpoint(key, "ret-ep", "https://1.1.1.1/retention")

	evW := e.ingest(key, "ret-event-0001", eventBody("ret-1"))
	if evW.Code != http.StatusCreated {
		t.Fatalf("ingest = %d %s", evW.Code, evW.Body.String())
	}
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(evW.Body.Bytes(), &ev)

	// Mark delivery completed and backdate event, idempotency record, and rate limit counter.
	_, err := e.pool.Exec(context.Background(), `
		UPDATE deliveries
		SET status = 'delivered', completed_at = now() - interval '39 days', created_at = now() - interval '40 days'
		WHERE event_id = $1`, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Exec(context.Background(), `
		UPDATE events SET status = 'delivered', created_at = now() - interval '40 days' WHERE id = $1`, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Exec(context.Background(), `
		UPDATE idempotency_records
		SET created_at = now() - interval '48 hours', expires_at = now() - interval '24 hours'
		WHERE tenant_id = $1`, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Exec(context.Background(), `
		INSERT INTO rate_limit_counters (scope_type, scope_id, window_start, count)
		VALUES ('tenant_ingest', $1, now() - interval '2 hours', 10)`, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}

	st := &store.Store{Pool: e.pool}
	summary, err := st.RunRetentionPass(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("RunRetentionPass: %v", err)
	}
	if summary.IdempotencyDeleted < 1 || summary.RateLimitsDeleted < 1 || summary.EventsDeleted != 1 {
		t.Fatalf("unexpected retention summary: %+v", summary)
	}

	// Execute all 5 data integrity queries from docs/02-data-model.md §8.
	integrityQueries := []string{
		`SELECT count(*) FROM deliveries WHERE status = 'in_progress' AND (lease_token IS NULL OR lease_expires_at IS NULL)`,
		`SELECT count(*) FROM deliveries WHERE status IN ('delivered','failed','dead_lettered','canceled') AND completed_at IS NULL`,
		`SELECT count(*) FROM deliveries WHERE attempt_count > max_attempts`,
		`SELECT count(*) FROM (SELECT event_id, endpoint_id FROM deliveries GROUP BY 1,2 HAVING count(*) > 1) dup`,
		`SELECT count(*) FROM (
			SELECT e.id
			FROM events e JOIN deliveries d ON d.event_id = e.id
			WHERE e.status = 'delivered'
			GROUP BY e.id
			HAVING count(d.id) FILTER (WHERE d.status <> 'delivered') > 0
		) drift`,
	}
	for i, q := range integrityQueries {
		var bad int
		if err := e.pool.QueryRow(context.Background(), q).Scan(&bad); err != nil {
			t.Fatalf("integrity query %d failed: %v", i, err)
		}
		if bad != 0 {
			t.Fatalf("integrity query %d returned %d violations", i, bad)
		}
	}
}

func TestLogRedactionAndSQLInjectionCorpus(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	box, _ := secrets.New(e.cfg.KEK, 1)
	st := &store.Store{
		Pool: e.pool, ExpectedSchemaVersion: 5, Box: box, EndpointLimit: 50,
		IdempotencyTTL: 24 * time.Hour, EventFutureSkew: 5 * time.Minute, EventPastWindow: 30 * 24 * time.Hour,
	}
	r := NewRouter(Deps{
		Config: e.cfg, Checker: &db.Store{Pool: e.pool, ExpectedSchemaVersion: 5},
		Metrics: observability.NewMetrics("test", "test"), Logger: logger,
		Store: st, Schemas: jsonschema.New(), SSRF: ssrf.New(),
	})

	// 1. Canary redaction test: signup key, signing secret, and event payload canary must never appear in logs.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants", strings.NewReader(`{"name":"Canary Tenant"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("signup = %d", w.Code)
	}
	var signupOut struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &signupOut)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/endpoints", strings.NewReader(`{"name":"canary-ep","url":"https://1.1.1.1/canary"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signupOut.APIKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("endpoint = %d", w.Code)
	}
	var epOut store.NewEndpoint
	_ = json.Unmarshal(w.Body.Bytes(), &epOut)

	canaryPayload := "CANARY_SUPER_SENSITIVE_PII_VALUE_998877"
	req = httptest.NewRequest(http.MethodPost, "/api/v1/events",
		strings.NewReader(fmt.Sprintf(`{"type":"order.created","data":{"secret_pii":%q}}`, canaryPayload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signupOut.APIKey)
	req.Header.Set("Idempotency-Key", "canary-idem-0001")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("ingest = %d", w.Code)
	}

	logged := logBuf.String()
	for _, forbidden := range []string{signupOut.APIKey, epOut.SigningSecret, canaryPayload} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("sensitive canary value %q leaked into structured logs", forbidden)
		}
	}

	// 2. SQL injection corpus across query filters, cursors, and path params.
	sqliPayloads := []string{
		`' OR '1'='1`,
		`order.created'; DROP TABLE events;--`,
		`" UNION SELECT id, secret_hmac FROM api_credentials--`,
		`1; SELECT pg_sleep(5)--`,
	}
	for _, p := range sqliPayloads {
		q := url.Values{}
		q.Set("type", p)
		w := e.request(http.MethodGet, "/api/v1/events?"+q.Encode(), nil, signupOut.APIKey, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for SQLi type %q, got %d body=%s", p, w.Code, w.Body.String())
		}
		if strings.Contains(strings.ToLower(w.Body.String()), "syntax error") || strings.Contains(strings.ToLower(w.Body.String()), "postgres") {
			t.Fatalf("SQL error detail leaked in response: %s", w.Body.String())
		}

		q2 := url.Values{}
		q2.Set("cursor", p)
		w2 := e.request(http.MethodGet, "/api/v1/events?"+q2.Encode(), nil, signupOut.APIKey, nil)
		if w2.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for SQLi cursor %q, got %d", p, w2.Code)
		}

		q3 := url.Values{}
		q3.Set("action", p)
		w3 := e.request(http.MethodGet, "/api/v1/audit-logs?"+q3.Encode(), nil, signupOut.APIKey, nil)
		if w3.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for SQLi audit action %q, got %d", p, w3.Code)
		}
	}
}
