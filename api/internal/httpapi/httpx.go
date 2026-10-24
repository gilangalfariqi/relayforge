// Package httpapi wires the public and internal HTTP listeners.
package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/relayforge/relayforge/api/internal/observability"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// Problem is an RFC 9457 problem document with a stable machine code.
type Problem struct {
	Type      string  `json:"type"`
	Title     string  `json:"title"`
	Status    int     `json:"status"`
	Detail    string  `json:"detail,omitempty"`
	Code      string  `json:"code"`
	Reason    string  `json:"reason,omitempty"`
	Instance  string  `json:"instance,omitempty"`
	RequestID string  `json:"request_id,omitempty"`
	Errors    []Field `json:"errors,omitempty"`
}

// Field is one validation failure, addressed by a JSON pointer.
type Field struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
}

func writeProblem(c *gin.Context, status int, code, title, detail string) {
	c.Header("Content-Type", "application/problem+json; charset=utf-8")
	c.AbortWithStatusJSON(status, Problem{
		Type:      "https://relayforge.example/errors/" + strings.ReplaceAll(code, "_", "-"),
		Title:     title,
		Status:    status,
		Detail:    detail,
		Code:      code,
		Instance:  c.Request.URL.Path,
		RequestID: c.GetString(requestIDKey),
	})
}

const requestIDKey = "request_id"

// RequestID accepts a safe caller-supplied ID or generates one.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if !requestIDPattern.MatchString(id) {
			var buf [16]byte
			if _, err := rand.Read(buf[:]); err != nil {
				id = fmt.Sprintf("fallback-%d", time.Now().UnixNano())
			} else {
				id = hex.EncodeToString(buf[:])
			}
		}
		c.Set(requestIDKey, id)
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

// AccessLog emits one structured line per request. It never logs headers,
// query strings, or bodies, which may contain credentials or payload data.
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.LogAttrs(c.Request.Context(), slog.LevelInfo, "http_request",
			slog.String("request_id", c.GetString(requestIDKey)),
			slog.String("method", c.Request.Method),
			slog.String("route", routeLabel(c)),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("remote_ip", c.ClientIP()),
		)
	}
}

// Metrics records request count and latency using the templated route.
func Metrics(m *observability.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		if m == nil {
			c.Next()
			return
		}
		start := time.Now()
		m.InFlight.Inc()
		defer m.InFlight.Dec()
		c.Next()
		m.Observe(routeLabel(c), c.Request.Method, c.Writer.Status(), time.Since(start))
	}
}

func routeLabel(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return "unmatched"
}

// Recovery converts a panic into a generic problem without leaking internals.
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(recovered)
				}
				logger.LogAttrs(c.Request.Context(), slog.LevelError, "panic",
					slog.String("request_id", c.GetString(requestIDKey)),
					slog.String("route", routeLabel(c)),
					slog.Any("panic", recovered),
					slog.String("stack", string(debug.Stack())),
				)
				writeProblem(c, http.StatusInternalServerError, "internal_error",
					"Internal server error", "The request could not be completed.")
			}
		}()
		c.Next()
	}
}

// AdminAuth enforces the X-Admin-Token header on internal routes when a token
// is configured. An empty token disables the check for local development; the
// listener must then be bound to loopback or a private network.
func AdminAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}
		provided := c.GetHeader("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeProblem(c, http.StatusUnauthorized, "unauthorized",
				"Unauthorized", "A valid admin token is required.")
			return
		}
		c.Next()
	}
}

// MaxBodyBytes rejects oversized request bodies before handlers decode them.
func MaxBodyBytes(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > limit {
			writeProblem(c, http.StatusRequestEntityTooLarge, "payload_too_large",
				"Payload too large", fmt.Sprintf("Request body must not exceed %d bytes.", limit))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}
