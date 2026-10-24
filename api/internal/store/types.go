// Package store owns PostgreSQL persistence for the control plane. Every
// query is tenant-scoped except admin lookups, which are explicit methods.
package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/relayforge/relayforge/api/internal/secrets"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrSlugTaken           = errors.New("slug already taken")
	ErrDuplicateEndpoint   = errors.New("duplicate endpoint")
	ErrEndpointLimit       = errors.New("endpoint limit reached")
	ErrLastOwnerCredential = errors.New("last owner credential")
	ErrLastActiveSecret    = errors.New("last active secret")
	ErrSecretOverlapLimit  = errors.New("secret overlap limit")
	ErrRotationRevoked     = errors.New("cannot rotate revoked credential")
	ErrIdempotencyReused   = errors.New("idempotency key reused")
	ErrIdempotencyInFlight = errors.New("idempotency request in progress")
	ErrSchemaExists        = errors.New("schema already exists")
	ErrInvalidSchema       = errors.New("invalid schema")
	ErrReplayNotAllowed    = errors.New("replay not allowed")
)

// Store wraps the pool and Phase 2 policy limits.
type Store struct {
	Pool                  *pgxpool.Pool
	ExpectedSchemaVersion int

	Box             *secrets.Box
	EndpointLimit   int
	IdempotencyTTL  time.Duration
	EventFutureSkew time.Duration
	EventPastWindow time.Duration

	// ingestHook is a test-only fault-injection seam. It runs inside the
	// ingest transaction after the event and deliveries are inserted.
	ingestHook func() error
}

type Tenant struct {
	ID          uuid.UUID  `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
}

type Credential struct {
	ID          uuid.UUID  `json:"id"`
	KeyID       string     `json:"key_id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	RotatedFrom *uuid.UUID `json:"rotated_from"`
}

type AuthCredential struct {
	Credential
	TenantID     uuid.UUID
	SecretHMAC   []byte
	TenantStatus string
}

type NewCredential struct {
	Credential
	APIKey string `json:"api_key"`
}

type Endpoint struct {
	ID                 uuid.UUID  `json:"id"`
	Name               string     `json:"name"`
	URL                string     `json:"url"`
	Status             string     `json:"status"`
	EventTypes         []string   `json:"event_types"`
	MaxAttempts        int        `json:"max_attempts"`
	InitialBackoffMS   int        `json:"initial_backoff_ms"`
	MaxBackoffMS       int        `json:"max_backoff_ms"`
	RetryWindowSeconds int        `json:"retry_window_seconds"`
	TimeoutMS          int        `json:"timeout_ms"`
	RateLimitPerSec    int        `json:"rate_limit_per_sec"`
	ConsecutiveFailure int        `json:"consecutive_failures"`
	DisabledAt         *time.Time `json:"disabled_at,omitempty"`
	DisabledReason     *string    `json:"disabled_reason,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DeletedAt          *time.Time `json:"deleted_at,omitempty"`
}

type NewEndpoint struct {
	Endpoint
	SigningSecret   string    `json:"signing_secret"`
	SigningSecretID uuid.UUID `json:"signing_secret_id"`
}

type SecretMeta struct {
	ID         uuid.UUID  `json:"id"`
	SecretHint string     `json:"secret_hint"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RetiredAt  *time.Time `json:"retired_at"`
}

type NewSecret struct {
	SecretMeta
	SigningSecret string `json:"signing_secret"`
}

type Event struct {
	ID             uuid.UUID `json:"id"`
	Type           string    `json:"type"`
	SchemaVersion  int       `json:"schema_version"`
	Status         string    `json:"status"`
	OccurredAt     time.Time `json:"occurred_at"`
	ReceivedAt     time.Time `json:"received_at"`
	DeliveryCount  int       `json:"delivery_count"`
	DeliveredCount int       `json:"delivered_count"`
	FailedCount    int       `json:"failed_count"`
	CreatedAt      time.Time `json:"created_at"`
}

