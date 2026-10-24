// Package config loads and validates process configuration from the environment.
//
// Configuration is fail-fast: an invalid or missing required value stops the
// process at startup instead of surfacing as a request-time error.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr        = ":8080"
	defaultAdminAddr       = ":9090"
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 15 * time.Second
	defaultReadinessTime   = 2 * time.Second
	defaultMigrationsDir   = "migrations"
	defaultDBMaxConns      = 10
	defaultMaxEventBytes   = 262144
	defaultEndpointLimit   = 50
	defaultIdempotencyTTL  = 24 * time.Hour
	defaultFutureSkew      = 5 * time.Minute
	defaultPastWindow      = 30 * 24 * time.Hour
	defaultIngestRateLimit = 50
	defaultMgmtRateLimit   = 10
	defaultAdminRateLimit  = 20
	defaultRetentionEvery  = 5 * time.Minute
	defaultRetentionEvents = 30 * 24 * time.Hour
)

var keyEnvPattern = regexp.MustCompile(`^[a-z]{2,8}$`)

// Config is the validated configuration for the API and migrate binaries.
type Config struct {
	Env                   string
	HTTPAddr              string
	AdminAddr             string
	DatabaseURL           string
	LogLevel              string
	ShutdownTimeout       time.Duration
	ReadinessTimeout      time.Duration
	ExpectedSchemaVersion int
	MigrationsDir         string
	OTLPEndpoint          string
	AdminToken            string
	DBMaxConns            int32

	// Phase 2 secrets and limits.
	APIKeyPepper   []byte
	KeyEnv         string
	KEK            []byte
	KEKVersion     int
	SignupMode     string
	SignupToken    string
	MaxEventBytes  int64
	EndpointLimit  int
	IdempotencyTTL time.Duration
	FutureSkew     time.Duration
	OccurredPast   time.Duration

	// Phase 4 rate limits and retention.
	RateLimitIngestPerSec     int
	RateLimitManagementPerSec int
	RateLimitAdminPerSec      int
	RetentionEnabled          bool
	RetentionInterval         time.Duration
	RetentionEvents           time.Duration
}

