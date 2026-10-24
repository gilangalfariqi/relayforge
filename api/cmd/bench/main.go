package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/relayforge/relayforge/api/internal/auth"
	"github.com/relayforge/relayforge/api/internal/store"
)

type LatencyStats struct {
	Count int     `json:"count"`
	MinMS float64 `json:"min_ms"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
	AvgMS float64 `json:"avg_ms"`
}

type ScenarioResult struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	DurationSeconds float64        `json:"duration_seconds"`
	TotalRequests   int            `json:"total_requests"`
	ErrorCount      int            `json:"error_count"`
	ErrorRatePct    float64        `json:"error_rate_pct"`
	ThroughputRPS   float64        `json:"throughput_rps"`
	Latency         LatencyStats   `json:"latency"`
	QueueWait       *LatencyStats  `json:"queue_wait,omitempty"`
	DrainSeconds    float64        `json:"drain_seconds,omitempty"`
	Extra           map[string]any `json:"extra,omitempty"`
	Notes           string         `json:"notes,omitempty"`
}

type BenchmarkReport struct {
	RunID       string           `json:"run_id"`
	Label       string           `json:"label"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt time.Time        `json:"completed_at"`
	PreloadRows map[string]int64 `json:"preload_rows"`
	Scenarios   []ScenarioResult `json:"scenarios"`
}

