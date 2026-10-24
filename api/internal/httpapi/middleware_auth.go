package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/relayforge/relayforge/api/internal/auth"
	"github.com/relayforge/relayforge/api/internal/store"
)

const principalKey = "principal"

// Authenticate verifies an API key and attaches a Principal.
func Authenticate(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		raw := c.GetHeader("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") || strings.Contains(strings.TrimPrefix(raw, "Bearer "), " ") {
			unauthorized(c)
			return
		}
		key, err := auth.ParseKey(strings.TrimPrefix(raw, "Bearer "))
		if err != nil || key.Env != d.Config.KeyEnv {
			unauthorized(c)
			return
		}
		cred, err := d.Store.CredentialByKeyID(c.Request.Context(), key.KeyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				_ = auth.VerifyDigest(d.Config.APIKeyPepper, key.Secret, auth.DummyDigest(d.Config.APIKeyPepper))
				unauthorized(c)
				return
			}
			writeStoreError(c, d.Logger, err)
			return
		}
		if !auth.VerifyDigest(d.Config.APIKeyPepper, key.Secret, cred.SecretHMAC) {
			unauthorized(c)
			return
		}
		now := time.Now()
		if cred.ExpiresAt != nil && !cred.ExpiresAt.After(now) {
			unauthorized(c)
			return
		}
		if cred.RevokedAt != nil && !cred.RevokedAt.After(now) {
			unauthorized(c)
			return
		}
		if cred.TenantStatus == "suspended" {
			writeProblem(c, http.StatusForbidden, "tenant_suspended", "Tenant suspended",
				"This tenant is suspended. Contact the operator.")
			return
		}

		p := auth.Principal{
			TenantID:     cred.TenantID,
			CredentialID: cred.ID,
			KeyID:        cred.KeyID,
			Role:         cred.Role,
			TenantStatus: cred.TenantStatus,
		}
		c.Set(principalKey, p)
		if d.TokenUse.Allow(cred.ID) {
			d.Store.TouchCredential(c.Request.Context(), cred.ID)
		}
		c.Next()
	}
}

// RequireAction enforces the tenant role matrix.
func RequireAction(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := PrincipalFrom(c)
		if !ok {
			unauthorized(c)
			return
		}
		if !auth.Can(p.Role, action) {
			writeProblem(c, http.StatusForbidden, "forbidden", "Forbidden",
				"This credential does not have permission for this action.")
			return
		}
		c.Next()
	}
}

// RateLimitTenant enforces fixed-window PostgreSQL rate limits per tenant and
// scope ("tenant_ingest" or "tenant_management").
func RateLimitTenant(d Deps, scopeType string, limit int) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit <= 0 || d.Store == nil || d.Store.Pool == nil {
			c.Next()
			return
		}
		allowed, err := d.Store.AllowRateLimit(c.Request.Context(), scopeType, principalID(c), limit)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if !allowed {
			if d.Metrics != nil {
				d.Metrics.RateLimitDenied.WithLabelValues(scopeType).Inc()
			}
			c.Header("Retry-After", "1")
			writeProblem(c, http.StatusTooManyRequests, "rate_limited", "Rate limited",
				"Too many requests in the current 1-second window.")
			return
		}
		c.Next()
	}
}

// RateLimitAdmin enforces fixed-window rate limits on internal admin routes.
func RateLimitAdmin(d Deps, limit int) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit <= 0 || d.Store == nil || d.Store.Pool == nil {
			c.Next()
			return
		}
		allowed, err := d.Store.AllowRateLimit(c.Request.Context(), "tenant_management", uuid.Nil, limit)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if !allowed {
			if d.Metrics != nil {
				d.Metrics.RateLimitDenied.WithLabelValues("admin").Inc()
			}
			c.Header("Retry-After", "1")
			writeProblem(c, http.StatusTooManyRequests, "rate_limited", "Rate limited",
				"Too many admin requests in the current 1-second window.")
			return
		}
		c.Next()
	}
}

// PrincipalFrom returns the authenticated caller.
func PrincipalFrom(c *gin.Context) (auth.Principal, bool) {
	v, ok := c.Get(principalKey)
	if !ok {
		return auth.Principal{}, false
	}
	p, ok := v.(auth.Principal)
	return p, ok
}

func unauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="relayforge"`)
	writeProblem(c, http.StatusUnauthorized, "unauthorized", "Unauthorized", "A valid API key is required.")
}

type touchCache struct {
	mu    sync.Mutex
	last  map[uuid.UUID]time.Time
	every time.Duration
}

func newTouchCache(every time.Duration) *touchCache {
	return &touchCache{last: make(map[uuid.UUID]time.Time), every: every}
}

func (t *touchCache) Allow(id uuid.UUID) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[id]; ok && now.Sub(last) < t.every {
		return false
	}
	t.last[id] = now
	if len(t.last) > 10000 {
		for k, v := range t.last {
			if now.Sub(v) > 10*t.every {
				delete(t.last, k)
			}
		}
	}
	return true
}

type signupWindow struct {
	start time.Time
	count int
}

type signupLimiter struct {
	mu      sync.Mutex
	windows map[string]signupWindow
	limit   int
	window  time.Duration
}

func newSignupLimiter(limit int, window time.Duration) *signupLimiter {
	return &signupLimiter{windows: make(map[string]signupWindow), limit: limit, window: window}
}

func (s *signupLimiter) Allow(ip string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.windows[ip]
	if !ok || now.Sub(w.start) >= s.window {
		s.windows[ip] = signupWindow{start: now, count: 1}
		return true
	}
	if w.count >= s.limit {
		return false
	}
	w.count++
	s.windows[ip] = w
	return true
}
