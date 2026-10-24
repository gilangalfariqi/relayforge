package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/relayforge/relayforge/api/internal/ssrf"
	"github.com/relayforge/relayforge/api/internal/store"
)

type endpointRequest struct {
	Name               *string   `json:"name"`
	URL                *string   `json:"url"`
	Status             *string   `json:"status"`
	EventTypes         *[]string `json:"event_types"`
	MaxAttempts        *int      `json:"max_attempts"`
	InitialBackoffMS   *int      `json:"initial_backoff_ms"`
	MaxBackoffMS       *int      `json:"max_backoff_ms"`
	RetryWindowSeconds *int      `json:"retry_window_seconds"`
	TimeoutMS          *int      `json:"timeout_ms"`
	RateLimitPerSec    *int      `json:"rate_limit_per_sec"`
}

func endpointsList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c) {
			return
		}
		eps, err := d.Store.ListEndpoints(c.Request.Context(), principalID(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if eps == nil {
			eps = []store.Endpoint{}
		}
		c.JSON(http.StatusOK, gin.H{"data": eps})
	}
}

func endpointsGet(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		ep, err := d.Store.Endpoint(c.Request.Context(), principalID(c), id)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, ep)
	}
}

func endpointsCreate(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req endpointRequest
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		if req.Name == nil || req.URL == nil {
			fieldErrors(c, []Field{
				{Pointer: "/name", Code: "required", Detail: "name is required"},
				{Pointer: "/url", Code: "required", Detail: "url is required"},
			})
			return
		}
		ep := store.Endpoint{
			Name:               strings.TrimSpace(*req.Name),
			URL:                strings.TrimSpace(*req.URL),
			Status:             "active",
			EventTypes:         []string{},
			MaxAttempts:        8,
			InitialBackoffMS:   5000,
			MaxBackoffMS:       3600000,
			RetryWindowSeconds: 86400,
			TimeoutMS:          15000,
			RateLimitPerSec:    10,
		}
		if req.Status != nil {
			fieldErrors(c, []Field{{Pointer: "/status", Code: "read_only", Detail: "status is not accepted on create"}})
			return
		}
		if fields := applyEndpointFields(&ep, req); len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		if fields := validateEndpointCommon(ep); len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		if _, err := d.SSRF.Validate(c.Request.Context(), ep.URL); err != nil {
			destinationRejected(c, err)
			return
		}
		out, err := d.Store.CreateEndpoint(c.Request.Context(), principalID(c), ep, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

func endpointsUpdate(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		var req endpointRequest
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		ep, err := d.Store.Endpoint(c.Request.Context(), principalID(c), id)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if fields := applyEndpointFields(&ep, req); len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		if fields := validateEndpointCommon(ep); len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		if req.Status != nil {
			if *req.Status != "active" && *req.Status != "paused" {
				fieldErrors(c, []Field{{Pointer: "/status", Code: "enum", Detail: "status must be active or paused"}})
				return
			}
			ep.Status = *req.Status
		}
		if req.URL != nil {
			if _, err := d.SSRF.Validate(c.Request.Context(), ep.URL); err != nil {
				destinationRejected(c, err)
				return
			}
		}
		out, err := d.Store.UpdateEndpoint(c.Request.Context(), principalID(c), id, ep, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func applyEndpointFields(ep *store.Endpoint, req endpointRequest) []Field {
	var fields []Field
	if req.Name != nil {
		ep.Name = strings.TrimSpace(*req.Name)
	}
	if req.URL != nil {
		ep.URL = strings.TrimSpace(*req.URL)
	}
	if req.EventTypes != nil {
		ep.EventTypes = *req.EventTypes
	}
	if req.MaxAttempts != nil {
		ep.MaxAttempts = *req.MaxAttempts
	}
	if req.InitialBackoffMS != nil {
		ep.InitialBackoffMS = *req.InitialBackoffMS
	}
	if req.MaxBackoffMS != nil {
		ep.MaxBackoffMS = *req.MaxBackoffMS
	}
	if req.RetryWindowSeconds != nil {
		ep.RetryWindowSeconds = *req.RetryWindowSeconds
	}
	if req.TimeoutMS != nil {
		ep.TimeoutMS = *req.TimeoutMS
	}
	if req.RateLimitPerSec != nil {
		ep.RateLimitPerSec = *req.RateLimitPerSec
	}
	return fields
}

func validateEndpointCommon(ep store.Endpoint) []Field {
	var fields []Field
	if n := len([]rune(ep.Name)); n < 1 || n > 100 {
		fields = append(fields, Field{Pointer: "/name", Code: "length", Detail: "name must be between 1 and 100 characters"})
	}
	if len(ep.URL) == 0 || len(ep.URL) > 2048 {
		fields = append(fields, Field{Pointer: "/url", Code: "length", Detail: "url must be between 1 and 2048 characters"})
	}
	if len(ep.EventTypes) > 20 {
		fields = append(fields, Field{Pointer: "/event_types", Code: "max_items", Detail: "at most 20 event types are allowed"})
	}
	seen := map[string]bool{}
	for i, t := range ep.EventTypes {
		if !eventTypePattern.MatchString(t) {
			fields = append(fields, Field{Pointer: "/event_types/" + itoa(i), Code: "format", Detail: "invalid event type"})
		}
		if seen[t] {
			fields = append(fields, Field{Pointer: "/event_types/" + itoa(i), Code: "duplicate", Detail: "duplicate event type"})
		}
		seen[t] = true
	}
	if ep.MaxAttempts < 1 || ep.MaxAttempts > 20 {
		fields = append(fields, Field{Pointer: "/max_attempts", Code: "range", Detail: "max_attempts must be between 1 and 20"})
	}
	if ep.InitialBackoffMS < 100 || ep.InitialBackoffMS > 3600000 {
		fields = append(fields, Field{Pointer: "/initial_backoff_ms", Code: "range", Detail: "initial_backoff_ms must be between 100 and 3600000"})
	}
	if ep.MaxBackoffMS < ep.InitialBackoffMS || ep.MaxBackoffMS > 86400000 {
		fields = append(fields, Field{Pointer: "/max_backoff_ms", Code: "range", Detail: "max_backoff_ms must be >= initial_backoff_ms and <= 86400000"})
	}
	if ep.RetryWindowSeconds < 60 || ep.RetryWindowSeconds > 604800 {
		fields = append(fields, Field{Pointer: "/retry_window_seconds", Code: "range", Detail: "retry_window_seconds must be between 60 and 604800"})
	}
	if ep.TimeoutMS < 1000 || ep.TimeoutMS > 30000 {
		fields = append(fields, Field{Pointer: "/timeout_ms", Code: "range", Detail: "timeout_ms must be between 1000 and 30000"})
	}
	if ep.RateLimitPerSec < 1 || ep.RateLimitPerSec > 100 {
		fields = append(fields, Field{Pointer: "/rate_limit_per_sec", Code: "range", Detail: "rate_limit_per_sec must be between 1 and 100"})
	}
	return fields
}

func endpointsDelete(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		if err := d.Store.DeleteEndpoint(c.Request.Context(), principalID(c), id, actorFor(c)); err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func secretsList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		secretList, err := d.Store.ListSecrets(c.Request.Context(), principalID(c), id)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if secretList == nil {
			secretList = []store.SecretMeta{}
		}
		c.JSON(http.StatusOK, gin.H{"data": secretList})
	}
}

func secretsCreate(d Deps) gin.HandlerFunc {
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
		overlap := 86400
		if req.OverlapSeconds != nil {
			overlap = *req.OverlapSeconds
		}
		if overlap < 0 || overlap > 604800 {
			fieldErrors(c, []Field{{Pointer: "/overlap_seconds", Code: "range", Detail: "overlap_seconds must be between 0 and 604800"}})
			return
		}
		secret, err := d.Store.CreateEndpointSecret(c.Request.Context(), principalID(c), id,
			time.Duration(overlap)*time.Second, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusCreated, secret)
	}
}

func secretsRetire(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		endpointID, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		secretID, ok := parseUUIDParam(c, "secret_id")
		if !ok {
			return
		}
		if err := d.Store.RetireEndpointSecret(c.Request.Context(), principalID(c), endpointID, secretID, actorFor(c)); err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func destinationRejected(c *gin.Context, err error) {
	reason := "destination_rejected"
	detail := "Destination is not permitted."
	if r, ok := ssrf.AsReason(err); ok {
		reason = r.Code
		detail = r.Detail
	}
	c.Header("Content-Type", "application/problem+json; charset=utf-8")
	c.AbortWithStatusJSON(http.StatusUnprocessableEntity, Problem{
		Type:      "https://relayforge.example/errors/destination-rejected",
		Title:     "Destination rejected",
		Status:    http.StatusUnprocessableEntity,
		Detail:    detail,
		Code:      "destination_rejected",
		Reason:    reason,
		RequestID: c.GetString(requestIDKey),
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
