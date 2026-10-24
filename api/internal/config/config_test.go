package config

import (
	"strings"
	"testing"
	"time"
)

const (
	testPepper = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	testKEK    = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"RELAYFORGE_ENV", "RELAYFORGE_HTTP_ADDR", "RELAYFORGE_ADMIN_ADDR",
		"RELAYFORGE_DATABASE_URL", "RELAYFORGE_LOG_LEVEL", "RELAYFORGE_SHUTDOWN_TIMEOUT",
		"RELAYFORGE_READINESS_TIMEOUT", "RELAYFORGE_EXPECTED_SCHEMA_VERSION",
		"RELAYFORGE_MIGRATIONS_DIR", "RELAYFORGE_OTEL_ENDPOINT", "RELAYFORGE_ADMIN_TOKEN",
		"RELAYFORGE_DB_MAX_CONNS", "RELAYFORGE_API_KEY_PEPPER", "RELAYFORGE_KEK",
		"RELAYFORGE_KEK_VERSION", "RELAYFORGE_SIGNUP_MODE", "RELAYFORGE_SIGNUP_TOKEN",
		"RELAYFORGE_KEY_ENV", "RELAYFORGE_MAX_EVENT_BYTES", "RELAYFORGE_ENDPOINT_LIMIT",
		"RELAYFORGE_IDEMPOTENCY_TTL", "RELAYFORGE_EVENT_FUTURE_SKEW", "RELAYFORGE_EVENT_PAST_WINDOW",
	} {
		t.Setenv(key, "")
	}
}

func setRequiredSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("RELAYFORGE_API_KEY_PEPPER", testPepper)
	t.Setenv("RELAYFORGE_KEK", testKEK)
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAYFORGE_DATABASE_URL", "postgres://relayforge:secret@localhost:5432/relayforge?sslmode=disable")
	setRequiredSecrets(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.AdminAddr != ":9090" {
		t.Fatalf("unexpected addresses: %q %q", cfg.HTTPAddr, cfg.AdminAddr)
	}
	if cfg.ExpectedSchemaVersion != 5 {
		t.Fatalf("expected schema version = %d, want 5", cfg.ExpectedSchemaVersion)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("shutdown timeout = %v", cfg.ShutdownTimeout)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	setRequiredSecrets(t)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "RELAYFORGE_DATABASE_URL") {
		t.Fatalf("expected database URL error, got %v", err)
	}
}

func TestLoadRejectsMalformedInteger(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAYFORGE_DATABASE_URL", "postgres://relayforge:secret@localhost:5432/relayforge?sslmode=disable")
	setRequiredSecrets(t)
	t.Setenv("RELAYFORGE_EXPECTED_SCHEMA_VERSION", "five")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "RELAYFORGE_EXPECTED_SCHEMA_VERSION") {
		t.Fatalf("expected schema version parse error, got %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	base := Config{
		Env:                   "development",
		HTTPAddr:              ":8080",
		AdminAddr:             ":9090",
		DatabaseURL:           "postgres://localhost/db",
		LogLevel:              "info",
		ShutdownTimeout:       time.Second,
		ReadinessTimeout:      time.Second,
		ExpectedSchemaVersion: 5,
		MigrationsDir:         "migrations",
		DBMaxConns:            10,
		APIKeyPepper:          []byte("01234567890123456789012345678901"),
		KEK:                   []byte("01234567890123456789012345678901"),
		KEKVersion:            1,
		KeyEnv:                "live",
		SignupMode:            "open",
		MaxEventBytes:         262144,
		EndpointLimit:         50,
		IdempotencyTTL:        24 * time.Hour,
		FutureSkew:            5 * time.Minute,
		OccurredPast:          30 * 24 * time.Hour,
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad scheme", func(c *Config) { c.DatabaseURL = "mysql://localhost/db" }, "postgres://"},
		{"bad http addr", func(c *Config) { c.HTTPAddr = "localhost" }, "host:port"},
		{"same addr", func(c *Config) { c.AdminAddr = c.HTTPAddr }, "must differ"},
		{"bad env", func(c *Config) { c.Env = "prod" }, "RELAYFORGE_ENV"},
		{"bad log level", func(c *Config) { c.LogLevel = "verbose" }, "RELAYFORGE_LOG_LEVEL"},
		{"zero schema", func(c *Config) { c.ExpectedSchemaVersion = 0 }, "EXPECTED_SCHEMA_VERSION"},
		{"bad otel", func(c *Config) { c.OTLPEndpoint = "grpc://collector" }, "OTEL_ENDPOINT"},
		{"prod without admin token", func(c *Config) { c.Env = "production" }, "ADMIN_TOKEN"},
		{"prod without TLS to database", func(c *Config) {
			c.Env = "production"
			c.AdminToken = "token"
			c.DatabaseURL = "postgres://localhost/db?sslmode=disable"
		}, "sslmode=require"},
		{"staging without TLS to database", func(c *Config) {
			c.Env = "staging"
			c.DatabaseURL = "postgres://localhost/db"
		}, "sslmode=require"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsProductionWithToken(t *testing.T) {
	c := Config{
		Env:                   "production",
		HTTPAddr:              ":8080",
		AdminAddr:             ":9090",
		DatabaseURL:           "postgresql://db/relayforge?sslmode=require",
		LogLevel:              "warn",
		ShutdownTimeout:       time.Second,
		ReadinessTimeout:      time.Second,
		ExpectedSchemaVersion: 5,
		MigrationsDir:         "migrations",
		AdminToken:            "test-token",
		DBMaxConns:            10,
		APIKeyPepper:          []byte("01234567890123456789012345678901"),
		KEK:                   []byte("01234567890123456789012345678901"),
		KEKVersion:            1,
		KeyEnv:                "live",
		SignupMode:            "open",
		MaxEventBytes:         262144,
		EndpointLimit:         50,
		IdempotencyTTL:        24 * time.Hour,
		FutureSkew:            5 * time.Minute,
		OccurredPast:          30 * 24 * time.Hour,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
