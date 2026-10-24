//go:build e2e

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func doJSON(t *testing.T, client *http.Client, method, url, apiKey string, headers map[string]string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, url, err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(rawResp) > 0 {
		_ = json.Unmarshal(rawResp, &parsed)
	}
	return resp.StatusCode, parsed
}

func TestEndToEndDeliveryLifecycle(t *testing.T) {
	apiURL := envOr("RELAYFORGE_E2E_API_URL", "http://127.0.0.1:8080")
	receiverControlURL := envOr("RELAYFORGE_E2E_RECEIVER_URL", "http://127.0.0.1:8090")
	receiverTargetURL := envOr("RELAYFORGE_E2E_TARGET_URL", "https://receiver:8090/webhook")

	client := &http.Client{Timeout: 10 * time.Second}

	// 1. Reset demo receiver.
	code, _ := doJSON(t, client, http.MethodPost, receiverControlURL+"/control/reset", "", nil, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("receiver reset returned %d", code)
	}

	// 2. Register a new tenant + owner API key via public signup.
	slug := "e2e-" + uuid.NewString()[:8]
	code, tenantResp := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/tenants", "", nil, map[string]any{
		"slug": slug,
		"name": "E2E Verification Tenant",
	})
	if code != http.StatusCreated {
		t.Fatalf("create tenant returned %d: %v", code, tenantResp)
	}
	apiKey, _ := tenantResp["api_key"].(string)
	if apiKey == "" {
		t.Fatalf("missing api_key in tenant creation response: %v", tenantResp)
	}

	// 3. Create an endpoint with fast initial backoff (500ms) and max_attempts=3.
	code, epResp := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/endpoints", apiKey, nil, map[string]any{
		"name":                 "e2e-receiver",
		"url":                  receiverTargetURL,
		"max_attempts":         3,
		"initial_backoff_ms":   500,
		"max_backoff_ms":       2000,
		"retry_window_seconds": 300,
		"timeout_ms":           3000,
		"rate_limit_per_sec":   50,
	})
	if code != http.StatusCreated {
		t.Fatalf("create endpoint returned %d: %v", code, epResp)
	}
	signingSecret, _ := epResp["signing_secret"].(string)
	if signingSecret == "" {
		t.Fatalf("missing signing_secret in endpoint response: %v", epResp)
	}

	// Configure receiver with the endpoint's signing secret.
	code, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/secrets", "", nil, map[string]any{
		"secrets": []string{signingSecret},
	})
	if code != http.StatusOK {
		t.Fatalf("configure receiver secrets returned %d", code)
	}

	// Scenario A: Immediate 200 OK delivery + duplicate-safe verification.
	code, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status": 200,
	})
	if code != http.StatusOK {
		t.Fatalf("set receiver behavior 200 failed: %d", code)
	}

	code, ev1 := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/events", apiKey, map[string]string{
		"Idempotency-Key": "e2e-success-" + uuid.NewString()[:8],
	}, map[string]any{
		"type":        "order.created",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"data":        map[string]any{"order_id": "ord_success_1", "amount": 4200},
	})
	if code != http.StatusCreated {
		t.Fatalf("ingest event 1 returned %d: %v", code, ev1)
	}
	ev1ID := ev1["id"].(string)
	deliv1 := waitForEventTerminal(t, client, apiURL, apiKey, ev1ID, "delivered", 15*time.Second)
	if deliv1["status"] != "delivered" || int(deliv1["attempt_count"].(float64)) != 1 {
		t.Fatalf("expected delivery 1 delivered in 1 attempt, got %v", deliv1)
	}

	// Scenario B: Transient 500 twice, then 200 OK on 3rd attempt.
	code, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status":       200,
		"fail_first_n": 2,
		"fail_status":  500,
	})
	if code != http.StatusOK {
		t.Fatalf("set receiver transient failure behavior failed: %d", code)
	}

	code, ev2 := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/events", apiKey, map[string]string{
		"Idempotency-Key": "e2e-retry-" + uuid.NewString()[:8],
	}, map[string]any{
		"type":        "order.created",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"data":        map[string]any{"order_id": "ord_retry_2"},
	})
	if code != http.StatusCreated {
		t.Fatalf("ingest event 2 returned %d: %v", code, ev2)
	}
	ev2ID := ev2["id"].(string)
	deliv2 := waitForEventTerminal(t, client, apiURL, apiKey, ev2ID, "delivered", 20*time.Second)
	if deliv2["status"] != "delivered" || int(deliv2["attempt_count"].(float64)) != 3 {
		t.Fatalf("expected delivery 2 delivered on attempt 3, got %v", deliv2)
	}

	// Scenario C: Permanent 404 failure (no retry).
	code, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status": 404,
	})
	if code != http.StatusOK {
		t.Fatalf("set receiver 404 behavior failed: %d", code)
	}

	code, ev3 := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/events", apiKey, map[string]string{
		"Idempotency-Key": "e2e-perm-" + uuid.NewString()[:8],
	}, map[string]any{
		"type":        "order.created",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"data":        map[string]any{"order_id": "ord_perm_3"},
	})
	if code != http.StatusCreated {
		t.Fatalf("ingest event 3 returned %d: %v", code, ev3)
	}
	ev3ID := ev3["id"].(string)
	deliv3 := waitForEventTerminal(t, client, apiURL, apiKey, ev3ID, "failed", 15*time.Second)
	if deliv3["status"] != "failed" || int(deliv3["attempt_count"].(float64)) != 1 {
		t.Fatalf("expected permanent failure on attempt 1, got %v", deliv3)
	}

	// Scenario D: Dead-letter after exhausting all 3 attempts on 503.
	code, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status": 503,
	})
	if code != http.StatusOK {
		t.Fatalf("set receiver 503 behavior failed: %d", code)
	}

	code, ev4 := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/events", apiKey, map[string]string{
		"Idempotency-Key": "e2e-dlq-" + uuid.NewString()[:8],
	}, map[string]any{
		"type":        "order.created",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"data":        map[string]any{"order_id": "ord_dlq_4"},
	})
	if code != http.StatusCreated {
		t.Fatalf("ingest event 4 returned %d: %v", code, ev4)
	}
	ev4ID := ev4["id"].(string)
	deliv4 := waitForEventTerminal(t, client, apiURL, apiKey, ev4ID, "failed", 25*time.Second)
	if deliv4["status"] != "dead_lettered" || int(deliv4["attempt_count"].(float64)) != 3 {
		t.Fatalf("expected dead_lettered after 3 attempts, got %v", deliv4)
	}

	// Scenario E: Authorised replay of dead-lettered event 4 after receiver recovers -> delivered in cycle 2.
	code, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status": 200,
	})
	if code != http.StatusOK {
		t.Fatalf("restore receiver 200 behavior failed: %d", code)
	}
	code, replayResp := doJSON(t, client, http.MethodPost, fmt.Sprintf("%s/api/v1/events/%s/replay", apiURL, ev4ID), apiKey, nil, map[string]any{
		"reason": "receiver recovered from 503 outage",
	})
	if code != http.StatusAccepted {
		t.Fatalf("replay event 4 returned %d: %v", code, replayResp)
	}
	deliv4Replayed := waitForEventTerminal(t, client, apiURL, apiKey, ev4ID, "delivered", 15*time.Second)
	if deliv4Replayed["status"] != "delivered" || int(deliv4Replayed["attempt_count"].(float64)) != 1 {
		t.Fatalf("expected replayed delivery to succeed on attempt 1 of cycle 2, got %v", deliv4Replayed)
	}
	deliv4ID := deliv4Replayed["id"].(string)
	code, attemptsResp := doJSON(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/deliveries/%s/attempts", apiURL, deliv4ID), apiKey, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list attempts for deliv4 returned %d", code)
	}
	attemptsList, _ := attemptsResp["data"].([]any)
	if len(attemptsList) != 4 {
		t.Fatalf("expected 4 total attempts preserved across cycle 1 (3) and cycle 2 (1), got %d", len(attemptsList))
	}

	// Verify /metrics is not exposed on the public API listener.
	code, _ = doJSON(t, client, http.MethodGet, apiURL+"/metrics", "", nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("expected /metrics on public listener to return 404, got %d", code)
	}

	// Verify receiver stats show 3 effects applied (ev1, ev2, and replayed ev4).
	code, stats := doJSON(t, client, http.MethodGet, receiverControlURL+"/control/stats", "", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("get receiver stats returned %d", code)
	}
	if int(stats["effects_applied"].(float64)) != 3 {
		t.Fatalf("expected 3 effects_applied on receiver, got %v", stats)
	}
}