// Load reads the environment and returns a validated configuration.
func Load() (Config, error) {
	cfg := Config{
		Env:           getenv("RELAYFORGE_ENV", "development"),
		HTTPAddr:      getenv("RELAYFORGE_HTTP_ADDR", defaultHTTPAddr),
		AdminAddr:     getenv("RELAYFORGE_ADMIN_ADDR", defaultAdminAddr),
		DatabaseURL:   os.Getenv("RELAYFORGE_DATABASE_URL"),
		LogLevel:      getenv("RELAYFORGE_LOG_LEVEL", defaultLogLevel),
		MigrationsDir: getenv("RELAYFORGE_MIGRATIONS_DIR", defaultMigrationsDir),
		OTLPEndpoint:  os.Getenv("RELAYFORGE_OTEL_ENDPOINT"),
		AdminToken:    os.Getenv("RELAYFORGE_ADMIN_TOKEN"),
	}

	var err error
	if cfg.ExpectedSchemaVersion, err = getenvInt("RELAYFORGE_EXPECTED_SCHEMA_VERSION", 5); err != nil {
		return Config{}, err
	}
	dbMaxConns, err := getenvInt("RELAYFORGE_DB_MAX_CONNS", defaultDBMaxConns)
	if err != nil {
		return Config{}, err
	}
	cfg.DBMaxConns = int32(dbMaxConns)
	if cfg.ShutdownTimeout, err = getenvDuration("RELAYFORGE_SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ReadinessTimeout, err = getenvDuration("RELAYFORGE_READINESS_TIMEOUT", defaultReadinessTime); err != nil {
		return Config{}, err
	}
	cfg.SignupMode = getenv("RELAYFORGE_SIGNUP_MODE", "open")
	cfg.SignupToken = os.Getenv("RELAYFORGE_SIGNUP_TOKEN")
	cfg.KeyEnv = getenv("RELAYFORGE_KEY_ENV", "live")
	if cfg.APIKeyPepper, err = getenvBase64("RELAYFORGE_API_KEY_PEPPER", true); err != nil {
		return Config{}, err
	}
	if cfg.KEK, err = getenvBase64("RELAYFORGE_KEK", true); err != nil {
		return Config{}, err
	}
	if cfg.KEKVersion, err = getenvInt("RELAYFORGE_KEK_VERSION", 1); err != nil {
		return Config{}, err
	}
	maxEventBytes, err := getenvInt("RELAYFORGE_MAX_EVENT_BYTES", defaultMaxEventBytes)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxEventBytes = int64(maxEventBytes)
	if cfg.EndpointLimit, err = getenvInt("RELAYFORGE_ENDPOINT_LIMIT", defaultEndpointLimit); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyTTL, err = getenvDuration("RELAYFORGE_IDEMPOTENCY_TTL", defaultIdempotencyTTL); err != nil {
		return Config{}, err
	}
	if cfg.FutureSkew, err = getenvDuration("RELAYFORGE_EVENT_FUTURE_SKEW", defaultFutureSkew); err != nil {
		return Config{}, err
	}
	if cfg.OccurredPast, err = getenvDuration("RELAYFORGE_EVENT_PAST_WINDOW", defaultPastWindow); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitIngestPerSec, err = getenvInt("RELAYFORGE_RATE_LIMIT_INGEST_PER_SEC", defaultIngestRateLimit); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitManagementPerSec, err = getenvInt("RELAYFORGE_RATE_LIMIT_MANAGEMENT_PER_SEC", defaultMgmtRateLimit); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitAdminPerSec, err = getenvInt("RELAYFORGE_RATE_LIMIT_ADMIN_PER_SEC", defaultAdminRateLimit); err != nil {
		return Config{}, err
	}
	cfg.RetentionEnabled = !strings.EqualFold(getenv("RELAYFORGE_RETENTION_ENABLED", "true"), "false")
	if cfg.RetentionInterval, err = getenvDuration("RELAYFORGE_RETENTION_INTERVAL", defaultRetentionEvery); err != nil {
		return Config{}, err
	}
	if cfg.RetentionEvents, err = getenvDuration("RELAYFORGE_RETENTION_EVENTS", defaultRetentionEvents); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks cross-field and format constraints.
func (c Config) Validate() error {
	var errs []error

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("RELAYFORGE_DATABASE_URL is required"))
	} else if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		errs = append(errs, errors.New("RELAYFORGE_DATABASE_URL must be a postgres:// or postgresql:// URL"))
	} else if c.Env == "production" || c.Env == "staging" {
		switch u.Query().Get("sslmode") {
		case "require", "verify-ca", "verify-full":
		default:
			errs = append(errs, fmt.Errorf("RELAYFORGE_DATABASE_URL must set sslmode=require, verify-ca, or verify-full in %s", c.Env))
		}
	}

	if err := validateAddr("RELAYFORGE_HTTP_ADDR", c.HTTPAddr); err != nil {
		errs = append(errs, err)
	}
	if err := validateAddr("RELAYFORGE_ADMIN_ADDR", c.AdminAddr); err != nil {
		errs = append(errs, err)
	}
	if c.HTTPAddr == c.AdminAddr {
		errs = append(errs, errors.New("RELAYFORGE_HTTP_ADDR and RELAYFORGE_ADMIN_ADDR must differ"))
	}

	switch c.Env {
	case "development", "test", "staging", "production":
	default:
		errs = append(errs, fmt.Errorf("RELAYFORGE_ENV %q must be one of development, test, staging, production", c.Env))
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("RELAYFORGE_LOG_LEVEL %q must be one of debug, info, warn, error", c.LogLevel))
	}

	if c.ExpectedSchemaVersion < 1 {
		errs = append(errs, errors.New("RELAYFORGE_EXPECTED_SCHEMA_VERSION must be >= 1"))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("RELAYFORGE_SHUTDOWN_TIMEOUT must be positive"))
	}
	if c.ReadinessTimeout <= 0 {
		errs = append(errs, errors.New("RELAYFORGE_READINESS_TIMEOUT must be positive"))
	}
	if c.DBMaxConns < 1 {
		errs = append(errs, errors.New("RELAYFORGE_DB_MAX_CONNS must be >= 1"))
	}
	if c.MigrationsDir == "" {
		errs = append(errs, errors.New("RELAYFORGE_MIGRATIONS_DIR must not be empty"))
	}
	if c.OTLPEndpoint != "" {
		if u, err := url.Parse(c.OTLPEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, errors.New("RELAYFORGE_OTEL_ENDPOINT must be an http(s) URL"))
		}
	}
	if c.Env == "production" && c.AdminToken == "" {
		errs = append(errs, errors.New("RELAYFORGE_ADMIN_TOKEN is required in production"))
	}
	if len(c.APIKeyPepper) < 32 {
		errs = append(errs, errors.New("RELAYFORGE_API_KEY_PEPPER must decode to at least 32 bytes"))
	}
	if len(c.KEK) != 32 {
		errs = append(errs, errors.New("RELAYFORGE_KEK must decode to exactly 32 bytes"))
	}
	if c.KEKVersion < 1 {
		errs = append(errs, errors.New("RELAYFORGE_KEK_VERSION must be >= 1"))
	}
	if !keyEnvPattern.MatchString(c.KeyEnv) {
		errs = append(errs, errors.New("RELAYFORGE_KEY_ENV must be 2-8 lowercase letters"))
	}
	switch c.SignupMode {
	case "open", "token", "disabled":
	default:
		errs = append(errs, errors.New("RELAYFORGE_SIGNUP_MODE must be open, token, or disabled"))
	}
	if c.SignupMode == "token" && len(c.SignupToken) < 16 {
		errs = append(errs, errors.New("RELAYFORGE_SIGNUP_TOKEN must be at least 16 characters when signup mode is token"))
	}
	if c.MaxEventBytes < 1 || c.MaxEventBytes > defaultMaxEventBytes {
		errs = append(errs, fmt.Errorf("RELAYFORGE_MAX_EVENT_BYTES must be between 1 and %d", defaultMaxEventBytes))
	}
	if c.EndpointLimit < 1 || c.EndpointLimit > 50 {
		errs = append(errs, errors.New("RELAYFORGE_ENDPOINT_LIMIT must be between 1 and 50"))
	}
	if c.IdempotencyTTL < time.Hour || c.IdempotencyTTL > 168*time.Hour {
		errs = append(errs, errors.New("RELAYFORGE_IDEMPOTENCY_TTL must be between 1h and 168h"))
	}
	if c.FutureSkew <= 0 || c.FutureSkew > time.Hour {
		errs = append(errs, errors.New("RELAYFORGE_EVENT_FUTURE_SKEW must be between 1ns and 1h"))
	}
	if c.OccurredPast <= 0 || c.OccurredPast > 90*24*time.Hour {
		errs = append(errs, errors.New("RELAYFORGE_EVENT_PAST_WINDOW must be between 1ns and 2160h"))
	}

	return errors.Join(errs...)
}

func validateAddr(name, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q is not host:port: %w", name, addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s has an invalid port %q", name, port)
	}
	return nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func getenvDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func getenvBase64(key string, required bool) ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		if required {
			return nil, fmt.Errorf("%s is required", key)
		}
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be standard base64: %w", key, err)
	}
	return b, nil
}
