package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/relayforge/relayforge/api/internal/config"
	"github.com/relayforge/relayforge/api/internal/jsonschema"
	"github.com/relayforge/relayforge/api/internal/observability"
	"github.com/relayforge/relayforge/api/internal/ssrf"
	"github.com/relayforge/relayforge/api/internal/store"
	"github.com/relayforge/relayforge/api/internal/version"
	"github.com/relayforge/relayforge/api/openapi"
)

// Checker is the readiness dependency contract.
type Checker interface {
	Check(context.Context) error
}

// Deps are the router dependencies.
type Deps struct {
	Config   config.Config
	Checker  Checker
	Metrics  *observability.Metrics
	Logger   *slog.Logger
	Store    *store.Store
	Schemas  *jsonschema.Validator
	SSRF     *ssrf.Validator
	Signup   *signupLimiter
	TokenUse *touchCache
}

// NewRouter builds the public listener.
func NewRouter(d Deps) *gin.Engine {
	if d.Config.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	_ = r.SetTrustedProxies(nil)
	r.Use(RequestID(), Recovery(d.Logger), AccessLog(d.Logger), Metrics(d.Metrics))

	r.GET("/healthz", liveness)
	r.GET("/readyz", readiness(d))
	r.GET("/api/v1/meta", meta(d))
	r.GET("/api/v1/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", openapi.Spec)
	})

	if d.Store != nil {
		registerTenantRoutes(r, d)
	}

	r.NoRoute(func(c *gin.Context) {
		writeProblem(c, http.StatusNotFound, "not_found", "Not found", "No route matches this request.")
	})
	return r
}

func registerTenantRoutes(r *gin.Engine, d Deps) {
	if d.Schemas == nil {
		d.Schemas = jsonschema.New()
	}
	if d.SSRF == nil {
		d.SSRF = ssrf.New()
	}
	if d.Signup == nil {
		d.Signup = newSignupLimiter(5, time.Minute)
	}
	if d.TokenUse == nil {
		d.TokenUse = newTouchCache(time.Minute)
	}

	v1 := r.Group("/api/v1")
	v1.POST("/tenants", MaxBodyBytes(16<<10), signupHandler(d))

	authed := v1.Group("")
	authed.Use(Authenticate(d))
	{
		mgmtRL := RateLimitTenant(d, "tenant_management", d.Config.RateLimitManagementPerSec)
		ingestRL := RateLimitTenant(d, "tenant_ingest", d.Config.RateLimitIngestPerSec)

		authed.GET("/tenant", mgmtRL, RequireAction("tenant.read"), tenantGet(d))
		authed.PATCH("/tenant", mgmtRL, RequireAction("tenant.write"), MaxBodyBytes(16<<10), tenantUpdate(d))

		authed.GET("/credentials", mgmtRL, RequireAction("credentials.write"), credentialsList(d))
		authed.POST("/credentials", mgmtRL, RequireAction("credentials.write"), MaxBodyBytes(16<<10), credentialsCreate(d))
		authed.POST("/credentials/:id/rotate", mgmtRL, RequireAction("credentials.write"), MaxBodyBytes(8<<10), credentialsRotate(d))
		authed.DELETE("/credentials/:id", mgmtRL, RequireAction("credentials.write"), credentialsRevoke(d))

		authed.GET("/endpoints", mgmtRL, RequireAction("endpoints.read"), endpointsList(d))
		authed.POST("/endpoints", mgmtRL, RequireAction("endpoints.write"), MaxBodyBytes(32<<10), endpointsCreate(d))
		authed.GET("/endpoints/:id", mgmtRL, RequireAction("endpoints.read"), endpointsGet(d))
		authed.PATCH("/endpoints/:id", mgmtRL, RequireAction("endpoints.write"), MaxBodyBytes(32<<10), endpointsUpdate(d))
		authed.DELETE("/endpoints/:id", mgmtRL, RequireAction("endpoints.write"), endpointsDelete(d))
		authed.GET("/endpoints/:id/secrets", mgmtRL, RequireAction("secrets.write"), secretsList(d))
		authed.POST("/endpoints/:id/secrets", mgmtRL, RequireAction("secrets.write"), MaxBodyBytes(8<<10), secretsCreate(d))
		authed.DELETE("/endpoints/:id/secrets/:secret_id", mgmtRL, RequireAction("secrets.write"), secretsRetire(d))

		authed.POST("/events", ingestRL, RequireAction("events.write"), MaxBodyBytes(d.Config.MaxEventBytes), eventsCreate(d))
		authed.GET("/events", mgmtRL, RequireAction("events.read"), eventsList(d))
		authed.GET("/events/:id", mgmtRL, RequireAction("events.read"), eventsGet(d))
		authed.POST("/events/:id/replay", mgmtRL, RequireAction("replay"), MaxBodyBytes(16<<10), eventsReplay(d))

		authed.GET("/deliveries", mgmtRL, RequireAction("deliveries.read"), deliveriesList(d))
		authed.GET("/deliveries/:id", mgmtRL, RequireAction("deliveries.read"), deliveriesGet(d))
		authed.GET("/deliveries/:id/attempts", mgmtRL, RequireAction("deliveries.read"), attemptsList(d))

		authed.GET("/schemas", mgmtRL, RequireAction("schemas.read"), schemasList(d))
		authed.POST("/schemas", mgmtRL, RequireAction("schemas.write"), MaxBodyBytes(72<<10), schemasCreate(d))
		authed.DELETE("/schemas/:id", mgmtRL, RequireAction("schemas.write"), schemasDelete(d))

		authed.GET("/audit-logs", mgmtRL, RequireAction("audit.read"), auditList(d))
	}
}

