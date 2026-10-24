package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	var (
		apiURL            = flag.String("api-url", "http://127.0.0.1:8080", "Public API base URL")
		workerReadyURL    = flag.String("worker-ready-url", "http://127.0.0.1:9091/readyz", "Worker readiness URL")
		receiverControl   = flag.String("receiver-control-url", "http://127.0.0.1:8090", "Host-accessible receiver control URL")
		receiverTargetURL = flag.String("receiver-target-url", "https://receiver:8090/webhook", "Worker-accessible receiver webhook URL (Compose network)")
	)
	flag.Parse()
	client := &http.Client{Timeout: 10 * time.Second}

	banner("RelayForge End-to-End Live Walkthrough (make demo)")

	step(1, "Verifying Go API & Rust Worker Readiness")
	apiReady := mustGetJSON(client, *apiURL+"/readyz", "")
	workerReady := mustGetJSON(client, *workerReadyURL, "")
	fmt.Printf("  API    /readyz -> status=%v version=%v\n", apiReady["status"], apiReady["version"])
	fmt.Printf("  Worker /readyz -> status=%v version=%v\n", workerReady["status"], workerReady["version"])
	mustPostJSON(client, *receiverControl+"/control/reset", "", nil, map[string]any{}, http.StatusOK)

	step(2, "Creating Demo Tenant & API Key")
	slug := fmt.Sprintf("demo-acme-%d", time.Now().Unix()%100000)
	signup := mustPostJSON(client, *apiURL+"/api/v1/tenants", "", nil, map[string]any{
		"slug": slug, "name": "Acme Payments Demo",
	}, http.StatusCreated)
	tenant := signup["tenant"].(map[string]any)
	key := signup["api_key"].(string)
	fmt.Printf("  Tenant: %v (slug=%v)\n  API key: %s... (shown once)\n", tenant["id"], tenant["slug"], key[:20])

	step(3, "Registering Webhook Endpoint & Configuring Receiver Secret")
	ep := mustPostJSON(client, *apiURL+"/api/v1/endpoints", key, nil, map[string]any{
		"name": "billing-service", "url": *receiverTargetURL,
		"max_attempts": 3, "initial_backoff_ms": 500, "max_backoff_ms": 2000,
		"retry_window_seconds": 300, "timeout_ms": 3000, "rate_limit_per_sec": 50,
	}, http.StatusCreated)
	secret := ep["signing_secret"].(string)
	mustPostJSON(client, *receiverControl+"/control/secrets", "", nil, map[string]any{"secrets": []string{secret}}, http.StatusOK)
	fmt.Printf("  Endpoint %v registered (secret %s..., shown once)\n", ep["id"], secret[:12])

	step(4, "Ingest + Transient Failure -> Automatic Retry -> Delivery")
	setBehavior(client, *receiverControl, map[string]any{"status": 200, "fail_first_n": 1, "fail_status": 503})
	idem := fmt.Sprintf("demo-idem-%d", time.Now().UnixNano())
	payload := map[string]any{
		"type":        "order.created",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"data":        map[string]any{"order_id": "ord_demo_9001", "amount_cents": 24900},
	}
	ev1, h1 := mustPostJSONWithHeaders(client, *apiURL+"/api/v1/events", key, map[string]string{"Idempotency-Key": idem}, payload, http.StatusCreated)
	ev1Again, h2 := mustPostJSONWithHeaders(client, *apiURL+"/api/v1/events", key, map[string]string{"Idempotency-Key": idem}, payload, http.StatusCreated)
	fmt.Printf("  POST #1 -> 201 id=%v (replayed=%q)\n  POST #2 same key -> 201 id=%v Idempotency-Replayed=%q\n",
		ev1["id"], h1.Get("Idempotency-Replayed"), ev1Again["id"], h2.Get("Idempotency-Replayed"))
	d1 := waitDelivery(client, *apiURL, key, ev1["id"].(string), "delivered", 20*time.Second)
	printAttempts(client, *apiURL, key, "Event 1 (503 then 200)", d1)

	step(5, "Destination Outage -> Retries Exhausted -> Dead-Letter")
	setBehavior(client, *receiverControl, map[string]any{"status": 503})
	payload["data"] = map[string]any{"order_id": "ord_demo_9002", "amount_cents": 1900}
	ev2 := mustPostJSON(client, *apiURL+"/api/v1/events", key, map[string]string{"Idempotency-Key": idem + "-2"}, payload, http.StatusCreated)
	d2 := waitDelivery(client, *apiURL, key, ev2["id"].(string), "dead_lettered", 30*time.Second)
	printAttempts(client, *apiURL, key, "Event 2 (3x 503)", d2)

	step(6, "Destination Recovers -> Authorised Replay (POST /events/:id/replay)")
	setBehavior(client, *receiverControl, map[string]any{"status": 200})
	rp := mustPostJSON(client, fmt.Sprintf("%s/api/v1/events/%v/replay", *apiURL, ev2["id"]), key, nil,
		map[string]any{"reason": "demo: receiver recovered"}, http.StatusAccepted)
	fmt.Printf("  replayed=%v skipped=%v\n", rp["replayed"], rp["skipped"])
	waitDelivery(client, *apiURL, key, ev2["id"].(string), "delivered", 20*time.Second)
	printAttempts(client, *apiURL, key, "Event 2 after replay (cycle 1 history preserved + cycle 2)", d2)

	step(7, "Audit Trail & Receiver Statistics")
	audit := mustGetJSON(client, *apiURL+"/api/v1/audit-logs?limit=10", key)
	for _, raw := range audit["data"].([]any) {
		a := raw.(map[string]any)
		fmt.Printf("  audit action=%-18v resource=%v actor=%v\n", a["action"], a["resource_type"], a["actor_id"])
	}
	stats := mustGetJSON(client, *receiverControl+"/control/stats", "")
	fmt.Printf("  receiver: effects_applied=%v duplicates_ignored=%v auth_failures=%v total_attempts=%v\n",
		stats["effects_applied"], stats["duplicates_ignored"], stats["auth_failures"], stats["total_attempts"])

	banner("Complete: ingest -> retry -> delivery -> dead-letter -> replay verified")
}

