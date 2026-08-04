package yazio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oauthClientID and oauthClientSecret identify the YAZIO Android app to
// its own backend. YAZIO has no public developer program, so there is no
// per-developer credential to request — every open-source YAZIO client
// (yazio_public_api, go-yazio, the juriadams JS client, ...) uses this
// same pair, extracted from the official mobile app. It is not a secret
// tied to any individual account, so unlike the account username/password
// it is safe to hardcode here rather than route through an env var.
const (
	oauthClientID     = "1_4hiybetvfksgw40o0sog4s884kwc840wwso8go4k8c04goo4c"
	oauthClientSecret = "6rok2m65xuskgkgogw40wkkk8sw0osg84s8cggsc4woos4s8o"
)

// maxOAuthResponseBytes caps how much of the token endpoint's response
// body we'll read, as a defensive limit against an unexpectedly large
// response from an API we don't control.
const maxOAuthResponseBytes = 1 << 20 // 1 MiB

// authenticator owns the YAZIO OAuth token for one account: it logs in
// with a password grant on first use, refreshes proactively before expiry,
// and persists the token pair to disk so a service restart doesn't force
// a fresh password login. Safe for concurrent use.
type authenticator struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
	store      *TokenStore
	logger     *slog.Logger

	mu     sync.Mutex
	token  Token
	loaded bool // whether we've attempted to load the cache from disk yet
}

func newAuthenticator(httpClient *http.Client, baseURL, username, password string, store *TokenStore, logger *slog.Logger) *authenticator {
	return &authenticator{
		httpClient: httpClient,
		baseURL:    baseURL,
		username:   username,
		password:   password,
		store:      store,
		logger:     logger,
	}
}

// AccessToken returns a currently-usable access token, logging in or
// refreshing as needed. It never logs the token value itself.
func (a *authenticator) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ensureLoadedLocked()

	if a.token.usable() {
		return a.token.AccessToken, nil
	}

	if a.token.RefreshToken != "" {
		err := a.refreshLocked(ctx)
		if err == nil {
			return a.token.AccessToken, nil
		}
		a.logger.Warn("yazio: refresh token was rejected, falling back to password login", "error", err)
	}

	if err := a.loginLocked(ctx); err != nil {
		return "", err
	}
	return a.token.AccessToken, nil
}

// ForceRefresh discards any in-memory freshness assumption and obtains a
// new token unconditionally — used by Client after an unexpected 401 to
// recover from a token that YAZIO invalidated server-side before its
// advertised expiry.
func (a *authenticator) ForceRefresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token.RefreshToken != "" {
		if err := a.refreshLocked(ctx); err == nil {
			return nil
		}
	}
	return a.loginLocked(ctx)
}

func (a *authenticator) ensureLoadedLocked() {
	if a.loaded {
		return
	}
	a.loaded = true

	t, err := a.store.Load()
	if err != nil {
		a.logger.Warn("yazio: failed to load cached token, will log in fresh", "error", err)
		return
	}
	a.token = t
}

func (a *authenticator) loginLocked(ctx context.Context) error {
	return a.exchangeLocked(ctx, url.Values{
		"grant_type":    {"password"},
		"client_id":     {oauthClientID},
		"client_secret": {oauthClientSecret},
		"username":      {a.username},
		"password":      {a.password},
	})
}

func (a *authenticator) refreshLocked(ctx context.Context) error {
	return a.exchangeLocked(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {oauthClientID},
		"client_secret": {oauthClientSecret},
		"refresh_token": {a.token.RefreshToken},
	})
}

// exchangeLocked performs a POST /oauth/token call (password or refresh
// grant, selected by form) and, on success, updates a.token and persists
// it. Caller must hold a.mu.
func (a *authenticator) exchangeLocked(ctx context.Context, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("yazio: build oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	// Debug-level only, and with password/refresh_token redacted — unlike
	// Client.send's request/response logging, this endpoint's request body
	// carries the account password and its response body carries the
	// access/refresh token, so these can never be logged verbatim (see the
	// top-level Conventions on secrets).
	a.logger.Debug("yazio: oauth request", "url", a.baseURL+"/oauth/token", "form", redactForm(form))

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("yazio: oauth token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes))
	if err != nil {
		return fmt.Errorf("yazio: read oauth response: %w", err)
	}

	a.logger.Debug("yazio: oauth response", "status", resp.StatusCode, "body", redactTokenFields(body))

	switch {
	case resp.StatusCode == http.StatusOK:
		// fall through to decode below
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return ErrInvalidCredentials
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w (oauth token endpoint returned %d)", ErrServiceUnavailable, resp.StatusCode)
	default:
		return fmt.Errorf("yazio: oauth token exchange failed with unexpected status %d", resp.StatusCode)
	}

	var dto struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return fmt.Errorf("yazio: decode oauth response (API may have changed shape): %w", err)
	}
	if dto.AccessToken == "" || dto.RefreshToken == "" || dto.ExpiresIn == 0 {
		return fmt.Errorf("yazio: oauth response missing access_token/refresh_token/expires_in — YAZIO may have changed its API")
	}

	a.token = Token{
		AccessToken:  dto.AccessToken,
		RefreshToken: dto.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(dto.ExpiresIn) * time.Second),
	}

	if err := a.store.Save(a.token); err != nil {
		// Non-fatal: the in-memory token is still usable for this run —
		// only future restarts lose the cache and need a fresh login.
		a.logger.Warn("yazio: failed to persist token cache", "error", err)
	}

	return nil
}

// redactForm renders form for logging with password/refresh_token values
// replaced — grant_type/client_id/username are not secrets and are useful
// for telling a login attempt apart from a refresh in the debug log.
func redactForm(form url.Values) string {
	redacted := url.Values{}
	for k, v := range form {
		if k == "password" || k == "refresh_token" {
			redacted[k] = []string{"[REDACTED]"}
			continue
		}
		redacted[k] = v
	}
	return redacted.Encode()
}

// redactTokenFields returns body with any "access_token"/"refresh_token"
// JSON field values replaced, for safe debug logging of the oauth
// endpoint's response. Falls back to a truncated raw string if body isn't
// valid JSON (e.g. an HTML error page from an outage) — still safe, since
// a non-JSON body can't carry a token value under either of those keys.
func redactTokenFields(body []byte) string {
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return truncate(body, 300)
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if _, ok := generic[key]; ok {
			generic[key] = "[REDACTED]"
		}
	}
	redacted, err := json.Marshal(generic)
	if err != nil {
		return truncate(body, 300)
	}
	return string(redacted)
}
