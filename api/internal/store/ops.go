package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AllowRateLimit atomically increments the 1-second fixed-window counter in
// rate_limit_counters and returns true when the request is within limitPerSec.
func (s *Store) AllowRateLimit(ctx context.Context, scopeType string, scopeID uuid.UUID, limitPerSec int) (bool, error) {
	if limitPerSec <= 0 {
		return true, nil
	}
	var count int
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO rate_limit_counters (scope_type, scope_id, window_start, count)
		VALUES ($1, $2, date_trunc('second', clock_timestamp()), 1)
		ON CONFLICT (scope_type, scope_id, window_start)
		DO UPDATE SET count = rate_limit_counters.count + 1
		WHERE rate_limit_counters.count < $3
		RETURNING count`,
		scopeType, scopeID, limitPerSec).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rate limit check: %w", err)
	}
	return true, nil
}

// ReplayEvent replays failed and dead-lettered deliveries for an event owned by
// tenantID within a single transaction, incrementing cycle, resetting attempt
// counters, preserving append-only attempt history, writing audit records, and
// recomputing the event status projection.
func (s *Store) ReplayEvent(ctx context.Context, tenantID, eventID uuid.UUID, endpointIDs []uuid.UUID, reason string, actor Actor) (ReplayEventResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ReplayEventResult{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return ReplayEventResult{}, err
	}

	var existingEventID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM events
		WHERE id = $1 AND tenant_id = $2`, eventID, tenantID).Scan(&existingEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplayEventResult{}, ErrNotFound
	}
	if err != nil {
		return ReplayEventResult{}, fmt.Errorf("lookup event for replay: %w", err)
	}

	var rows pgx.Rows
	if len(endpointIDs) > 0 {
		rows, err = tx.Query(ctx, `
			SELECT id, endpoint_id, cycle, status
			FROM deliveries
			WHERE tenant_id = $1 AND event_id = $2 AND endpoint_id = ANY($3)
			ORDER BY id ASC
			FOR UPDATE`, tenantID, eventID, endpointIDs)
	} else {
		rows, err = tx.Query(ctx, `
			SELECT id, endpoint_id, cycle, status
			FROM deliveries
			WHERE tenant_id = $1 AND event_id = $2
			ORDER BY id ASC
			FOR UPDATE`, tenantID, eventID)
	}
	if err != nil {
		return ReplayEventResult{}, fmt.Errorf("lock deliveries for replay: %w", err)
	}

	type lockedDelivery struct {
		id         uuid.UUID
		endpointID uuid.UUID
		cycle      int
		status     string
	}
	var locked []lockedDelivery
	for rows.Next() {
		var item lockedDelivery
		if err := rows.Scan(&item.id, &item.endpointID, &item.cycle, &item.status); err != nil {
			rows.Close()
			return ReplayEventResult{}, err
		}
		locked = append(locked, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ReplayEventResult{}, err
	}

	out := ReplayEventResult{
		EventID: eventID,
		Results: make([]ReplayDeliveryResult, 0, len(locked)),
	}

	for _, d := range locked {
		if d.status == "failed" || d.status == "dead_lettered" {
			newCycle := d.cycle + 1
			if _, err := tx.Exec(ctx, `
				UPDATE deliveries
				SET status = 'pending',
				    cycle = $2,
				    cycle_started_at = now(),
				    attempt_count = 0,
				    attempt_seq = 0,
				    next_attempt_at = now(),
				    lease_token = NULL,
				    lease_expires_at = NULL,
				    worker_id = NULL,
				    last_error_code = NULL,
				    last_error_detail = NULL,
				    last_http_status = NULL,
				    completed_at = NULL,
				    dead_lettered_at = NULL,
				    updated_at = now()
				WHERE id = $1`, d.id, newCycle); err != nil {
				return ReplayEventResult{}, fmt.Errorf("reset delivery for replay: %w", err)
			}
			deliveryID := d.id
			if err := writeAudit(ctx, tx, &tenantID, actor, "event.replay", "delivery", &deliveryID, map[string]any{
				"event_id":       eventID.String(),
				"endpoint_id":    d.endpointID.String(),
				"previous_cycle": d.cycle,
				"cycle":          newCycle,
				"reason":         reason,
			}); err != nil {
				return ReplayEventResult{}, err
			}
			out.Replayed++
			out.Results = append(out.Results, ReplayDeliveryResult{
				DeliveryID:    d.id,
				EndpointID:    d.endpointID,
				PreviousCycle: d.cycle,
				Cycle:         &newCycle,
				Status:        "replayed",
			})
		} else {
			out.Skipped++
			out.Results = append(out.Results, ReplayDeliveryResult{
				DeliveryID:    d.id,
				EndpointID:    d.endpointID,
				PreviousCycle: d.cycle,
				Status:        "skipped",
				Reason:        replaySkipReason(d.status),
			})
		}
	}

	if out.Replayed == 0 {
		return out, &ReplayNotAllowedError{Results: out.Results}
	}

	if err := recomputeEventProjectionTx(ctx, tx, eventID); err != nil {
		return ReplayEventResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplayEventResult{}, err
	}
	return out, nil
}