type DeliverySummary struct {
	ID            uuid.UUID  `json:"id"`
	EndpointID    uuid.UUID  `json:"endpoint_id"`
	EndpointName  string     `json:"endpoint_name"`
	EndpointURL   string     `json:"endpoint_url"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

type EventDetail struct {
	Event
	Payload       any               `json:"payload"`
	PayloadSHA256 string            `json:"payload_sha256"`
	Deliveries    []DeliverySummary `json:"deliveries"`
}

type Delivery struct {
	ID              uuid.UUID  `json:"id"`
	EventID         uuid.UUID  `json:"event_id"`
	EndpointID      uuid.UUID  `json:"endpoint_id"`
	Cycle           int        `json:"cycle"`
	Status          string     `json:"status"`
	AttemptCount    int        `json:"attempt_count"`
	MaxAttempts     int        `json:"max_attempts"`
	NextAttemptAt   time.Time  `json:"next_attempt_at"`
	LastErrorCode   *string    `json:"last_error_code"`
	LastErrorDetail *string    `json:"last_error_detail"`
	LastHTTPStatus  *int       `json:"last_http_status"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	DeadLetteredAt  *time.Time `json:"dead_lettered_at"`
}

type Attempt struct {
	AttemptNo   int        `json:"attempt_no"`
	Cycle       int        `json:"cycle"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	Outcome     *string    `json:"outcome"`
	HTTPStatus  *int       `json:"http_status"`
	ErrorCode   *string    `json:"error_code"`
	ErrorDetail *string    `json:"error_detail"`
	DurationMS  *int       `json:"duration_ms"`
	CursorID    int64      `json:"-"`
}

type AuditEntry struct {
	ID           int64      `json:"id"`
	ActorType    string     `json:"actor_type"`
	ActorID      string     `json:"actor_id"`
	Action       string     `json:"action"`
	ResourceType string     `json:"resource_type"`
	ResourceID   *uuid.UUID `json:"resource_id"`
	RequestID    *string    `json:"request_id"`
	IP           *string    `json:"ip"`
	Metadata     any        `json:"metadata"`
	CreatedAt    time.Time  `json:"created_at"`
}

type EventSchema struct {
	ID        uuid.UUID `json:"id"`
	EventType string    `json:"event_type"`
	Version   int       `json:"version"`
	Schema    any       `json:"schema"`
	CreatedAt time.Time `json:"created_at"`
}

type ReplayDeliveryResult struct {
	DeliveryID    uuid.UUID `json:"delivery_id"`
	EndpointID    uuid.UUID `json:"endpoint_id"`
	PreviousCycle int       `json:"previous_cycle"`
	Cycle         *int      `json:"cycle,omitempty"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
}

type ReplayEventResult struct {
	EventID  uuid.UUID              `json:"event_id"`
	Replayed int                    `json:"replayed"`
	Skipped  int                    `json:"skipped"`
	Results  []ReplayDeliveryResult `json:"results"`
}

type ReplayNotAllowedError struct {
	Results []ReplayDeliveryResult
}

func (e *ReplayNotAllowedError) Error() string { return ErrReplayNotAllowed.Error() }
func (e *ReplayNotAllowedError) Unwrap() error { return ErrReplayNotAllowed }

type AdminTenantSummary struct {
	Tenant
	EndpointCount int64 `json:"endpoint_count"`
	EventCount    int64 `json:"event_count"`
	DeliveryCount int64 `json:"delivery_count"`
}

type AdminQueueStats struct {
	ByStatus            map[string]int64 `json:"by_status"`
	DueCount            int64            `json:"due_count"`
	OldestDueAgeSeconds float64          `json:"oldest_due_age_seconds"`
	OverdueLeasesCount  int64            `json:"overdue_leases_count"`
}

type AdminStuckDelivery struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	EventID        uuid.UUID `json:"event_id"`
	EndpointID     uuid.UUID `json:"endpoint_id"`
	Cycle          int       `json:"cycle"`
	AttemptCount   int       `json:"attempt_count"`
	WorkerID       *string   `json:"worker_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	OverdueSeconds float64   `json:"overdue_seconds"`
}

type RetentionSummary struct {
	IdempotencyDeleted int64 `json:"idempotency_deleted"`
	RateLimitsDeleted  int64 `json:"rate_limits_deleted"`
	EventsDeleted      int64 `json:"events_deleted"`
}
