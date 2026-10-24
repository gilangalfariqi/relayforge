package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/relayforge/relayforge/api/internal/store"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,255}$`)

type envelope struct {
	Type          string          `json:"type"`
	SchemaVersion *int            `json:"schema_version"`
	OccurredAt    *string         `json:"occurred_at"`
	Data          json.RawMessage `json:"data"`
}

func eventsCreate(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("Idempotency-Key")
		if key == "" {
			writeProblem(c, http.StatusBadRequest, "idempotency_key_required", "Idempotency key required",
				"Provide an Idempotency-Key header.")
			return
		}
		if !idempotencyKeyPattern.MatchString(key) {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request",
				"Idempotency-Key must be 8-255 characters matching ^[A-Za-z0-9_.:-]+$.")
			return
		}
		if ct := c.GetHeader("Content-Type"); ct == "" {
			writeProblem(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported media type",
				"Content-Type must be application/json.")
			return
		} else if mediaType, _, err := mime.ParseMediaType(ct); err != nil || mediaType != "application/json" {
			writeProblem(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported media type",
				"Content-Type must be application/json.")
			return
		}
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeProblem(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Payload too large",
					"Event body exceeds 256 KiB.")
				return
			}
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "Could not read request body.")
			return
		}
		if len(raw) == 0 {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "Request body is required.")
			return
		}
		if int64(len(raw)) > d.Config.MaxEventBytes {
			writeProblem(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Payload too large",
				"Event body exceeds the configured limit.")
			return
		}

		var env envelope
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&env); err != nil {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "The event envelope is invalid JSON.")
			return
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "Request body must contain exactly one JSON object.")
			return
		}

		fields, parsedOccurred := validateEnvelope(env, d.Config.FutureSkew, d.Config.OccurredPast)
		if len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		if len(env.Data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(env.Data), []byte("{")) {
			fieldErrors(c, []Field{{Pointer: "/data", Code: "type", Detail: "data must be a JSON object"}})
			return
		}
		if depth(env.Data) > 32 {
			fieldErrors(c, []Field{{Pointer: "/data", Code: "depth", Detail: "data nesting exceeds 32 levels"}})
			return
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, env.Data); err != nil {
			fieldErrors(c, []Field{{Pointer: "/data", Code: "json", Detail: "data must be valid JSON"}})
			return
		}
		if compact.Len() == 0 || compact.Len() > int(d.Config.MaxEventBytes) {
			fieldErrors(c, []Field{{Pointer: "/data", Code: "size", Detail: "data exceeds the payload limit"}})
			return
		}

		now := time.Now().UTC()
		occurredAt := now
		if parsedOccurred != nil {
			occurredAt = *parsedOccurred
		}
		schemaVersion := 1
		if env.SchemaVersion != nil {
			schemaVersion = *env.SchemaVersion
		}

		if schemaRaw, err := d.Store.EventSchemaJSON(c.Request.Context(), principalID(c), env.Type, schemaVersion); err == nil {
			var data any
			if err := json.Unmarshal(env.Data, &data); err != nil {
				fieldErrors(c, []Field{{Pointer: "/data", Code: "json", Detail: "data must be valid JSON"}})
				return
			}
			schemaErrs, err := d.Schemas.Validate(schemaRaw, data)
			if err != nil {
				writeStoreError(c, d.Logger, err)
				return
			}
			if len(schemaErrs) > 0 {
				out := make([]Field, 0, len(schemaErrs))
				for _, e := range schemaErrs {
					out = append(out, Field{Pointer: e.Pointer, Code: e.Code, Detail: e.Detail})
				}
				c.Header("Content-Type", "application/problem+json; charset=utf-8")
				c.AbortWithStatusJSON(http.StatusUnprocessableEntity, Problem{
					Type: "https://relayforge.example/errors/schema-validation-failed", Title: "Schema validation failed",
					Status: http.StatusUnprocessableEntity, Code: "schema_validation_failed",
					Detail: "The payload does not match the registered schema.", RequestID: c.GetString(requestIDKey), Errors: out,
				})
				return
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			writeStoreError(c, d.Logger, err)
			return
		}

		in := store.IngestInput{
			TenantID:      principalID(c),
			Type:          env.Type,
			SchemaVersion: schemaVersion,
			OccurredAt:    occurredAt,
			WireBody:      raw,
			Payload:       compact.Bytes(),
			PayloadBytes:  compact.Len(),
			PayloadSHA256: sha256Bytes(raw),
			TraceParent:   c.GetHeader("Traceparent"),
			Scope:         "events.create",
			Key:           key,
			Fingerprint:   sha256Bytes(raw),
		}
		result, err := d.Store.IngestEvent(c.Request.Context(), in)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if result.Replayed {
			c.Header("Idempotency-Replayed", "true")
		}
		var body struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(result.Body, &body)
		if body.ID != "" {
			c.Header("Location", "/api/v1/events/"+body.ID)
		}
		c.Data(result.StatusCode, "application/json; charset=utf-8", result.Body)
	}
}

