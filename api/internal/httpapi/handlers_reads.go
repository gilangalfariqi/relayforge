package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/relayforge/relayforge/api/internal/jsonschema"
	"github.com/relayforge/relayforge/api/internal/store"
)

func deliveriesList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c, "limit", "cursor", "status", "endpoint_id", "event_id", "from", "to") {
			return
		}
		limit, ok := parseLimit(c, 25, 100)
		if !ok {
			return
		}
		statuses, ok := parseStatuses(c, c.Query("status"), map[string]bool{
			"pending": true, "in_progress": true, "retry_scheduled": true,
			"delivered": true, "failed": true, "dead_lettered": true, "canceled": true,
		})
		if !ok {
			return
		}
		endpointID, ok := parseUUIDQuery(c, "endpoint_id")
		if !ok {
			return
		}
		eventID, ok := parseUUIDQuery(c, "event_id")
		if !ok {
			return
		}
		from, to, ok := parseRange(c)
		if !ok {
			return
		}
		filter := store.DeliveryFilter{Statuses: statuses, EndpointID: endpointID, EventID: eventID, From: from, To: to, Limit: limit}
		fh := filterHash("deliveries", strings.Join(statuses, ","), uuidPtr(endpointID), uuidPtr(eventID), fmtTime(from), fmtTime(to))
		if raw := c.Query("cursor"); raw != "" {
			t, id, err := decodeCursor(raw, fh)
			if err != nil {
				writeProblem(c, http.StatusBadRequest, "invalid_cursor", "Invalid cursor", "The cursor is invalid or does not match this filter.")
				return
			}
			filter.Cursor = &store.EventCursor{CreatedAt: t, ID: id}
		}
		items, more, err := d.Store.ListDeliveries(c.Request.Context(), principalID(c), filter)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if items == nil {
			items = []store.Delivery{}
		}
		resp := gin.H{"data": items, "has_more": more}
		if more && len(items) > 0 {
			last := items[len(items)-1]
			resp["next_cursor"] = encodeCursor(last.CreatedAt, last.ID, fh)
		} else {
			resp["next_cursor"] = nil
		}
		c.JSON(http.StatusOK, resp)
	}
}

func deliveriesGet(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		item, err := d.Store.Delivery(c.Request.Context(), principalID(c), id)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, item)
	}
}

func attemptsList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c, "limit", "cursor") {
			return
		}
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		limit, ok := parseLimit(c, 50, 100)
		if !ok {
			return
		}
		fh := filterHash("attempts", id.String())
		var cursor *store.AttemptCursor
		if raw := c.Query("cursor"); raw != "" {
			t, cursorID, err := decodeAttemptCursor(raw, fh)
			if err != nil {
				writeProblem(c, http.StatusBadRequest, "invalid_cursor", "Invalid cursor", "The cursor is invalid or does not match this filter.")
				return
			}
			cur := store.AttemptCursorFor(t, cursorID)
			cursor = &cur
		}
		items, more, err := d.Store.ListAttempts(c.Request.Context(), principalID(c), id, limit, cursor)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if items == nil {
			items = []store.Attempt{}
		}
		resp := gin.H{"data": items, "has_more": more}
		if more && len(items) > 0 {
			last := items[len(items)-1]
			resp["next_cursor"] = encodeAttemptCursor(last.StartedAt, last.CursorID, fh)
		} else {
			resp["next_cursor"] = nil
		}
		c.JSON(http.StatusOK, resp)
	}
}

func auditList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c, "limit", "cursor", "action", "resource_type", "resource_id", "from", "to") {
			return
		}
		limit, ok := parseLimit(c, 25, 100)
		if !ok {
			return
		}
		action := c.Query("action")
		if action != "" && !auditActionPattern.MatchString(action) {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "action filter is invalid.")
			return
		}
		resourceType := c.Query("resource_type")
		if resourceType != "" && !auditResourcePattern.MatchString(resourceType) {
			writeProblem(c, http.StatusBadRequest, "invalid_request", "Invalid request", "resource_type filter is invalid.")
			return
		}
		resourceID, ok := parseUUIDQuery(c, "resource_id")
		if !ok {
			return
		}
		from, to, ok := parseRange(c)
		if !ok {
			return
		}
		filter := store.AuditFilter{
			Action: action, ResourceType: resourceType, ResourceID: resourceID,
			From: from, To: to, Limit: limit,
		}
		fh := filterHash("audit", action, resourceType, uuidPtr(resourceID), fmtTime(from), fmtTime(to))
		if raw := c.Query("cursor"); raw != "" {
			t, cursorID, err := decodeAttemptCursor(raw, fh)
			if err != nil {
				writeProblem(c, http.StatusBadRequest, "invalid_cursor", "Invalid cursor", "The cursor is invalid or does not match this filter.")
				return
			}
			filter.Cursor = &store.AuditCursor{CreatedAt: t, ID: cursorID}
		}
		items, more, err := d.Store.ListAudit(c.Request.Context(), principalID(c), filter)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if items == nil {
			items = []store.AuditEntry{}
		}
		resp := gin.H{"data": items, "has_more": more}
		if more && len(items) > 0 {
			last := items[len(items)-1]
			resp["next_cursor"] = encodeAttemptCursor(last.CreatedAt, last.ID, fh)
		} else {
			resp["next_cursor"] = nil
		}
		c.JSON(http.StatusOK, resp)
	}
}