func setBehavior(client *http.Client, base string, b map[string]any) {
	mustPostJSON(client, base+"/control/behavior", "", nil, b, http.StatusOK)
}

// waitDelivery polls event detail until its single delivery reaches want; returns the delivery ID.
func waitDelivery(client *http.Client, apiURL, key, eventID, want string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev := mustGetJSON(client, apiURL+"/api/v1/events/"+eventID, key)
		for _, raw := range ev["deliveries"].([]any) {
			d := raw.(map[string]any)
			if d["status"] == want {
				fmt.Printf("  event %s: delivery %v status=%v attempt_count=%v (event status=%v)\n",
					eventID, d["id"], d["status"], d["attempt_count"], ev["status"])
				return d["id"].(string)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	fatalf("timeout waiting for event %s delivery status %s", eventID, want)
	return ""
}
func printAttempts(client *http.Client, apiURL, apiKey, label, deliveryID string) {
	resp := mustGetJSON(client, fmt.Sprintf("%s/api/v1/deliveries/%s/attempts", apiURL, deliveryID), apiKey)
	fmt.Printf("  Attempt history for %s (delivery=%s):\n", label, deliveryID)
	for _, raw := range resp["data"].([]any) {
		a := raw.(map[string]any)
		fmt.Printf("    cycle=%v attempt_no=%v outcome=%-17v http_status=%v duration_ms=%v\n",
			a["cycle"], a["attempt_no"], a["outcome"], a["http_status"], a["duration_ms"])
	}
}

func banner(msg string) {
	fmt.Println("================================================================================")
	fmt.Println(" ", msg)
	fmt.Println("================================================================================")
}

func step(n int, title string) {
	fmt.Printf("\n[%d/7] %s\n", n, title)
}

func mustGetJSON(client *http.Client, url, apiKey string) map[string]any {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fatalf("GET %s: status %d: %s", url, resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		fatalf("decode %s: %v", url, err)
	}
	return out
}

func mustPostJSON(client *http.Client, url, apiKey string, headers map[string]string, body any, wantStatus int) map[string]any {
	out, _ := mustPostJSONWithHeaders(client, url, apiKey, headers, body, wantStatus)
	return out
}

func mustPostJSONWithHeaders(client *http.Client, url, apiKey string, headers map[string]string, body any, wantStatus int) (map[string]any, http.Header) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		fatalf("POST %s: got status %d want %d: %s", url, resp.StatusCode, wantStatus, string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		fatalf("decode %s: %v", url, err)
	}
	return out, resp.Header
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}
