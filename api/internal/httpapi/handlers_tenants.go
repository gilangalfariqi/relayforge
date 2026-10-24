package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/relayforge/relayforge/api/internal/auth"
	"github.com/relayforge/relayforge/api/internal/store"
)

var (
	slugPattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)
	eventTypePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+){1,3}$`)
	rolePattern          = regexp.MustCompile(`^(owner|admin|ingest|viewer)$`)
	auditActionPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){1,3}$`)
	auditResourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,49}$`)
)

func signupHandler(d Deps) gin.HandlerFunc {
	type request struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		switch d.Config.SignupMode {
		case "disabled":
			writeProblem(c, http.StatusForbidden, "signup_disabled", "Signup disabled", "Tenant registration is disabled.")
			return
		case "token":
			provided := c.GetHeader("X-Signup-Token")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(d.Config.SignupToken)) != 1 {
				writeProblem(c, http.StatusForbidden, "signup_disabled", "Signup denied", "A valid signup token is required.")
				return
			}
		}
		if !d.Signup.Allow(c.ClientIP()) {
			if d.Metrics != nil {
				d.Metrics.RateLimitDenied.WithLabelValues("signup").Inc()
			}
			c.Header("Retry-After", "60")
			writeProblem(c, http.StatusTooManyRequests, "rate_limited", "Rate limited", "Too many registrations from this address.")
			return
		}
		var req request
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if len([]rune(req.Name)) < 1 || len([]rune(req.Name)) > 200 {
			fieldErrors(c, []Field{{Pointer: "/name", Code: "length", Detail: "name must be between 1 and 200 characters"}})
			return
		}
		explicit := req.Slug != ""
		if explicit && !slugPattern.MatchString(req.Slug) {
			fieldErrors(c, []Field{{Pointer: "/slug", Code: "format", Detail: "slug must match ^[a-z0-9][a-z0-9-]{2,62}$"}})
			return
		}
		base := req.Slug
		if !explicit {
			base = deriveSlug(req.Name)
		}
		key, digest, err := auth.GenerateKey(d.Config.KeyEnv, d.Config.APIKeyPepper)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		actor := store.Actor{Type: "system", ID: "signup", RequestID: c.GetString(requestIDKey), IP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent")}
		for attempt := 0; attempt < 6; attempt++ {
			slug := base
			if attempt > 0 {
				slug = base + "-" + randomSuffix()
			}
			t, cred, err := d.Store.CreateTenant(c.Request.Context(), slug, req.Name, key, digest, actor)
			if err == nil {
				c.Header("Location", "/api/v1/tenant")
				c.JSON(http.StatusCreated, gin.H{
					"tenant":     t,
					"api_key":    cred.APIKey,
					"api_key_id": cred.ID,
					"warning":    "Store this key now. It cannot be retrieved again.",
				})
				return
			}
			if err == store.ErrSlugTaken && !explicit {
				continue
			}
			writeStoreError(c, d.Logger, err)
			return
		}
		writeProblem(c, http.StatusConflict, "slug_taken", "Slug already taken", "Choose a different tenant slug.")
	}
}

func tenantGet(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := d.Store.Tenant(c.Request.Context(), principalID(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

func tenantUpdate(d Deps) gin.HandlerFunc {
	type request struct {
		Name string `json:"name"`
	}
	return func(c *gin.Context) {
		var req request
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if len([]rune(req.Name)) < 1 || len([]rune(req.Name)) > 200 {
			fieldErrors(c, []Field{{Pointer: "/name", Code: "length", Detail: "name must be between 1 and 200 characters"}})
			return
		}
		t, err := d.Store.UpdateTenantName(c.Request.Context(), principalID(c), req.Name, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

func credentialsList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c) {
			return
		}
		creds, err := d.Store.ListCredentials(c.Request.Context(), principalID(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if creds == nil {
			creds = []store.Credential{}
		}
		c.JSON(http.StatusOK, gin.H{"data": creds})
	}
}

func credentialsCreate(d Deps) gin.HandlerFunc {
	type request struct {
		Name          string `json:"name"`
		Role          string `json:"role"`
		ExpiresInDays *int   `json:"expires_in_days"`
	}
	return func(c *gin.Context) {
		var req request
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		var fields []Field
		req.Name = strings.TrimSpace(req.Name)
		if len([]rune(req.Name)) < 1 || len([]rune(req.Name)) > 100 {
			fields = append(fields, Field{Pointer: "/name", Code: "length", Detail: "name must be between 1 and 100 characters"})
		}
		if !rolePattern.MatchString(req.Role) {
			fields = append(fields, Field{Pointer: "/role", Code: "enum", Detail: "role must be owner, admin, ingest, or viewer"})
		}
		var expiresAt *time.Time
		if req.ExpiresInDays != nil {
			if *req.ExpiresInDays < 1 || *req.ExpiresInDays > 365 {
				fields = append(fields, Field{Pointer: "/expires_in_days", Code: "range", Detail: "expires_in_days must be between 1 and 365"})
			} else {
				t := time.Now().UTC().Add(time.Duration(*req.ExpiresInDays) * 24 * time.Hour)
				expiresAt = &t
			}
		}
		if len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		key, digest, err := auth.GenerateKey(d.Config.KeyEnv, d.Config.APIKeyPepper)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		cred, err := d.Store.CreateCredential(c.Request.Context(), principalID(c), req.Name, req.Role, expiresAt, key, digest, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusCreated, cred)
	}
}

func credentialsRotate(d Deps) gin.HandlerFunc {
	type request struct {
		OverlapSeconds *int `json:"overlap_seconds"`
	}
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		var req request
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		overlap := 3600
		if req.OverlapSeconds != nil {
			overlap = *req.OverlapSeconds
		}
		if overlap < 0 || overlap > 86400 {
			fieldErrors(c, []Field{{Pointer: "/overlap_seconds", Code: "range", Detail: "overlap_seconds must be between 0 and 86400"}})
			return
		}
		key, digest, err := auth.GenerateKey(d.Config.KeyEnv, d.Config.APIKeyPepper)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		cred, oldRevokedAt, err := d.Store.RotateCredential(c.Request.Context(), principalID(c), id,
			time.Duration(overlap)*time.Second, key, digest, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"id":             cred.ID,
			"key_id":         cred.KeyID,
			"name":           cred.Name,
			"role":           cred.Role,
			"created_at":     cred.CreatedAt,
			"expires_at":     cred.ExpiresAt,
			"rotated_from":   cred.RotatedFrom,
			"api_key":        cred.APIKey,
			"old_revoked_at": oldRevokedAt,
		})
	}
}

func credentialsRevoke(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		if err := d.Store.RevokeCredential(c.Request.Context(), principalID(c), id, actorFor(c)); err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func deriveSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) < 3 {
		s = "tenant"
	}
	if len(s) > 50 {
		s = strings.TrimRight(s[:50], "-")
	}
	return s
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uuid.NewString()[:4]
	}
	return hex.EncodeToString(b[:])
}
