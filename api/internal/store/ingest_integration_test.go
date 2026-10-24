//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/relayforge/relayforge/api/internal/db"
	"github.com/relayforge/relayforge/api/internal/secrets"
)

func testStore(t *testing.T) (*Store, *pgxpool.Pool, *pgxpool.Conn) {
	t.Helper()
	dsn := os.Getenv("RELAYFORGE_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://relayforge:relayforge_local_only@localhost:55432/relayforge?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn, 10)
	if err != nil {
		t.Skipf("integration database unavailable: %v", err)
	}
	lock, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = current_database()
		  AND pid <> pg_backend_pid()
		  AND application_name = 'psql'`); err != nil {
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
	kek, _ := base64.StdEncoding.DecodeString("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	box, err := secrets.New(kek, 1)
	if err != nil {
		t.Fatal(err)
	}
	return &Store{
		Pool: pool, ExpectedSchemaVersion: 5, Box: box, EndpointLimit: 50,
		IdempotencyTTL: 24 * time.Hour, EventFutureSkew: 5 * time.Minute, EventPastWindow: 30 * 24 * time.Hour,
	}, pool, lock
}

func seedTenantEndpoint(t *testing.T, s *Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var tenantID, endpointID uuid.UUID
	if err := s.Pool.QueryRow(ctx, `INSERT INTO tenants (slug,name) VALUES ($1,$2) RETURNING id`,
		"fault-"+uuid.NewString()[:8], "Fault Test").Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO endpoints (tenant_id,name,url) VALUES ($1,'e','https://1.1.1.1/hook')
		RETURNING id`, tenantID).Scan(&endpointID); err != nil {
		t.Fatal(err)
	}
	return tenantID, endpointID
}

func ingestInput(tenantID uuid.UUID, wire string, key string) IngestInput {
	h := sha256.Sum256([]byte(wire))
	return IngestInput{
		TenantID: tenantID, Type: "order.created", SchemaVersion: 1,
		OccurredAt: time.Now().UTC(), WireBody: []byte(wire),
		Payload: json.RawMessage(`{"order_id":"x"}`), PayloadBytes: 16,
		PayloadSHA256: h[:], Scope: "events.create", Key: key, Fingerprint: h[:],
	}
}

func TestIngestFaultInjectionLeavesNoRows(t *testing.T) {
	s, pool, lock := testStore(t)
	defer func() { lock.Release(); pool.Close() }()
	tenantID, _ := seedTenantEndpoint(t, s)
	s.ingestHook = func() error { return errors.New("injected failure") }
	_, err := s.IngestEvent(context.Background(), ingestInput(tenantID, `{"type":"order.created","data":{}}`, "fault-key-0001"))
	if err == nil {
		t.Fatal("ingest succeeded despite injected failure")
	}
	for _, table := range []string{"events", "deliveries", "idempotency_records"} {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows after rollback", table, n)
		}
	}
}

func TestIngestSuccessAndReplay(t *testing.T) {
	s, pool, lock := testStore(t)
	defer func() { lock.Release(); pool.Close() }()
	tenantID, _ := seedTenantEndpoint(t, s)
	wire := `{"type":"order.created","data":{"order_id":"x"}}`
	in := ingestInput(tenantID, wire, "success-key-0001")
	first, err := s.IngestEvent(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.StatusCode != 201 {
		t.Fatalf("first result = %+v", first)
	}
	replay, err := s.IngestEvent(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || string(replay.Body) != string(first.Body) {
		t.Fatalf("replay mismatch: %+v vs %+v", replay, first)
	}
	var events, deliveries, idem int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&events)
	pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries`).Scan(&deliveries)
	pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_records`).Scan(&idem)
	if events != 1 || deliveries != 1 || idem != 1 {
		t.Fatalf("counts events=%d deliveries=%d idem=%d", events, deliveries, idem)
	}
}