// AdminReplayDelivery replays a single failed or dead-lettered delivery via the
// internal admin listener and records an admin.replay audit entry.
func (s *Store) AdminReplayDelivery(ctx context.Context, deliveryID uuid.UUID, reason string, actor Actor) (ReplayDeliveryResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ReplayDeliveryResult{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return ReplayDeliveryResult{}, err
	}

	var tenantID, eventID, endpointID uuid.UUID
	var cycle int
	var status string
	err = tx.QueryRow(ctx, `
		SELECT tenant_id, event_id, endpoint_id, cycle, status
		FROM deliveries
		WHERE id = $1
		FOR UPDATE`, deliveryID).Scan(&tenantID, &eventID, &endpointID, &cycle, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplayDeliveryResult{}, ErrNotFound
	}
	if err != nil {
		return ReplayDeliveryResult{}, fmt.Errorf("lock delivery for admin replay: %w", err)
	}

	if status != "failed" && status != "dead_lettered" {
		res := ReplayDeliveryResult{
			DeliveryID:    deliveryID,
			EndpointID:    endpointID,
			PreviousCycle: cycle,
			Status:        "skipped",
			Reason:        replaySkipReason(status),
		}
		return res, &ReplayNotAllowedError{Results: []ReplayDeliveryResult{res}}
	}

	newCycle := cycle + 1
	if _, err := tx.Exec(ctx, `
		UPDATE deliveries
		SET status = 'pending',
		    cycle = $2,
		    cycle_started_at = now(),
		    attempt_count = 0,
		    attempt_seq = 0,
		    next_attempt_at = now(),
		    lease_token = NULL,
		    lease_expires_at = NULL,
		    worker_id = NULL,
		    last_error_code = NULL,
		    last_error_detail = NULL,
		    last_http_status = NULL,
		    completed_at = NULL,
		    dead_lettered_at = NULL,
		    updated_at = now()
		WHERE id = $1`, deliveryID, newCycle); err != nil {
		return ReplayDeliveryResult{}, fmt.Errorf("reset delivery for admin replay: %w", err)
	}

	if err := writeAudit(ctx, tx, &tenantID, actor, "admin.replay", "delivery", &deliveryID, map[string]any{
		"event_id":       eventID.String(),
		"endpoint_id":    endpointID.String(),
		"previous_cycle": cycle,
		"cycle":          newCycle,
		"reason":         reason,
	}); err != nil {
		return ReplayDeliveryResult{}, err
	}

	if err := recomputeEventProjectionTx(ctx, tx, eventID); err != nil {
		return ReplayDeliveryResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplayDeliveryResult{}, err
	}

	return ReplayDeliveryResult{
		DeliveryID:    deliveryID,
		EndpointID:    endpointID,
		PreviousCycle: cycle,
		Cycle:         &newCycle,
		Status:        "replayed",
	}, nil
}

func replaySkipReason(status string) string {
	switch status {
	case "delivered":
		return "already_delivered"
	case "pending", "retry_scheduled":
		return "already_queued"
	case "in_progress":
		return "in_progress"
	case "canceled":
		return "canceled"
	default:
		return "not_replayable"
	}
}

func recomputeEventProjectionTx(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, eventID); err != nil {
		return fmt.Errorf("lock event for status projection: %w", err)
	}
	_, err := tx.Exec(ctx, `
		WITH counts AS (
			SELECT
				count(*) AS total,
				count(*) FILTER (WHERE status = 'delivered') AS delivered,
				count(*) FILTER (WHERE status IN ('failed', 'dead_lettered', 'canceled')) AS terminal_failed,
				count(*) FILTER (WHERE status IN ('pending', 'in_progress', 'retry_scheduled')) AS active
			FROM deliveries
			WHERE event_id = $1
		)
		UPDATE events e
		SET status = CASE
				WHEN counts.total = 0 THEN 'no_subscribers'
				WHEN counts.active > 0 THEN 'delivering'
				WHEN counts.delivered = counts.total THEN 'delivered'
				WHEN counts.delivered > 0 AND counts.terminal_failed > 0 THEN 'partially_delivered'
				ELSE 'failed'
			END,
			updated_at = now()
		FROM counts
		WHERE e.id = $1`, eventID)
	if err != nil {
		return fmt.Errorf("recompute event status projection: %w", err)
	}
	return nil
}