func schemasList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Store.ListEventSchemas(c.Request.Context(), principalID(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if items == nil {
			items = []store.EventSchema{}
		}
		c.JSON(http.StatusOK, gin.H{"data": items})
	}
}

func schemasCreate(d Deps) gin.HandlerFunc {
	type request struct {
		EventType string          `json:"event_type"`
		Version   int             `json:"version"`
		Schema    json.RawMessage `json:"schema"`
	}
	return func(c *gin.Context) {
		var req request
		if handleDecodeError(c, decodeJSON(c, &req)) {
			return
		}
		var fields []Field
		if !eventTypePattern.MatchString(req.EventType) {
			fields = append(fields, Field{Pointer: "/event_type", Code: "format", Detail: "invalid event type"})
		}
		if req.Version < 1 {
			fields = append(fields, Field{Pointer: "/version", Code: "range", Detail: "version must be >= 1"})
		}
		if len(req.Schema) == 0 {
			fields = append(fields, Field{Pointer: "/schema", Code: "required", Detail: "schema is required"})
		} else if err := jsonschema.Compile(req.Schema); err != nil {
			fields = append(fields, Field{Pointer: "/schema", Code: "invalid", Detail: err.Error()})
		}
		if len(fields) > 0 {
			fieldErrors(c, fields)
			return
		}
		out, err := d.Store.CreateEventSchema(c.Request.Context(), principalID(c), req.EventType, req.Version, req.Schema, actorFor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

func schemasDelete(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		if err := d.Store.DeleteEventSchema(c.Request.Context(), principalID(c), id, actorFor(c)); err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func adminSuspendTenant(d Deps) gin.HandlerFunc {
	type request struct {
		Reason string `json:"reason"`
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
		req.Reason = strings.TrimSpace(req.Reason)
		if len(req.Reason) < 1 || len([]rune(req.Reason)) > 200 {
			fieldErrors(c, []Field{{Pointer: "/reason", Code: "length", Detail: "reason must be between 1 and 200 characters"}})
			return
		}
		t, err := d.Store.SetTenantStatus(c.Request.Context(), id, "suspended", req.Reason, adminActor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

func adminResumeTenant(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		t, err := d.Store.SetTenantStatus(c.Request.Context(), id, "active", "resumed by operator", adminActor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

func adminListTenants(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c, "limit") {
			return
		}
		limit, ok := parseLimit(c, 50, 100)
		if !ok {
			return
		}
		items, err := d.Store.AdminListTenants(c.Request.Context(), limit)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if items == nil {
			items = []store.AdminTenantSummary{}
		}
		c.JSON(http.StatusOK, gin.H{"data": items})
	}
}

func adminQueueSummary(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c) {
			return
		}
		stats, err := d.Store.AdminQueueSummary(c.Request.Context())
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusOK, stats)
	}
}

func adminStuckDeliveries(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectUnknownQuery(c, "limit") {
			return
		}
		limit, ok := parseLimit(c, 50, 100)
		if !ok {
			return
		}
		items, err := d.Store.AdminStuckDeliveries(c.Request.Context(), limit)
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		if items == nil {
			items = []store.AdminStuckDelivery{}
		}
		c.JSON(http.StatusOK, gin.H{"data": items})
	}
}

func adminReplayDelivery(d Deps) gin.HandlerFunc {
	type request struct {
		Reason string `json:"reason"`
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
		req.Reason = strings.TrimSpace(req.Reason)
		if len([]rune(req.Reason)) < 1 || len([]rune(req.Reason)) > 500 {
			fieldErrors(c, []Field{{Pointer: "/reason", Code: "length", Detail: "reason must be between 1 and 500 characters"}})
			return
		}
		res, err := d.Store.AdminReplayDelivery(c.Request.Context(), id, req.Reason, adminActor(c))
		if err != nil {
			writeStoreError(c, d.Logger, err)
			return
		}
		c.JSON(http.StatusAccepted, res)
	}
}

func uuidPtr(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