func TestQueryPlansUseIndexes(t *testing.T) {
	_, pool, lock := testStore(t)
	defer func() { lock.Release(); pool.Close() }()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	setupSQL := `
	INSERT INTO tenants (id, slug, name)
	SELECT
	  ('0199a000-0000-7000-8000-' || lpad(to_hex(t), 12, '0'))::uuid,
	  'bench-t' || t,
	  'Bench ' || t
	FROM generate_series(1, 200) AS t;

	INSERT INTO endpoints (id, tenant_id, name, url)
	SELECT
	  ('0199a000-0000-7000-8001-' || lpad(to_hex(t), 12, '0'))::uuid,
	  ('0199a000-0000-7000-8000-' || lpad(to_hex(t), 12, '0'))::uuid,
	  'ep' || t,
	  'https://1.1.1.1/hook' || t
	FROM generate_series(1, 200) AS t;

	INSERT INTO events (id, tenant_id, type, schema_version, occurred_at, received_at, wire_body, payload, payload_bytes, payload_sha256, status, delivery_count, created_at)
	SELECT
	  uuidv7(),
	  ('0199a000-0000-7000-8000-' || lpad(to_hex((g % 200) + 1), 12, '0'))::uuid,
	  CASE WHEN g % 3 = 0 THEN 'order.created' ELSE 'invoice.paid' END,
	  1,
	  now() - (g || ' seconds')::interval,
	  now() - (g || ' seconds')::interval,
	  '{"type":"order.created","data":{"id":1}}'::bytea,
	  '{"id":1}'::jsonb,
	  8,
	  decode('0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 'hex'),
	  CASE WHEN g % 10 = 0 THEN 'failed' ELSE 'accepted' END,
	  1,
	  now() - (g || ' seconds')::interval
	FROM generate_series(1, 50000) AS g;

	INSERT INTO deliveries (id, tenant_id, event_id, endpoint_id, status, attempt_count, attempt_seq, max_attempts, initial_backoff_ms, max_backoff_ms, retry_window_seconds, timeout_ms, next_attempt_at, created_at)
	SELECT
	  uuidv7(),
	  e.tenant_id,
	  e.id,
	  ('0199a000-0000-7000-8001-' || right(e.tenant_id::text, 12))::uuid,
	  CASE WHEN e.status = 'failed' THEN 'dead_lettered' ELSE 'pending' END,
	  0, 0, 8, 5000, 3600000, 86400, 15000,
	  e.created_at,
	  e.created_at
	FROM events e;

	INSERT INTO audit_logs (tenant_id, actor_type, actor_id, action, resource_type, metadata, created_at)
	SELECT
	  ('0199a000-0000-7000-8000-' || lpad(to_hex((g % 200) + 1), 12, '0'))::uuid,
	  'api_key', 'cred-1', 'endpoint.create', 'endpoint', '{}'::jsonb,
	  now() - (g || ' seconds')::interval
	FROM generate_series(1, 10000) AS g;

	ANALYZE events, deliveries, audit_logs;
	`
	if _, err := tx.Exec(ctx, setupSQL); err != nil {
		t.Fatalf("seed 50k fixture: %v", err)
	}

	queries := []struct {
		name      string
		wantIndex string
		sql       string
	}{
		{
			name:      "events_tenant_created_page1",
			wantIndex: "events_tenant_created_idx",
			sql: `EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, type, status, created_at FROM events
			WHERE tenant_id = '0199a000-0000-7000-8000-000000000001'
			ORDER BY created_at DESC, id DESC LIMIT 25`,
		},
		{
			name:      "events_tenant_created_cursor",
			wantIndex: "events_tenant_created_idx",
			sql: `EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, type, status, created_at FROM events
			WHERE tenant_id = '0199a000-0000-7000-8000-000000000001'
			  AND (created_at, id) < (now() - interval '1000 seconds', 'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid)
			ORDER BY created_at DESC, id DESC LIMIT 25`,
		},
		{
			name:      "events_tenant_type_created",
			wantIndex: "events_tenant_type_created_idx",
			sql: `EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, type, status, created_at FROM events
			WHERE tenant_id = '0199a000-0000-7000-8000-000000000001'
			  AND type = 'order.created'
			ORDER BY created_at DESC, id DESC LIMIT 25`,
		},
		{
			name:      "deliveries_tenant_status_created",
			wantIndex: "deliveries_tenant_status_created_idx",
			sql: `EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, status, attempt_count, next_attempt_at FROM deliveries
			WHERE tenant_id = '0199a000-0000-7000-8000-000000000001'
			  AND status = 'dead_lettered'
			ORDER BY created_at DESC, id DESC LIMIT 25`,
		},
		{
			name:      "audit_logs_tenant_created",
			wantIndex: "audit_logs_tenant_created_idx",
			sql: `EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, action, resource_type, created_at FROM audit_logs
			WHERE tenant_id = '0199a000-0000-7000-8000-000000000001'
			ORDER BY created_at DESC, id DESC LIMIT 25`,
		},
		{
			name:      "deliveries_claim",
			wantIndex: "deliveries_claim_idx",
			sql: `EXPLAIN (ANALYZE, BUFFERS)
			SELECT id FROM deliveries
			WHERE status IN ('pending', 'retry_scheduled')
			  AND next_attempt_at <= now()
			ORDER BY next_attempt_at, id
			FOR UPDATE SKIP LOCKED LIMIT 20`,
		},
	}

	for _, q := range queries {
		rows, err := tx.Query(ctx, q.sql)
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		var plan string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan += line + "\n"
		}
		rows.Close()
		t.Logf("=== %s ===\n%s", q.name, plan)
		if !contains(plan, q.wantIndex) {
			t.Errorf("%s did not use index %s:\n%s", q.name, q.wantIndex, plan)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