// validateEnvelope checks envelope fields and returns the parsed occurred_at.
func validateEnvelope(env envelope, future, past time.Duration) ([]Field, *time.Time) {
	var fields []Field
	var occurred *time.Time
	if len(env.Type) < 3 || len(env.Type) > 100 || !eventTypePattern.MatchString(env.Type) {
		fields = append(fields, Field{Pointer: "/type", Code: "format", Detail: "type must match ^[a-z][a-z0-9_]*(\\.[a-z0-9_]+){1,3}$"})
	}
	if env.SchemaVersion != nil && *env.SchemaVersion < 1 {
		fields = append(fields, Field{Pointer: "/schema_version", Code: "range", Detail: "schema_version must be >= 1"})
	}
	if env.OccurredAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *env.OccurredAt)
		if err != nil {
			fields = append(fields, Field{Pointer: "/occurred_at", Code: "format", Detail: "occurred_at must be RFC 3339"})
		} else {
			now := time.Now().UTC()
			if t.After(now.Add(future)) {
				fields = append(fields, Field{Pointer: "/occurred_at", Code: "future", Detail: "occurred_at is too far in the future"})
			}
			if t.Before(now.Add(-past)) {
				fields = append(fields, Field{Pointer: "/occurred_at", Code: "past", Detail: "occurred_at is too far in the past"})
			}
			occurred = &t
		}
	}
	if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		fields = append(fields, Field{Pointer: "/data", Code: "required", Detail: "data is required and must be an object"})
	}
	return fields, occurred
}

func sha256Bytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func depth(raw []byte) int {
	max, cur := 0, 0
	inString, escaped := false, false
	for _, c := range raw {
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			cur++
			if cur > max {
				max = cur
			}
		case '}', ']':
			cur--
		}
	}
	return max
}

func eventsList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c, "limit", "cursor", "status", "type", "from", "to", "order") {
			return
		}
		limit, ok := parseLimit(c, 25, 100)
		if !ok {
			return
		}
		order := c.DefaultQuery("order", "desc")
		if order != "asc" && order != "desc" {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "order must be asc or desc.")
			return
		}
		statuses, ok := parseStatuses(c, c.Query("status"), map[string]bool{
			"accepted": true, "delivering": true, "delivered": true,
			"partially_delivered": true, "failed": true, "no_subscribers": true,
		})
		if !ok {
			return
		}
		eventType := c.Query("type")
		if eventType != "" && !eventTypePattern.MatchString(eventType) {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "type is invalid.")
			return
		}
		from, to, ok := parseRange(c)
		if !ok {
			return
		}
		filter := store.EventFilter{Statuses: statuses, Type: eventType, From: from, To: to, Order: order, Limit: limit}
		fh := filterHash("events", strings.Join(statuses, ","), eventType, fmtTime(from), fmtTime(to), order)
		if raw := c.Query("cursor"); raw != "" {
			t, id, err := decodeCursor(raw, fh)
			if err != nil {
				writeProblem(c, http.StatusBadRequest, "invalid_cursor", "Invalid cursor", "The cursor is invalid or does not match this filter.")
				return
			}
			filter.Cursor = &store.EventCursor{CreatedAt: t, ID: id}
		}
		events, more, err := d.Store.ListEvents(c.Request.Context(), principalID(c), filter)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if events == nil {
			events = []store.Event{}
		}
		resp := gin.H{"data": events, "has_more": more}
		if more && len(events) > 0 {
			last := events[len(events)-1]
			resp["next_cursor"] = encodeCursor(last.CreatedAt, last.ID, fh)
		} else {
			resp["next_cursor"] = nil
		}
		c.JSON(http.StatusOK, resp)
	}
}

func eventsGet(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		detail, err := d.Store.EventDetail(c.Request.Context(), principalID(c), id)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, detail)
	}
}

func eventsReplay(d Deps) gin.HandlerFunc {
	type request struct {
		EndpointIDs []string `json:"endpoint_ids"`
		Reason      string   `json:"reason"`
	}
	return func(c *gin.Context) {
		eventID, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		var req request
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		var fields []Field
		req.Reason = strings.TrimSpace(req.Reason)
		if len([]rune(req.Reason)) < 1 || len([]rune(req.Reason)) > 500 {
			fields = append(fields, Field{Pointer: "/reason", Code: "length", Detail: "reason must be between 1 and 500 characters"})
		}
		if len(req.EndpointIDs) > 50 {
			fields = append(fields, Field{Pointer: "/endpoint_ids", Code: "max_items", Detail: "endpoint_ids may contain at most 50 entries"})
		}
		seen := make(map[uuid.UUID]bool, len(req.EndpointIDs))
		var endpointUUIDs []uuid.UUID
		for _, rawID := range req.EndpointIDs {
			epID, err := uuid.Parse(rawID)
			if err != nil {
				fields = append(fields, Field{Pointer: "/endpoint_ids", Code: "format", Detail: "every endpoint_id must be a valid UUID"})
				break
			}
			if !seen[epID] {
				seen[epID] = true
				endpointUUIDs = append(endpointUUIDs, epID)
			}
		}
		if len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		out, err := d.Store.ReplayEvent(c.Request.Context(), principalID(c), eventID, endpointUUIDs, req.Reason, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusAccepted, out)
	}
}

func parseStatuses(c *gin.Context, raw string, allowed map[string]bool) ([]string, bool) {
	if raw == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !allowed[p] {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "Unknown status in filter.")
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}

func parseRange(c *gin.Context) (*time.Time, *time.Time, bool) {
	var from, to *time.Time
	if raw := c.Query("from"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "from must be RFC 3339.")
			return nil, nil, false
		}
		from = &t
	}
	if raw := c.Query("to"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "to must be RFC 3339.")
			return nil, nil, false
		}
		to = &t
	}
	if from != nil && to != nil {
		if to.Before(*from) || to.Sub(*from) > 90*24*time.Hour {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "Time range must be ordered and at most 90 days.")
			return nil, nil, false
		}
	}
	return from, to, true
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseUUIDQuery(c *gin.Context, name string) (*uuid.UUID, bool) {
	raw := c.Query(name)
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", name+" must be a UUID.")
		return nil, false
	}
	return &id, true
}