// NewAdminRouter builds the internal listener.
func NewAdminRouter(d Deps) *gin.Engine {
	if d.Config.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	_ = r.SetTrustedProxies(nil)
	r.Use(RequestID(), Recovery(d.Logger), AccessLog(d.Logger))
	r.GET("/healthz", liveness)
	r.GET("/readyz", readiness(d))
	if d.Metrics != nil {
		r.GET("/metrics", AdminAuth(d.Config.AdminToken), gin.WrapH(d.Metrics.Handler()))
	}
	if d.Store != nil {
		admin := r.Group("/internal/v1/admin", AdminAuth(d.Config.AdminToken), RateLimitAdmin(d, d.Config.RateLimitAdminPerSec))
		admin.GET("/tenants", adminListTenants(d))
		admin.POST("/tenants/:id/suspend", MaxBodyBytes(8<<10), adminSuspendTenant(d))
		admin.POST("/tenants/:id/resume", adminResumeTenant(d))
		admin.GET("/queue", adminQueueSummary(d))
		admin.GET("/deliveries/stuck", adminStuckDeliveries(d))
		admin.POST("/deliveries/:id/replay", MaxBodyBytes(8<<10), adminReplayDelivery(d))
	}
	r.NoRoute(func(c *gin.Context) {
		writeProblem(c, http.StatusNotFound, "not_found", "Not found", "No route matches this request.")
	})
	return r
}

func liveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func readiness(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d.Config.ReadinessTimeout)
		defer cancel()

		if err := d.Checker.Check(ctx); err != nil {
			if d.Metrics != nil {
				d.Metrics.DBUp.Set(0)
			}
			d.Logger.LogAttrs(ctx, slog.LevelWarn, "readiness_failed",
				slog.String("request_id", c.GetString(requestIDKey)),
				slog.String("error", err.Error()),
			)
			writeProblem(c, http.StatusServiceUnavailable, "not_ready",
				"Service not ready", "A required dependency is unavailable.")
			return
		}
		if d.Metrics != nil {
			d.Metrics.DBUp.Set(1)
		}
		c.JSON(http.StatusOK, gin.H{
			"status":  "ready",
			"checks":  gin.H{"database": "ok", "schema": "ok"},
			"version": version.Version,
		})
	}
}

func meta(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service":     "relayforge-api",
			"version":     version.Version,
			"commit":      version.Commit,
			"environment": d.Config.Env,
			"time":        time.Now().UTC().Format(time.RFC3339),
		})
	}
}
