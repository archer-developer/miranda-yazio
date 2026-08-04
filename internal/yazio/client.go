// Package yazio is a client for YAZIO's unofficial v15 REST API
// (https://yzapi.yazio.com/v15). YAZIO has no public developer program —
// this package talks to the same endpoints the official mobile app uses,
// reverse-engineered the same way every other open-source YAZIO client
// (yazio_public_api, go-yazio, the juriadams JS client, ...) does.
//
// Because the API is unofficial and can change without notice, every
// method here returns a wrapped error rather than panicking if a response
// doesn't decode as expected — see errors.go for the sentinel errors
// callers should check with errors.Is.
//
// This package has no MCP dependency; internal/mcpserver wraps it.
package yazio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultBaseURL is YAZIO's unofficial mobile-API host and version.
const defaultBaseURL = "https://yzapi.yazio.com/v15"

// maxResponseBytes caps how much of any single API response we'll read,
// as a defensive limit since we don't control the server.
const maxResponseBytes = 1 << 20 // 1 MiB

// Client talks to YAZIO's unofficial v15 REST API on behalf of one
// account. It is safe for concurrent use.
type Client struct {
	httpClient *http.Client
	baseURL    string
	auth       *authenticator
	timeout    time.Duration
	country    string
	locales    string // pre-joined comma-separated locale priority list
	sex        string
	logger     *slog.Logger
}

// Options configures New. Username and Password are required; every other
// field has a sensible default if left zero.
type Options struct {
	// Username and Password are the YAZIO account credentials used for
	// the password OAuth grant. Required.
	Username string
	Password string

	// TokenCachePath overrides where the access/refresh token pair is
	// cached on disk (mode 0600). Empty uses DefaultTokenCachePath().
	TokenCachePath string

	// RequestTimeout bounds each individual HTTP call to YAZIO. Zero
	// defaults to 15 seconds.
	RequestTimeout time.Duration

	// DefaultCountry/DefaultLocales/DefaultSex are sent on every product
	// search. DefaultLocales is a priority-ordered fallback list
	// (most-preferred first); a zero value for either defaults to
	// "BY"/["by_BY", "ru_RU", "en_EN"]/"male".
	DefaultCountry string
	DefaultLocales []string
	DefaultSex     string

	// BaseURL and HTTPClient override the API base URL and the HTTP
	// client used to reach it. Both exist for tests; leave zero in
	// production.
	BaseURL    string
	HTTPClient *http.Client

	// Logger receives startup/auth diagnostics. Never logs credential or
	// token values. Nil falls back to slog.Default().
	Logger *slog.Logger
}

// New builds a Client. It does not perform any network I/O itself — the
// first real request triggers login (or loads a cached token from disk).
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Username) == "" {
		return nil, fmt.Errorf("yazio: New: Username must not be empty")
	}
	if strings.TrimSpace(opts.Password) == "" {
		return nil, fmt.Errorf("yazio: New: Password must not be empty")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	country := opts.DefaultCountry
	if country == "" {
		country = "BY"
	}
	locales := opts.DefaultLocales
	if len(locales) == 0 {
		locales = []string{"by_BY", "ru_RU", "en_EN"}
	}
	sex := opts.DefaultSex
	if sex == "" {
		sex = "male"
	}

	cachePath := opts.TokenCachePath
	if cachePath == "" {
		cachePath = DefaultTokenCachePath()
	}

	auth := newAuthenticator(httpClient, baseURL, opts.Username, opts.Password, NewTokenStore(cachePath), logger)

	return &Client{
		httpClient: httpClient,
		baseURL:    baseURL,
		auth:       auth,
		timeout:    timeout,
		country:    country,
		locales:    strings.Join(locales, ","),
		sex:        sex,
		logger:     logger,
	}, nil
}

// do performs one authenticated API call, retrying exactly once with a
// forced token refresh if the first attempt comes back 401 — YAZIO
// occasionally invalidates a token server-side before its advertised
// expiry (e.g. after a password change), so a proactively-fresh token can
// still be rejected. It returns the raw response body on any 2xx status.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, reqBody any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var bodyBytes []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("yazio: encode request body: %w", err)
		}
		bodyBytes = b
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.auth.AccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("yazio: authenticate: %w", err)
		}

		respBody, status, err := c.send(ctx, method, path, query, bodyBytes, token)
		if err != nil {
			return nil, fmt.Errorf("yazio: %s %s: %w", method, path, err)
		}

		if status >= 200 && status < 300 {
			return respBody, nil
		}

		if status == http.StatusUnauthorized && attempt == 0 {
			if refreshErr := c.auth.ForceRefresh(ctx); refreshErr != nil {
				return nil, fmt.Errorf("%w: refresh also failed: %v", ErrUnauthorized, refreshErr)
			}
			continue
		}

		return nil, classifyStatus(method, path, status, respBody)
	}

	return nil, ErrUnauthorized
}

func (c *Client) send(ctx context.Context, method, path string, query url.Values, bodyBytes []byte, token string) ([]byte, int, error) {
	reqURL := c.baseURL + path
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}

	// body is declared as the io.Reader interface (not a concrete pointer
	// type) so that leaving it unset produces a true nil interface value
	// for http.NewRequestWithContext — assigning a nil *bytes.Reader to
	// it later would instead produce a non-nil interface wrapping a nil
	// pointer, which panics inside net/http when it tries to read it.
	var body io.Reader
	if bodyBytes != nil {
		body = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Debug-level only (routed to logs/debug.log, not stdout/journal — see
	// cmd/miranda-yazio/main.go's buildLogger). Safe to log verbatim: every
	// request that reaches send() is a product/diary call, never the OAuth
	// exchange (that's authenticator.exchangeLocked, which redacts
	// separately), so neither the request body nor the response body here
	// ever contains a credential or token.
	c.logger.Debug("yazio: request", "method", method, "url", reqURL, "body", string(bodyBytes))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	c.logger.Debug("yazio: response", "method", method, "url", reqURL, "status", resp.StatusCode, "body", string(respBody))

	return respBody, resp.StatusCode, nil
}

func classifyStatus(method, path string, status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return fmt.Errorf("%w (status %d)", ErrServiceUnavailable, status)
	default:
		return fmt.Errorf("yazio: %s %s: unexpected status %d: %s", method, path, status, truncate(body, 300))
	}
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