func waitForEventTerminal(
	t *testing.T,
	client *http.Client,
	apiURL, apiKey, eventID, expectedEventStatus string,
	timeout time.Duration,
) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, detail := doJSON(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/events/%s", apiURL, eventID), apiKey, nil, nil)
		if code == http.StatusOK {
			last = detail
			if detail["status"] == expectedEventStatus {
				delivs, _ := detail["deliveries"].([]any)
				if len(delivs) > 0 {
					return delivs[0].(map[string]any)
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event %s to reach %s; last=%v", eventID, expectedEventStatus, last)
	return nil
}

// TestWorkerSIGKILLAndLeaseRecovery verifies Phase 5 crash/failover recovery:
// killing the live worker container with SIGKILL while a delivery is in_progress
// leaves the row leased in PostgreSQL; once the lease expires and the worker is
// restarted, the reaper marks the crashed attempt abandoned (lease_expired) and
// reschedules the delivery, which succeeds on attempt 2.
func TestWorkerSIGKILLAndLeaseRecovery(t *testing.T) {
	apiURL := envOr("RELAYFORGE_E2E_API_URL", "http://127.0.0.1:8080")
	receiverControlURL := envOr("RELAYFORGE_E2E_RECEIVER_URL", "http://127.0.0.1:8090")
	receiverTargetURL := envOr("RELAYFORGE_E2E_TARGET_URL", "https://receiver:8090/webhook")
	dsn := envOr("RELAYFORGE_TEST_DATABASE_URL", "postgres://relayforge:relayforge_local_only@localhost:55432/relayforge?sslmode=disable")

	client := &http.Client{Timeout: 10 * time.Second}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	// Ensure worker is started on exit even if the test fails mid-way.
	defer func() {
		_ = exec.Command("docker", "start", "relayforge-worker-1").Run()
	}()

	_, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/reset", "", nil, map[string]any{})

	slug := "sigkill-" + uuid.NewString()[:8]
	code, tenantResp := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/tenants", "", nil, map[string]any{
		"name": "SIGKILL Drill Tenant",
		"slug": slug,
	})
	if code != http.StatusCreated {
		t.Fatalf("create tenant returned %d: %v", code, tenantResp)
	}
	apiKey := tenantResp["api_key"].(string)

	code, epResp := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/endpoints", apiKey, nil, map[string]any{
		"name":               "SIGKILL Endpoint",
		"url":                receiverTargetURL,
		"event_types":        []string{"order.created"},
		"max_attempts":       4,
		"initial_backoff_ms": 1000,
		"max_backoff_ms":     5000,
		"timeout_ms":         15000,
		"rate_limit_per_sec": 50,
	})
	if code != http.StatusCreated {
		t.Fatalf("create endpoint returned %d: %v", code, epResp)
	}
	signingSecret := epResp["signing_secret"].(string)
	_, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/secrets", "", nil, map[string]any{
		"secrets": []string{signingSecret},
	})

	// Delay receiver by 8000ms so we can catch the delivery in_progress and SIGKILL the worker.
	_, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status":   200,
		"delay_ms": 8000,
	})

	code, ev := doJSON(t, client, http.MethodPost, apiURL+"/api/v1/events", apiKey, map[string]string{
		"Idempotency-Key": "sigkill-event-" + uuid.NewString()[:8],
	}, map[string]any{
		"type": "order.created",
		"data": map[string]any{"order_id": "ord_sigkill_1"},
	})
	if code != http.StatusCreated {
		t.Fatalf("ingest returned %d: %v", code, ev)
	}
	evID := ev["id"].(string)

	// Poll until delivery is claimed (status == 'in_progress').
	var deliveryID uuid.UUID
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		err := pool.QueryRow(context.Background(),
			`SELECT id, status FROM deliveries WHERE event_id = $1`, evID).Scan(&deliveryID, &st)
		if err == nil && st == "in_progress" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if deliveryID == uuid.Nil {
		t.Fatal("delivery never transitioned to in_progress")
	}

	// Send SIGKILL to the live worker container while HTTP is in-flight.
	if out, err := exec.Command("docker", "kill", "--signal=SIGKILL", "relayforge-worker-1").CombinedOutput(); err != nil {
		t.Fatalf("docker kill SIGKILL failed: %v (%s)", err, string(out))
	}

	// Restore fast 200 OK on receiver and expire the crashed worker's lease so the reaper recovers immediately upon restart.
	_, _ = doJSON(t, client, http.MethodPost, receiverControlURL+"/control/behavior", "", nil, map[string]any{
		"status":   200,
		"delay_ms": 0,
	})
	_, err = pool.Exec(context.Background(), `
		UPDATE deliveries
		SET created_at = now() - interval '120 seconds',
		    lease_expires_at = now() - interval '5 seconds'
		WHERE id = $1 AND status = 'in_progress'`, deliveryID)
	if err != nil {
		t.Fatalf("backdate expired lease: %v", err)
	}

	// Start the worker container back up.
	if out, err := exec.Command("docker", "start", "relayforge-worker-1").CombinedOutput(); err != nil {
		t.Fatalf("docker start worker failed: %v (%s)", err, string(out))
	}

	// Wait for the reaper to mark attempt 1 abandoned and worker to deliver attempt 2.
	deliv := waitForEventTerminal(t, client, apiURL, apiKey, evID, "delivered", 25*time.Second)
	if deliv["status"] != "delivered" {
		t.Fatalf("expected delivered after SIGKILL recovery, got %v", deliv)
	}

	code, attemptsResp := doJSON(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/deliveries/%s/attempts", apiURL, deliveryID.String()), apiKey, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list attempts returned %d", code)
	}
	attempts, _ := attemptsResp["data"].([]any)
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 attempts (1 abandoned + 1 success), got %v", attempts)
	}
	foundAbandoned := false
	foundSuccess := false
	for _, rawA := range attempts {
		a := rawA.(map[string]any)
		if a["outcome"] == "abandoned" && a["error_code"] == "lease_expired" {
			foundAbandoned = true
		}
		if a["outcome"] == "success" {
			foundSuccess = true
		}
	}
	if !foundAbandoned || !foundSuccess {
		t.Fatalf("expected both abandoned (lease_expired) and success attempts, got %v", attempts)
	}
}
