package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/relayforge/relayforge/api/internal/store"
)

func writeStoreError(c *gin.Context, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(c, http.StatusNotFound, "not_found", "Not found", "The requested resource does not exist.")
	case errors.Is(err, store.ErrSlugTaken):
		writeProblem(c, http.StatusConflict, "slug_taken", "Slug already taken", "Choose a different tenant slug.")
	case errors.Is(err, store.ErrDuplicateEndpoint):
		writeProblem(c, http.StatusConflict, "endpoint_exists", "Endpoint already exists", "An active endpoint with this URL already exists.")
	case errors.Is(err, store.ErrEndpointLimit):
		writeProblem(c, http.StatusUnprocessableEntity, "endpoint_limit_reached", "Endpoint limit reached", "The tenant has reached its endpoint limit.")
	case errors.Is(err, store.ErrLastOwnerCredential):
		writeProblem(c, http.StatusConflict, "last_owner_credential", "Last owner credential", "A tenant must retain at least one active owner credential.")
	case errors.Is(err, store.ErrLastActiveSecret):
		writeProblem(c, http.StatusConflict, "last_active_secret", "Last active secret", "Create a replacement secret before retiring the last active one.")
	case errors.Is(err, store.ErrSecretOverlapLimit):
		writeProblem(c, http.StatusConflict, "secret_overlap_limit", "Secret overlap limit", "An endpoint may have at most two active signing secrets.")
	case errors.Is(err, store.ErrRotationRevoked):
		writeProblem(c, http.StatusConflict, "credential_revoked", "Credential revoked", "A revoked credential cannot be rotated.")
	case errors.Is(err, store.ErrIdempotencyReused):
		writeProblem(c, http.StatusConflict, "idempotency_key_reused", "Idempotency key reused",
			"The key was first used with a different request body.")
	case errors.Is(err, store.ErrIdempotencyInFlight):
		c.Header("Retry-After", "1")
		writeProblem(c, http.StatusConflict, "idempotency_in_progress", "Idempotency request in progress",
			"A request with this key is still committing. Retry shortly.")
	case errors.Is(err, store.ErrSchemaExists):
		writeProblem(c, http.StatusConflict, "schema_exists", "Schema already exists",
			"A schema for this event type and version already exists.")
	case errors.Is(err, store.ErrReplayNotAllowed):
		var replayErr *store.ReplayNotAllowedError
		var errs []Field
		reason := "no_deliveries"
		if errors.As(err, &replayErr) {
			for _, r := range replayErr.Results {
				if r.Reason != "" {
					reason = r.Reason
					errs = append(errs, Field{
						Pointer: "/deliveries/" + r.DeliveryID.String(),
						Code:    r.Reason,
						Detail:  "delivery status is " + r.Reason,
					})
				}
			}
		}
		c.Header("Content-Type", "application/problem+json; charset=utf-8")
		c.AbortWithStatusJSON(http.StatusConflict, Problem{
			Type:      "https://relayforge.example/errors/replay-not-allowed",
			Title:     "Replay not allowed",
			Status:    http.StatusConflict,
			Detail:    "No deliveries are in a replayable state.",
			Code:      "replay_not_allowed",
			Reason:    reason,
			Instance:  c.Request.URL.Path,
			RequestID: c.GetString(requestIDKey),
			Errors:    errs,
		})
	default:
		logger.LogAttrs(c.Request.Context(), slog.LevelError, "request_failed",
			slog.String("request_id", c.GetString(requestIDKey)),
			slog.String("route", routeLabel(c)),
			slog.String("error", err.Error()),
		)
		writeProblem(c, http.StatusInternalServerError, "internal_error", "Internal server error",
			"The request could not be completed.")
	}
}

func decodeJSON(c *gin.Context, dst any) error {
	ct := c.GetHeader("Content-Type")
	if ct == "" {
		return errUnsupportedMedia
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return errUnsupportedMedia
	}
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

var errUnsupportedMedia = errors.New("unsupported media type")

func handleDecodeError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeProblem(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Payload too large", "Request body exceeds the limit.")
		return true
	}
	if errors.Is(err, errUnsupportedMedia) {
		writeProblem(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported media type",
			"Content-Type must be application/json.")
		return true
	}
	writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "The JSON request body is invalid.")
	return true
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "The resource identifier is invalid.")
		return uuid.Nil, false
	}
	return id, true
}

