package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/relayforge/relayforge/api/internal/config"
	"github.com/relayforge/relayforge/api/internal/observability"
)

type fakeChecker struct{ err error }

func (f fakeChecker) Check(context.Context) error { return f.err }

func testDeps(checker Checker) Deps {
	return Deps{
		Config: config.Config{
			Env:              "test",
			ReadinessTimeout: time.Second,
		},
		Checker: checker,
		Metrics: observability.NewMetrics("test", "test"),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func TestLivenessAndMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(testDeps(fakeChecker{}))

	for _, path := range []string{"/healthz", "/api/v1/meta"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", path, w.Code, w.Body.String())
		}
	}
}

func TestReadinessReflectsDependency(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	NewRouter(testDeps(fakeChecker{})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("ready status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	NewRouter(testDeps(fakeChecker{err: errors.New("database down")})).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d", w.Code)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Code != "not_ready" {
		t.Fatalf("problem code = %q", p.Code)
	}
	if strings.Contains(w.Body.String(), "database down") {
		t.Fatal("readiness response leaked dependency error")
	}
}

func TestRequestIDEchoAndGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(testDeps(fakeChecker{}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "client-request-1234")
	r.ServeHTTP(w, req)
	if got := w.Header().Get("X-Request-Id"); got != "client-request-1234" {
		t.Fatalf("echoed request id = %q", got)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "bad id with spaces")
	r.ServeHTTP(w, req)
	got := w.Header().Get("X-Request-Id")
	if got == "" || strings.Contains(got, " ") || len(got) != 32 {
		t.Fatalf("generated request id = %q", got)
	}
}

func TestNotFoundIsProblemJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(testDeps(fakeChecker{}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestRecoveryHidesPanicDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(testDeps(fakeChecker{}))
	r.GET("/panic", func(*gin.Context) { panic("secret panic detail") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret panic detail") {
		t.Fatal("panic detail leaked to client")
	}
}

func TestAdminMetricsRequiresToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	d := testDeps(fakeChecker{})
	d.Config.AdminToken = "admin-test-token"
	r := NewAdminRouter(d)

	for _, tc := range []struct {
		name   string
		token  string
		status int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "not-the-token", http.StatusUnauthorized},
		{"correct", "admin-test-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.token != "" {
				req.Header.Set("X-Admin-Token", tc.token)
			}
			r.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestAdminHealthRemainsOpenWithToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	d := testDeps(fakeChecker{})
	d.Config.AdminToken = "admin-test-token"
	r := NewAdminRouter(d)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestMaxBodyBytesRejectsBeforeHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(testDeps(fakeChecker{}))
	called := false
	r.POST("/limited", MaxBodyBytes(8), func(c *gin.Context) {
		called = true
		c.Status(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/limited", strings.NewReader("more than eight bytes"))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", w.Code)
	}
	if called {
		t.Fatal("handler ran despite oversized body")
	}
}