// AdminListTenants returns tenants with endpoint, event, and delivery counts.
func (s *Store) AdminListTenants(ctx context.Context, limit int) ([]AdminTenantSummary, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT t.id, t.slug, t.name, t.status, t.created_at, t.updated_at, t.suspended_at,
		       (SELECT count(*) FROM endpoints ep WHERE ep.tenant_id = t.id AND ep.deleted_at IS NULL),
		       (SELECT count(*) FROM events ev WHERE ev.tenant_id = t.id),
		       (SELECT count(*) FROM deliveries d WHERE d.tenant_id = t.id)
		FROM tenants t
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AdminTenantSummary
	for rows.Next() {
		var item AdminTenantSummary
		if err := rows.Scan(
			&item.ID, &item.Slug, &item.Name, &item.Status,
			&item.CreatedAt, &item.UpdatedAt, &item.SuspendedAt,
			&item.EndpointCount, &item.EventCount, &item.DeliveryCount,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// AdminQueueSummary returns delivery counts by status and oldest due age.
func (s *Store) AdminQueueSummary(ctx context.Context) (AdminQueueStats, error) {
	stats := AdminQueueStats{
		ByStatus: map[string]int64{
			"pending":         0,
			"in_progress":     0,
			"retry_scheduled": 0,
			"delivered":       0,
			"failed":          0,
			"dead_lettered":   0,
			"canceled":        0,
		},
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT status, count(*)
		FROM deliveries
		GROUP BY status`)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var st string
		var cnt int64
		if err := rows.Scan(&st, &cnt); err != nil {
			rows.Close()
			return stats, err
		}
		stats.ByStatus[st] = cnt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stats, err
	}

	err = s.Pool.QueryRow(ctx, `
		SELECT
			count(*),
			COALESCE(GREATEST(EXTRACT(EPOCH FROM (now() - min(next_attempt_at))), 0), 0)::float8
		FROM deliveries
		WHERE status IN ('pending', 'retry_scheduled')
		  AND next_attempt_at <= now()`).Scan(&stats.DueCount, &stats.OldestDueAgeSeconds)
	if err != nil {
		return stats, err
	}

	err = s.Pool.QueryRow(ctx, `
		SELECT count(*)
		FROM deliveries
		WHERE status = 'in_progress'
		  AND lease_expires_at < now()`).Scan(&stats.OverdueLeasesCount)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

// AdminStuckDeliveries returns in_progress deliveries whose lease has expired.
func (s *Store) AdminStuckDeliveries(ctx context.Context, limit int) ([]AdminStuckDelivery, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, tenant_id, event_id, endpoint_id, cycle, attempt_count,
		       worker_id, lease_expires_at,
		       GREATEST(EXTRACT(EPOCH FROM (now() - lease_expires_at)), 0)::float8 AS overdue_seconds
		FROM deliveries
		WHERE status = 'in_progress'
		  AND lease_expires_at < now()
		ORDER BY lease_expires_at ASC, id ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AdminStuckDelivery
	for rows.Next() {
		var item AdminStuckDelivery
		if err := rows.Scan(
			&item.ID, &item.TenantID, &item.EventID, &item.EndpointID,
			&item.Cycle, &item.AttemptCount, &item.WorkerID,
			&item.LeaseExpiresAt, &item.OverdueSeconds,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

const retentionAdvisoryLockKey int64 = 738291045

// RunRetentionPass executes one bounded cleanup pass for expired idempotency
// records, rate-limit counters older than 1 hour, and terminal events older
// than eventRetention. Only one API replica executes at a time via a non-blocking
// PostgreSQL advisory lock.
func (s *Store) RunRetentionPass(ctx context.Context, eventRetention time.Duration) (RetentionSummary, error) {
	var summary RetentionSummary
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return summary, err
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, retentionAdvisoryLockKey).Scan(&locked); err != nil {
		return summary, err
	}
	if !locked {
		return summary, nil
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, retentionAdvisoryLockKey)
	}()

	tag, err := conn.Exec(ctx, `
		DELETE FROM idempotency_records
		WHERE ctid IN (
			SELECT ctid FROM idempotency_records
			WHERE expires_at <= now()
			ORDER BY expires_at
			LIMIT 5000
			FOR UPDATE SKIP LOCKED
		)`)
	if err != nil {
		return summary, fmt.Errorf("delete expired idempotency_records: %w", err)
	}
	summary.IdempotencyDeleted = tag.RowsAffected()

	tag, err = conn.Exec(ctx, `
		DELETE FROM rate_limit_counters
		WHERE ctid IN (
			SELECT ctid FROM rate_limit_counters
			WHERE window_start <= now() - interval '1 hour'
			ORDER BY window_start
			LIMIT 5000
			FOR UPDATE SKIP LOCKED
		)`)
	if err != nil {
		return summary, fmt.Errorf("delete expired rate_limit_counters: %w", err)
	}
	summary.RateLimitsDeleted = tag.RowsAffected()

	if eventRetention > 0 {
		tag, err = conn.Exec(ctx, `
			DELETE FROM events
			WHERE id IN (
				SELECT e.id FROM events e
				WHERE e.created_at <= now() - $1::interval
				  AND NOT EXISTS (
					SELECT 1 FROM deliveries d
					WHERE d.event_id = e.id
					  AND d.status IN ('pending', 'in_progress', 'retry_scheduled')
				  )
				ORDER BY e.created_at
				LIMIT 5000
				FOR UPDATE SKIP LOCKED
			)`, fmt.Sprintf("%d seconds", int(eventRetention.Seconds())))
		if err != nil {
			return summary, fmt.Errorf("delete expired events: %w", err)
		}
		summary.EventsDeleted = tag.RowsAffected()
	}

	return summary, nil
}