func fieldErrors(c *gin.Context, fields []Field) {
	c.Header("Content-Type", "application/problem+json; charset=utf-8")
	c.AbortWithStatusJSON(http.StatusUnprocessableEntity, Problem{
		Type:      "https://relayforge.example/errors/validation-failed",
		Title:     "Validation failed",
		Status:    http.StatusUnprocessableEntity,
		Detail:    "One or more fields are invalid.",
		Code:      "validation_failed",
		RequestID: c.GetString(requestIDKey),
		Errors:    fields,
	})
}

func actorFor(c *gin.Context) store.Actor {
	p, _ := PrincipalFrom(c)
	return store.Actor{
		Type:      "api_key",
		ID:        p.CredentialID.String(),
		RequestID: c.GetString(requestIDKey),
		IP:        c.ClientIP(),
		UserAgent: c.GetHeader("User-Agent"),
	}
}

func adminActor(c *gin.Context) store.Actor {
	return store.Actor{
		Type:      "admin_token",
		ID:        "admin",
		RequestID: c.GetString(requestIDKey),
		IP:        c.ClientIP(),
		UserAgent: c.GetHeader("User-Agent"),
	}
}

type cursorToken struct {
	Time   string `json:"t"`
	ID     string `json:"id"`
	Filter string `json:"f"`
}

type attemptCursorToken struct {
	Time   string `json:"t"`
	ID     int64  `json:"id"`
	Filter string `json:"f"`
}

func encodeCursor(t time.Time, id uuid.UUID, filter string) string {
	b, _ := json.Marshal(cursorToken{Time: t.UTC().Format(time.RFC3339Nano), ID: id.String(), Filter: filter})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw, filter string) (time.Time, uuid.UUID, error) {
	if len(raw) > 512 {
		return time.Time{}, uuid.Nil, errors.New("cursor too long")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	var tok cursorToken
	if err := json.Unmarshal(b, &tok); err != nil {
		return time.Time{}, uuid.Nil, err
	}
	if tok.Filter != filter {
		return time.Time{}, uuid.Nil, errors.New("cursor filter mismatch")
	}
	t, err := time.Parse(time.RFC3339Nano, tok.Time)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(tok.ID)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}

func encodeAttemptCursor(t time.Time, id int64, filter string) string {
	b, _ := json.Marshal(attemptCursorToken{Time: t.UTC().Format(time.RFC3339Nano), ID: id, Filter: filter})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAttemptCursor(raw, filter string) (time.Time, int64, error) {
	if len(raw) > 512 {
		return time.Time{}, 0, errors.New("cursor too long")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, 0, err
	}
	var tok attemptCursorToken
	if err := json.Unmarshal(b, &tok); err != nil {
		return time.Time{}, 0, err
	}
	if tok.Filter != filter {
		return time.Time{}, 0, errors.New("cursor filter mismatch")
	}
	t, err := time.Parse(time.RFC3339Nano, tok.Time)
	if err != nil {
		return time.Time{}, 0, err
	}
	if tok.ID <= 0 {
		return time.Time{}, 0, errors.New("invalid cursor id")
	}
	return t, tok.ID, nil
}

func filterHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return base64.RawURLEncoding.EncodeToString(h[:9])
}

func rejectUnknownQuery(c *gin.Context, allowed ...string) bool {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	for key := range c.Request.URL.Query() {
		if !set[key] {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request",
				"Unknown query parameter: "+truncateForProblem(key)+".")
			return true
		}
	}
	return false
}

func truncateForProblem(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

func parseLimit(c *gin.Context, fallback, max int) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return fallback, true
	}
	n, err := parsePositiveInt(raw)
	if err != nil || n > max {
		writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request",
			fmt.Sprintf("limit must be an integer between 1 and %d.", max))
		return 0, false
	}
	return n, true
}

func parsePositiveInt(raw string) (int, error) {
	n := 0
	if raw == "" {
		return 0, errors.New("empty")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errors.New("not an integer")
		}
		n = n*10 + int(r-'0')
		if n > 1000000 {
			return 0, errors.New("too large")
		}
	}
	if n < 1 {
		return 0, errors.New("too small")
	}
	return n, nil
}

func principalID(c *gin.Context) uuid.UUID {
	p, _ := PrincipalFrom(c)
	return p.TenantID
}
