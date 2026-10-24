package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/relayforge/relayforge/api/internal/config"
	"github.com/relayforge/relayforge/api/internal/observability"
	"github.com/relayforge/relayforge/api/internal/store"
	"github.com/relayforge/relayforge/api/openapi"
)

// TestOpenAPIMatchesRouter is the anti-drift contract test: every route the
// public listener serves must be documented, and every documented operation
// must exist.
func TestOpenAPIMatchesRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	d := Deps{
		Config: config.Config{
			Env: "test", ReadinessTimeout: time.Second, MaxEventBytes: 262144,
			EndpointLimit: 50, AdminToken: "test",
		},
		Checker: fakeChecker{},
		Metrics: observability.NewMetrics("test", "test"),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Store:   &store.Store{},
	}
	r := NewRouter(d)

	routerOps := map[string]bool{}
	for _, route := range r.Routes() {
		if route.Method == http.MethodHead || route.Method == http.MethodOptions {
			continue
		}
		routerOps[route.Method+" "+normalizeRoute(route.Path)] = true
	}

	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapi.Spec, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	specOps := map[string]bool{}
	for path, item := range doc.Paths {
		for method, raw := range item {
			switch method {
			case "get", "post", "put", "patch", "delete":
				op, _ := raw.(map[string]any)
				if id, _ := op["operationId"].(string); id == "" {
					t.Errorf("%s %s has no operationId", method, path)
				}
				if summary, _ := op["summary"].(string); summary == "" {
					t.Errorf("%s %s has no summary", method, path)
				}
				specOps[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	var missingInSpec, missingInRouter []string
	for op := range routerOps {
		if !specOps[op] {
			missingInSpec = append(missingInSpec, op)
		}
	}
	for op := range specOps {
		if !routerOps[op] {
			missingInRouter = append(missingInRouter, op)
		}
	}
	sort.Strings(missingInSpec)
	sort.Strings(missingInRouter)
	if len(missingInSpec) > 0 {
		t.Errorf("routes missing from OpenAPI: %v", missingInSpec)
	}
	if len(missingInRouter) > 0 {
		t.Errorf("OpenAPI operations without handlers: %v", missingInRouter)
	}
}

func normalizeRoute(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + strings.TrimPrefix(p, ":") + "}"
		}
	}
	return strings.Join(parts, "/")
}