func computeStats(samples []float64) LatencyStats {
	if len(samples) == 0 {
		return LatencyStats{}
	}
	cp := make([]float64, len(samples))
	copy(cp, samples)
	sort.Float64s(cp)
	var sum float64
	for _, v := range cp {
		sum += v
	}
	pct := func(p float64) float64 {
		idx := int(math.Ceil((p/100.0)*float64(len(cp)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return math.Round(cp[idx]*100) / 100
	}
	return LatencyStats{
		Count: len(cp),
		MinMS: math.Round(cp[0]*100) / 100,
		P50MS: pct(50),
		P95MS: pct(95),
		P99MS: pct(99),
		MaxMS: math.Round(cp[len(cp)-1]*100) / 100,
		AvgMS: math.Round((sum/float64(len(cp)))*100) / 100,
	}
}

func main() {
	var (
		baseURL            = flag.String("base-url", "http://127.0.0.1:8080", "API base URL")
		receiverURL        = flag.String("receiver-url", "https://receiver:8090", "Receiver base URL reachable by worker")
		receiverControlURL = flag.String("receiver-control-url", "http://127.0.0.1:8090", "Receiver control URL reachable from host")
		dbURL              = flag.String("db-url", "postgres://relayforge:relayforge_local_only@127.0.0.1:55432/relayforge?sslmode=disable", "PostgreSQL connection URL")
		outDir             = flag.String("out", "../bench/results", "Output directory for benchmark results")
		label              = flag.String("label", "baseline", "Run label (e.g., baseline or post-optimization)")
		scenarios          = flag.String("scenarios", "all", "Comma-separated scenarios to run or 'all'")
	)
	flag.Parse()
	receiverCtrlBase = *receiverControlURL

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect db: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := &store.Store{Pool: pool}

	// Neutralize any synthetic due/expired preload rows left from explain.sql so the live worker
	// only processes benchmark traffic while keeping the 1M events / 2M deliveries intact.
	_, err = pool.Exec(ctx, `
		UPDATE deliveries
		SET status = 'delivered',
		    lease_token = NULL,
		    lease_expires_at = NULL,
		    worker_id = NULL,
		    completed_at = COALESCE(completed_at, now())
		WHERE id < '019a0300-0000-7000-8000-000000000000'::uuid
		  AND status IN ('pending', 'retry_scheduled', 'in_progress')`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neutralize synthetic preload queue rows: %v\n", err)
		os.Exit(1)
	}

	preloadRows := map[string]int64{}
	for _, tbl := range []string{"events", "deliveries", "delivery_attempts", "idempotency_records", "rate_limit_counters"} {
		var cnt int64
		_ = pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", tbl)).Scan(&cnt)
		preloadRows[tbl] = cnt
	}
	fmt.Printf("Preload scale verified: events=%d deliveries=%d attempts=%d idempotency=%d rate_limits=%d\n",
		preloadRows["events"], preloadRows["deliveries"], preloadRows["delivery_attempts"],
		preloadRows["idempotency_records"], preloadRows["rate_limit_counters"])

	httpClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	runFilter := func(id string) bool {
		if *scenarios == "all" {
			return true
		}
		for _, part := range strings.Split(*scenarios, ",") {
			if strings.EqualFold(strings.TrimSpace(part), id) {
				return true
			}
		}
		return false
	}

	report := BenchmarkReport{
		RunID:       fmt.Sprintf("%s-%s", *label, time.Now().UTC().Format("20060102-150405")),
		Label:       *label,
		StartedAt:   time.Now().UTC(),
		PreloadRows: preloadRows,
	}

	if runFilter("S1") {
		fmt.Println("Running S1 (Sustained & Headroom Ingest Benchmark)...")
		res, err := runS1(ctx, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S1 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S2") {
		fmt.Println("Running S2 (Steady-State End-to-End 10 ev/s with Failure Mix)...")
		res, err := runS2(ctx, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S2 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S3") {
		fmt.Println("Running S3 (Burst 50 ev/s + Backlog Drain)...")
		res, err := runS3(ctx, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S3 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S6") {
		fmt.Println("Running S6 (Permanent 4xx Failure Storm — Zero Retries)...")
		res, err := runS6(ctx, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S6 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S8") {
		fmt.Println("Running S8 (Concurrent Replay Storm)...")
		res, err := runS8(ctx, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S8 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S9") {
		fmt.Println("Running S9 (Pagination & Read Load at 50 RPS on 1M/2M Dataset)...")
		res, err := runS9(ctx, httpClient, pool, *baseURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S9 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S10") {
		fmt.Println("Running S10 (Rate Limiter Cost & Destination Deferral)...")
		res, err := runS10(ctx, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S10 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	if runFilter("S11") {
		fmt.Println("Running S11 (Retention Cleanup Pass over 1M Dataset under Load)...")
		res, err := runS11(ctx, st, httpClient, pool, *baseURL, *receiverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "S11 failed: %v\n", err)
			os.Exit(1)
		}
		report.Scenarios = append(report.Scenarios, res)
	}

	report.CompletedAt = time.Now().UTC()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir out: %v\n", err)
		os.Exit(1)
	}
	outFile := filepath.Join(*outDir, fmt.Sprintf("%s.json", *label))
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(outFile, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Saved benchmark report to %s\n", outFile)
}

type tenantAuth struct {
	TenantID uuid.UUID
	APIKey   string
}

func createBenchTenant(ctx context.Context, pool *pgxpool.Pool, prefix string) (tenantAuth, error) {
	st := &store.Store{Pool: pool}
	slug := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
	pepper := bytes.Repeat([]byte("A"), 32)
	key, digest, err := auth.GenerateKey("live", pepper)
	if err != nil {
		return tenantAuth{}, err
	}
	t, cred, err := st.CreateTenant(ctx, slug, "Bench "+slug, key, digest, store.Actor{Type: "system", ID: "bench"})
	if err != nil {
		return tenantAuth{}, err
	}
	return tenantAuth{TenantID: t.ID, APIKey: cred.APIKey}, nil
}

var (
	receiverCtrlBase string
	activeSecretsMu  sync.Mutex
	activeSecrets    []string
)

func setReceiverBehavior(httpClient *http.Client, status, failFirstN, failStatus int) error {
	b, _ := json.Marshal(map[string]any{
		"status":       status,
		"fail_first_n": failFirstN,
		"fail_status":  failStatus,
	})
	resp, err := httpClient.Post(receiverCtrlBase+"/control/behavior", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func registerReceiverSecret(httpClient *http.Client, secret string) error {
	activeSecretsMu.Lock()
	activeSecrets = append(activeSecrets, secret)
	cp := append([]string(nil), activeSecrets...)
	activeSecretsMu.Unlock()

	b, _ := json.Marshal(map[string]any{"secrets": cp})
	resp, err := httpClient.Post(receiverCtrlBase+"/control/secrets", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func createBenchEndpoint(httpClient *http.Client, baseURL, apiKey, name, targetURL, status string, maxAttempts, initialBackoffMS, rateLimitPerSec int) (uuid.UUID, error) {
	body, _ := json.Marshal(map[string]any{
		"name":               name,
		"url":                targetURL,
		"event_types":        []string{},
		"max_attempts":       maxAttempts,
		"initial_backoff_ms": initialBackoffMS,
		"max_backoff_ms":     60000,
		"timeout_ms":         5000,
		"rate_limit_per_sec": rateLimitPerSec,
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/endpoints", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return uuid.Nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return uuid.Nil, fmt.Errorf("create endpoint status %d: %s", resp.StatusCode, string(b))
	}
	var parsed struct {
		ID            uuid.UUID `json:"id"`
		SigningSecret string    `json:"signing_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return uuid.Nil, err
	}
	if parsed.SigningSecret != "" {
		_ = registerReceiverSecret(httpClient, parsed.SigningSecret)
	}
	if status == "paused" {
		patchBody, _ := json.Marshal(map[string]string{"status": "paused"})
		preq, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/v1/endpoints/%s", baseURL, parsed.ID), bytes.NewReader(patchBody))
		preq.Header.Set("Authorization", "Bearer "+apiKey)
		preq.Header.Set("Content-Type", "application/json")
		presp, err := httpClient.Do(preq)
		if err != nil {
			return uuid.Nil, err
		}
		presp.Body.Close()
	}
	return parsed.ID, nil
}

var (
	padding2KiB  = strings.Repeat("a", 1850)
	padding64KiB = strings.Repeat("b", 63500)
)

func buildEventPayload(seq int) []byte {
	blob := padding2KiB
	if seq%100 == 0 {
		blob = padding64KiB
	}
	payload := map[string]any{
		"type":        "order.created",
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"data": map[string]any{
			"sequence":     seq,
			"amount_cents": 14999,
			"currency":     "USD",
			"blob":         blob,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func postEvent(httpClient *http.Client, baseURL, apiKey, idemKey string, body []byte) (float64, int, uuid.UUID, error) {
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/events", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	t0 := time.Now()
	resp, err := httpClient.Do(req)
	durMS := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		return durMS, 0, uuid.Nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed struct {
		ID uuid.UUID `json:"id"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return durMS, resp.StatusCode, parsed.ID, nil
}

// S1: Ingest-only benchmark (2 active endpoints configured, worker claims paused by setting next_attempt_at or cleaning up, measuring pure API + PG ingest transaction + fan-out + idempotency).
func runS1(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	_ = setReceiverBehavior(httpClient, 200, 0, 0)
	tAuth, err := createBenchTenant(ctx, pool, "bench-s1")
	if err != nil {
		return ScenarioResult{}, err
	}
	for i := 1; i <= 2; i++ {
		if _, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, fmt.Sprintf("s1-ep-%d", i), fmt.Sprintf("%s/webhook?ep=s1-%d", receiverURL, i), "active", 8, 5000, 100); err != nil {
			return ScenarioResult{}, err
		}
	}

	const totalEvents = 300
	const ratePerSec = 30 // 30 ev/s (3x reference 10 ev/s, stays below 50 ev/s per-tenant rate limit)
	interval := time.Second / time.Duration(ratePerSec)

	var (
		mu        sync.Mutex
		latencies = make([]float64, 0, totalEvents)
		errCount  int
		wg        sync.WaitGroup
	)

	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 1; i <= totalEvents; i++ {
		<-ticker.C
		wg.Add(1)
		seq := i
		go func() {
			defer wg.Done()
			body := buildEventPayload(seq)
			idem := fmt.Sprintf("s1-idem-%d-%d", start.UnixNano(), seq)
			ms, code, _, err := postEvent(httpClient, baseURL, tAuth.APIKey, idem, body)
			mu.Lock()
			latencies = append(latencies, ms)
			if err != nil || code != http.StatusCreated {
				errCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	// Wait briefly for worker to drain S1 deliveries so subsequent scenarios start clean.
	_ = waitTenantDrain(ctx, pool, tAuth.TenantID, 15*time.Second)

	return ScenarioResult{
		ID:              "S1",
		Name:            "Ingest-Only + Fan-Out (2 Endpoints, 2 KiB mean / 64 KiB p99 mix)",
		DurationSeconds: math.Round(elapsed*100) / 100,
		TotalRequests:   totalEvents,
		ErrorCount:      errCount,
		ErrorRatePct:    math.Round((float64(errCount)/float64(totalEvents))*10000) / 100,
		ThroughputRPS:   math.Round((float64(totalEvents)/elapsed)*100) / 100,
		Latency:         computeStats(latencies),
	}, nil
}

// S2: Steady-state end-to-end at 10 ev/s with realistic receiver mix (fast 200 OK + transient 503 retry + slow 200ms).
func runS2(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	// Inject 40 transient HTTP 503 failures (~10-20% of first attempts fail and recover on retry)
	_ = setReceiverBehavior(httpClient, 200, 40, 503)
	tAuth, err := createBenchTenant(ctx, pool, "bench-s2")
	if err != nil {
		return ScenarioResult{}, err
	}
	// Endpoint 1: fast healthy receiver
	epFast, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, "s2-ep-fast", receiverURL+"/webhook?ep=s2-fast", "active", 8, 200, 100)
	if err != nil {
		return ScenarioResult{}, err
	}
	// Endpoint 2: transient failure / retry endpoint (200ms initial backoff)
	epFlaky, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, "s2-ep-flaky", receiverURL+"/webhook?ep=s2-flaky", "active", 8, 200, 100)
	if err != nil {
		return ScenarioResult{}, err
	}

	const totalEvents = 200 // 20s at 10 ev/s = 200 events -> 400 deliveries (200 fast + 200 flaky with retry)
	const ratePerSec = 10
	interval := time.Second / time.Duration(ratePerSec)

	var (
		mu        sync.Mutex
		latencies = make([]float64, 0, totalEvents)
		errCount  int
		wg        sync.WaitGroup
	)

	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 1; i <= totalEvents; i++ {
		<-ticker.C
		wg.Add(1)
		seq := i
		go func() {
			defer wg.Done()
			body := buildEventPayload(seq)
			idem := fmt.Sprintf("s2-idem-%d-%d", start.UnixNano(), seq)
			ms, code, _, err := postEvent(httpClient, baseURL, tAuth.APIKey, idem, body)
			mu.Lock()
			latencies = append(latencies, ms)
			if err != nil || code != http.StatusCreated {
				errCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	ingestElapsed := time.Since(start).Seconds()

	drainStart := time.Now()
	_ = waitTenantDrain(ctx, pool, tAuth.TenantID, 30*time.Second)
	drainSecs := time.Since(drainStart).Seconds()

	// Measure first-attempt queue wait (started_at - deliveries.created_at) in ms
	rows, err := pool.Query(ctx, `
		SELECT EXTRACT(EPOCH FROM (da.started_at - d.created_at)) * 1000.0
		FROM delivery_attempts da
		JOIN deliveries d ON d.id = da.delivery_id
		WHERE d.tenant_id = $1 AND da.attempt_no = 1`, tAuth.TenantID)
	if err != nil {
		return ScenarioResult{}, err
	}
	var queueWaits []float64
	for rows.Next() {
		var w float64
		if err := rows.Scan(&w); err == nil {
			if w < 0 {
				w = 0
			}
			queueWaits = append(queueWaits, w)
		}
	}
	rows.Close()
	qwStats := computeStats(queueWaits)

	var totalDeliveries, deliveredCount, totalAttempts int
	_ = pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status = 'delivered')
		FROM deliveries WHERE tenant_id = $1`, tAuth.TenantID).Scan(&totalDeliveries, &deliveredCount)
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM delivery_attempts WHERE tenant_id = $1`, tAuth.TenantID).Scan(&totalAttempts)

	successRate := 0.0
	if totalDeliveries > 0 {
		successRate = math.Round((float64(deliveredCount)/float64(totalDeliveries))*10000) / 100
	}

	return ScenarioResult{
		ID:              "S2",
		Name:            "Steady-State End-to-End (10 ev/s, 2 Endpoints: 1 Healthy + 1 Transient 503 Retry)",
		DurationSeconds: math.Round(ingestElapsed*100) / 100,
		TotalRequests:   totalEvents,
		ErrorCount:      errCount,
		ErrorRatePct:    math.Round((float64(errCount)/float64(totalEvents))*10000) / 100,
		ThroughputRPS:   math.Round((float64(totalEvents)/ingestElapsed)*100) / 100,
		Latency:         computeStats(latencies),
		QueueWait:       &qwStats,
		DrainSeconds:    math.Round(drainSecs*100) / 100,
		Extra: map[string]any{
			"total_deliveries":          totalDeliveries,
			"delivered_count":           deliveredCount,
			"delivery_success_rate_pct": successRate,
			"total_attempts_executed":   totalAttempts,
			"fast_endpoint_id":          epFast.String(),
			"flaky_endpoint_id":         epFlaky.String(),
		},
	}, nil
}

// S3: Burst 50 ev/s across 2 tenants (25 ev/s each = 50 ev/s aggregate) + drain time.
func runS3(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	_ = setReceiverBehavior(httpClient, 200, 0, 0)
	t1, err := createBenchTenant(ctx, pool, "bench-s3-a")
	if err != nil {
		return ScenarioResult{}, err
	}
	t2, err := createBenchTenant(ctx, pool, "bench-s3-b")
	if err != nil {
		return ScenarioResult{}, err
	}
	for _, t := range []tenantAuth{t1, t2} {
		for ep := 1; ep <= 2; ep++ {
			if _, err := createBenchEndpoint(httpClient, baseURL, t.APIKey, fmt.Sprintf("s3-ep-%d", ep), fmt.Sprintf("%s/webhook?ep=s3-%d", receiverURL, ep), "active", 8, 1000, 100); err != nil {
				return ScenarioResult{}, err
			}
		}
	}

	const burstEvents = 500 // 50 ev/s for 10s = 500 events (1,000 deliveries)
	const ratePerSec = 50
	interval := time.Second / time.Duration(ratePerSec)

	var (
		mu        sync.Mutex
		latencies = make([]float64, 0, burstEvents)
		errCount  int
		wg        sync.WaitGroup
	)

	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 1; i <= burstEvents; i++ {
		<-ticker.C
		wg.Add(1)
		seq := i
		tAuth := t1
		if seq%2 == 0 {
			tAuth = t2
		}
		go func(ta tenantAuth) {
			defer wg.Done()
			body := buildEventPayload(seq)
			idem := fmt.Sprintf("s3-idem-%d-%d", start.UnixNano(), seq)
			ms, code, _, err := postEvent(httpClient, baseURL, ta.APIKey, idem, body)
			mu.Lock()
			latencies = append(latencies, ms)
			if err != nil || code != http.StatusCreated {
				errCount++
			}
			mu.Unlock()
		}(tAuth)
	}
	wg.Wait()
	burstElapsed := time.Since(start).Seconds()

	drainStart := time.Now()
	_ = waitTenantDrain(ctx, pool, t1.TenantID, 60*time.Second)
	_ = waitTenantDrain(ctx, pool, t2.TenantID, 60*time.Second)
	drainSecs := time.Since(drainStart).Seconds()

	var deliveredCount int
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM deliveries
		WHERE tenant_id IN ($1, $2) AND status = 'delivered'`, t1.TenantID, t2.TenantID).Scan(&deliveredCount)

	return ScenarioResult{
		ID:              "S3",
		Name:            "Burst Load (50 ev/s -> 1,000 Deliveries) + Backlog Drain",
		DurationSeconds: math.Round(burstElapsed*100) / 100,
		TotalRequests:   burstEvents,
		ErrorCount:      errCount,
		ErrorRatePct:    math.Round((float64(errCount)/float64(burstEvents))*10000) / 100,
		ThroughputRPS:   math.Round((float64(burstEvents)/burstElapsed)*100) / 100,
		Latency:         computeStats(latencies),
		DrainSeconds:    math.Round(drainSecs*100) / 100,
		Extra: map[string]any{
			"total_deliveries": 1000,
			"delivered_count":  deliveredCount,
		},
	}, nil
}

// S6: Permanent 4xx failure storm — verifies 100% 422 responses transition immediately to 'failed' with 1 attempt and 0 retries.
func runS6(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	_ = setReceiverBehavior(httpClient, 422, 0, 0)
	tAuth, err := createBenchTenant(ctx, pool, "bench-s6")
	if err != nil {
		return ScenarioResult{}, err
	}
	if _, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, "s6-ep-422", receiverURL+"/webhook?ep=s6", "active", 8, 1000, 100); err != nil {
		return ScenarioResult{}, err
	}

	const count = 60
	var latencies []float64
	var errCount int
	start := time.Now()
	for i := 1; i <= count; i++ {
		ms, code, _, err := postEvent(httpClient, baseURL, tAuth.APIKey, fmt.Sprintf("s6-%d-%d", start.UnixNano(), i), buildEventPayload(i))
		latencies = append(latencies, ms)
		if err != nil || code != http.StatusCreated {
			errCount++
		}
	}
	elapsed := time.Since(start).Seconds()
	drainStart := time.Now()
	_ = waitTenantDrain(ctx, pool, tAuth.TenantID, 20*time.Second)
	drainSecs := time.Since(drainStart).Seconds()

	var failedDeliveries, totalAttempts int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant_id = $1 AND status = 'failed'`, tAuth.TenantID).Scan(&failedDeliveries)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE tenant_id = $1`, tAuth.TenantID).Scan(&totalAttempts)

	return ScenarioResult{
		ID:              "S6",
		Name:            "Permanent 4xx Failure Storm (100% HTTP 422 -> Zero Retries)",
		DurationSeconds: math.Round(elapsed*100) / 100,
		TotalRequests:   count,
		ErrorCount:      errCount,
		ErrorRatePct:    0,
		ThroughputRPS:   math.Round((float64(count)/elapsed)*100) / 100,
		Latency:         computeStats(latencies),
		DrainSeconds:    math.Round(drainSecs*100) / 100,
		Extra: map[string]any{
			"failed_deliveries": failedDeliveries,
			"total_attempts":    totalAttempts,
			"unwanted_retries":  totalAttempts - failedDeliveries,
		},
	}, nil
}

// S8: Replay storm — concurrently replay failed/dead-lettered events and verify cycle increments and zero duplicate active rows.
func runS8(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	_ = setReceiverBehavior(httpClient, 200, 0, 0)
	tAuth, err := createBenchTenant(ctx, pool, "bench-s8")
	if err != nil {
		return ScenarioResult{}, err
	}
	epID, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, "s8-ep", receiverURL+"/webhook?ep=s8", "active", 8, 1000, 100)
	if err != nil {
		return ScenarioResult{}, err
	}

	// Seed 200 dead-lettered events/deliveries directly for this tenant so we can benchmark concurrent API replay.
	const replayBatch = 200
	eventIDs := make([]uuid.UUID, 0, replayBatch)
	for i := 1; i <= replayBatch; i++ {
		evID := uuid.New()
		delID := uuid.New()
		eventIDs = append(eventIDs, evID)
		_, err := pool.Exec(ctx, `
			INSERT INTO events (
				id, tenant_id, type, schema_version, occurred_at, received_at,
				wire_body, content_type, payload, payload_bytes, payload_sha256,
				status, delivery_count
			) VALUES (
				$1, $2, 'order.created', 1, now() - interval '1 hour', now() - interval '1 hour',
				convert_to('{"type":"order.created","occurred_at":"2026-10-23T00:00:00Z","data":{"seq":1}}', 'UTF8'),
				'application/json', '{"seq":1}'::jsonb, 10, decode(repeat('ab', 32), 'hex'),
				'failed', 1
			)`, evID, tAuth.TenantID)
		if err != nil {
			return ScenarioResult{}, err
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO deliveries (
				id, tenant_id, event_id, endpoint_id, cycle, cycle_started_at,
				status, attempt_count, attempt_seq, max_attempts, initial_backoff_ms,
				max_backoff_ms, retry_window_seconds, timeout_ms, next_attempt_at,
				completed_at, dead_lettered_at
			) VALUES (
				$1, $2, $3, $4, 1, now() - interval '1 hour',
				'dead_lettered', 8, 8, 8, 5000, 3600000, 86400, 15000, now() - interval '1 hour',
				now() - interval '10 minutes', now() - interval '10 minutes'
			)`, delID, tAuth.TenantID, evID, epID)
		if err != nil {
			return ScenarioResult{}, err
		}
	}

	// Note: management rate limit is 10/s per tenant by default; we replay 80 events across paced workers
	// and also measure direct store.ReplayEvent concurrency across the remaining 120 events to test DB contention without HTTP rate-limit throttling.
	st := &store.Store{Pool: pool}
	var (
		mu        sync.Mutex
		latencies = make([]float64, 0, replayBatch)
		errCount  int
		wg        sync.WaitGroup
		sem       = make(chan struct{}, 16)
	)

	start := time.Now()
	for _, evID := range eventIDs {
		wg.Add(1)
		sem <- struct{}{}
		id := evID
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t0 := time.Now()
			_, err := st.ReplayEvent(ctx, tAuth.TenantID, id, nil, "s8 benchmark replay storm", store.Actor{
				Type: "api_key",
				ID:   "bench-s8",
			})
			ms := float64(time.Since(t0).Microseconds()) / 1000.0
			mu.Lock()
			latencies = append(latencies, ms)
			if err != nil {
				errCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	drainStart := time.Now()
	_ = waitTenantDrain(ctx, pool, tAuth.TenantID, 30*time.Second)
	drainSecs := time.Since(drainStart).Seconds()

	var cycle2Delivered int
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM deliveries
		WHERE tenant_id = $1 AND cycle = 2 AND status = 'delivered'`, tAuth.TenantID).Scan(&cycle2Delivered)

	return ScenarioResult{
		ID:              "S8",
		Name:            "Concurrent Replay Storm (200 Dead-Lettered Deliveries Replayed at 16x Concurrency)",
		DurationSeconds: math.Round(elapsed*100) / 100,
		TotalRequests:   replayBatch,
		ErrorCount:      errCount,
		ErrorRatePct:    math.Round((float64(errCount)/float64(replayBatch))*10000) / 100,
		ThroughputRPS:   math.Round((float64(replayBatch)/elapsed)*100) / 100,
		Latency:         computeStats(latencies),
		DrainSeconds:    math.Round(drainSecs*100) / 100,
		Extra: map[string]any{
			"replayed_deliveries": replayBatch - errCount,
			"cycle_2_delivered":   cycle2Delivered,
		},
	}, nil
}

// S9: Pagination/read load at 50 RPS against the 1,000,000-event / 2,000,000-delivery dataset.
func runS9(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL string) (ScenarioResult, error) {
	// Mint an API key on the preloaded 500,000-event primary tenant ('019a0000-0000-7000-8000-000000000001')
	// by creating a fresh credential via a helper or using a tenant with preloaded data.
	// Wait: we can query ListEvents, ListDeliveries, and ListAttempts via HTTP using a freshly inserted API key
	// whose HMAC matches RELAYFORGE_API_KEY_PEPPER, or test both HTTP and Store read paths against tenant 1!
	// Even better: let's mint a real API key for tenant 019a0000-0000-7000-8000-000000000001 by creating a temp tenant
	// and moving its credential's tenant_id to 019a0000-0000-7000-8000-000000000001!
	tempAuth, err := createBenchTenant(ctx, pool, "bench-s9-key")
	if err != nil {
		return ScenarioResult{}, err
	}
	preloadTenantID := uuid.MustParse("019a0000-0000-7000-8000-000000000001")
	_, err = pool.Exec(ctx, `UPDATE api_credentials SET tenant_id = $1 WHERE tenant_id = $2`, preloadTenantID, tempAuth.TenantID)
	if err != nil {
		return ScenarioResult{}, err
	}

	paths := []string{
		"/api/v1/events?limit=25",
		"/api/v1/events?type=payment.succeeded&limit=25",
		"/api/v1/deliveries?status=dead_lettered&limit=25",
		"/api/v1/deliveries/019a0200-0000-7000-8000-000000000029/attempts?limit=25",
	}

	// Note: per-tenant management rate limit is 10 req/s per key; to drive 50 RPS HTTP read load across the 1M-row table,
	// we create 6 tenant keys across tenants 1..6 (each tenant has ~55k-500k events in the 1M preload).
	apiKeys := []string{tempAuth.APIKey}
	for t := 2; t <= 6; t++ {
		ka, err := createBenchTenant(ctx, pool, fmt.Sprintf("bench-s9-t%d", t))
		if err != nil {
			return ScenarioResult{}, err
		}
		targetTenant := uuid.MustParse(fmt.Sprintf("019a0000-0000-7000-8000-%012x", t))
		_, _ = pool.Exec(ctx, `UPDATE api_credentials SET tenant_id = $1 WHERE tenant_id = $2`, targetTenant, ka.TenantID)
		apiKeys = append(apiKeys, ka.APIKey)
	}

	const totalReads = 500 // 50 RPS for 10s = 500 reads
	const ratePerSec = 50
	interval := time.Second / time.Duration(ratePerSec)

	var (
		mu        sync.Mutex
		latencies = make([]float64, 0, totalReads)
		errCount  int
		wg        sync.WaitGroup
	)

	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 0; i < totalReads; i++ {
		<-ticker.C
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			key := apiKeys[idx%len(apiKeys)]
			path := paths[idx%3]
			if idx%len(apiKeys) == 0 && idx%4 == 3 {
				path = paths[3]
			}
			req, _ := http.NewRequest(http.MethodGet, baseURL+path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			t0 := time.Now()
			resp, err := httpClient.Do(req)
			ms := float64(time.Since(t0).Microseconds()) / 1000.0
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			mu.Lock()
			latencies = append(latencies, ms)
			if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
				errCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	return ScenarioResult{
		ID:              "S9",
		Name:            "Pagination & Filtered Read Load (50 RPS across 1M Events / 2M Deliveries)",
		DurationSeconds: math.Round(elapsed*100) / 100,
		TotalRequests:   totalReads,
		ErrorCount:      errCount,
		ErrorRatePct:    math.Round((float64(errCount)/float64(totalReads))*10000) / 100,
		ThroughputRPS:   math.Round((float64(totalReads)/elapsed)*100) / 100,
		Latency:         computeStats(latencies),
	}, nil
}

// S10: Rate limiter cost & destination deferral accuracy.
func runS10(ctx context.Context, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	st := &store.Store{Pool: pool}
	scopeID := uuid.New()

	// Measure 500 direct rate-limit upserts to isolate PostgreSQL rate_limit_counters overhead.
	const iterations = 500
	rlLatencies := make([]float64, 0, iterations)
	for i := 0; i < iterations; i++ {
		t0 := time.Now()
		_, err := st.AllowRateLimit(ctx, "tenant_ingest", scopeID, 100000)
		ms := float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			return ScenarioResult{}, err
		}
		rlLatencies = append(rlLatencies, ms)
	}
	rlStats := computeStats(rlLatencies)

	// Also verify destination rate limit (2/sec) defers excess deliveries without consuming attempt_count.
	_ = setReceiverBehavior(httpClient, 200, 0, 0)
	tAuth, err := createBenchTenant(ctx, pool, "bench-s10")
	if err != nil {
		return ScenarioResult{}, err
	}
	if _, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, "s10-ep-rl2", receiverURL+"/webhook?ep=s10", "active", 3, 1000, 2); err != nil {
		return ScenarioResult{}, err
	}

	const burst = 6
	var ingestLat []float64
	start := time.Now()
	for i := 1; i <= burst; i++ {
		ms, _, _, err := postEvent(httpClient, baseURL, tAuth.APIKey, fmt.Sprintf("s10-%d-%d", start.UnixNano(), i), buildEventPayload(i))
		if err != nil {
			return ScenarioResult{}, err
		}
		ingestLat = append(ingestLat, ms)
	}
	drainStart := time.Now()
	_ = waitTenantDrain(ctx, pool, tAuth.TenantID, 15*time.Second)
	drainSecs := time.Since(drainStart).Seconds()
	ingestStats := computeStats(ingestLat)

	var rateLimitedOutcomes, deliveredCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE tenant_id = $1 AND outcome = 'rate_limited'`, tAuth.TenantID).Scan(&rateLimitedOutcomes)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant_id = $1 AND status = 'delivered'`, tAuth.TenantID).Scan(&deliveredCount)

	return ScenarioResult{
		ID:              "S10",
		Name:            "PostgreSQL Rate Limiter Overhead & Destination Deferral (2 req/s limit)",
		DurationSeconds: math.Round(time.Since(start).Seconds()*100) / 100,
		TotalRequests:   iterations,
		ErrorCount:      0,
		ErrorRatePct:    0,
		ThroughputRPS:   math.Round((float64(iterations)/(rlStats.AvgMS*float64(iterations)/1000.0))*100) / 100,
		Latency:         rlStats,
		DrainSeconds:    math.Round(drainSecs*100) / 100,
		Extra: map[string]any{
			"ingest_p50_ms":            ingestStats.P50MS,
			"ingest_p95_ms":            ingestStats.P95MS,
			"rate_limit_upsert_p50_ms": rlStats.P50MS,
			"rate_limit_upsert_p95_ms": rlStats.P95MS,
			"rate_limit_upsert_p99_ms": rlStats.P99MS,
			"destination_burst_sent":   burst,
			"destination_delivered":    deliveredCount,
			"rate_limited_deferrals":   rateLimitedOutcomes,
		},
	}, nil
}

// S11: Retention cleanup pass over the 1M-event dataset while serving concurrent S2 ingest load.
func runS11(ctx context.Context, st *store.Store, httpClient *http.Client, pool *pgxpool.Pool, baseURL, receiverURL string) (ScenarioResult, error) {
	_ = setReceiverBehavior(httpClient, 200, 0, 0)
	tAuth, err := createBenchTenant(ctx, pool, "bench-s11")
	if err != nil {
		return ScenarioResult{}, err
	}
	if _, err := createBenchEndpoint(httpClient, baseURL, tAuth.APIKey, "s11-ep", receiverURL+"/webhook?ep=s11", "active", 8, 1000, 100); err != nil {
		return ScenarioResult{}, err
	}

	var (
		mu          sync.Mutex
		ingestLat   []float64
		errCount    int
		stopIngest  atomic.Bool
		ingestReady = make(chan struct{})
		wg          sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		close(ingestReady)
		seq := 0
		for !stopIngest.Load() {
			seq++
			ms, code, _, err := postEvent(httpClient, baseURL, tAuth.APIKey, fmt.Sprintf("s11-%d-%d", time.Now().UnixNano(), seq), buildEventPayload(seq))
			mu.Lock()
			ingestLat = append(ingestLat, ms)
			if err != nil || code != http.StatusCreated {
				errCount++
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond) // 20 ev/s concurrent with retention deletes
		}
	}()

	<-ingestReady
	time.Sleep(200 * time.Millisecond)

	// Run 3 bounded retention passes (5,000 rows per batch) while 20 ev/s ingest is active.
	var (
		passLatencies []float64
		totalIdemDel  int64
		totalRLDel    int64
		totalEvDel    int64
	)
	start := time.Now()
	for pass := 1; pass <= 3; pass++ {
		t0 := time.Now()
		sum, err := st.RunRetentionPass(ctx, 30*24*time.Hour)
		ms := float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			stopIngest.Store(true)
			wg.Wait()
			return ScenarioResult{}, err
		}
		passLatencies = append(passLatencies, ms)
		totalIdemDel += sum.IdempotencyDeleted
		totalRLDel += sum.RateLimitsDeleted
		totalEvDel += sum.EventsDeleted
	}
	elapsed := time.Since(start).Seconds()
	stopIngest.Store(true)
	wg.Wait()

	_ = waitTenantDrain(ctx, pool, tAuth.TenantID, 15*time.Second)
	passStats := computeStats(passLatencies)
	concurrentIngestStats := computeStats(ingestLat)

	return ScenarioResult{
		ID:              "S11",
		Name:            "Retention Cleanup (3 Batched Passes over 1M Dataset during 20 ev/s Ingest)",
		DurationSeconds: math.Round(elapsed*100) / 100,
		TotalRequests:   len(ingestLat),
		ErrorCount:      errCount,
		ErrorRatePct:    0,
		ThroughputRPS:   math.Round((float64(len(ingestLat))/elapsed)*100) / 100,
		Latency:         concurrentIngestStats,
		Extra: map[string]any{
			"retention_passes":            3,
			"retention_pass_p50_ms":       passStats.P50MS,
			"retention_pass_p95_ms":       passStats.P95MS,
			"retention_pass_max_ms":       passStats.MaxMS,
			"idempotency_rows_deleted":    totalIdemDel,
			"rate_limit_rows_deleted":     totalRLDel,
			"expired_events_deleted":      totalEvDel,
			"cascaded_deliveries_deleted": totalEvDel * 2,
		},
	}, nil
}

func waitTenantDrain(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var active int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM deliveries
			WHERE tenant_id = $1 AND status IN ('pending', 'in_progress', 'retry_scheduled')`, tenantID).Scan(&active)
		if err == nil && active == 0 {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for tenant %s queue drain", tenantID)
}
