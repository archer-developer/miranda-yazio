package yazio

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactForm_HidesPasswordAndRefreshToken(t *testing.T) {
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {"some-client-id"},
		"username":   {"alexander"},
		"password":   {"hunter2"},
	}

	got := redactForm(form)

	assert.NotContains(t, got, "hunter2")
	assert.Contains(t, got, "password=%5BREDACTED%5D") // url.Values.Encode() percent-encodes [ and ]
	assert.Contains(t, got, "username=alexander")
	assert.Contains(t, got, "grant_type=password")
}

func TestRedactForm_HidesRefreshTokenValue(t *testing.T) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"super-secret-refresh-token"},
	}

	got := redactForm(form)

	assert.NotContains(t, got, "super-secret-refresh-token")
	assert.Contains(t, got, "refresh_token=%5BREDACTED%5D")
}

func TestRedactTokenFields_HidesAccessAndRefreshToken(t *testing.T) {
	body := []byte(`{"access_token":"secret-access","refresh_token":"secret-refresh","token_type":"bearer","expires_in":172800}`)

	got := redactTokenFields(body)

	assert.NotContains(t, got, "secret-access")
	assert.NotContains(t, got, "secret-refresh")
	assert.Contains(t, got, "[REDACTED]")
	assert.Contains(t, got, `"expires_in":172800`, "non-secret fields stay visible for debugging")
}

func TestRedactTokenFields_FallsBackOnNonJSONBody(t *testing.T) {
	got := redactTokenFields([]byte("<html>not json</html>"))
	assert.Equal(t, "<html>not json</html>", got)
}

// TestDebugLogging_NeverLeaksCredentialsOrTokens drives a real login +
// API call through Client with a slog handler that captures every record,
// then asserts the account password and the issued access/refresh tokens
// never appear in the captured log text — while confirming debug logging
// of the request/response actually happened at all, so this test would
// fail loudly if the log calls were ever removed rather than just redacted.
func TestDebugLogging_NeverLeaksCredentialsOrTokens(t *testing.T) {
	const (
		testPassword = "very-secret-password-xyz"
		accessToken  = "very-secret-access-token-xyz"
		refreshToken = "very-secret-refresh-token-xyz"
	)

	api := &fakeAPI{
		oauthHandler: tokenReply(accessToken, refreshToken),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[{"product_id":"p1","name":"Test product","nutrients":{}}]`)) //nolint:errcheck
		},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := newTestClient(t, api, func(o *Options) {
		o.Password = testPassword
		o.Logger = logger
	})

	_, err := client.SearchProducts(t.Context(), "test")
	require.NoError(t, err)

	logText := logBuf.String()

	assert.NotContains(t, logText, testPassword, "account password must never be logged")
	assert.NotContains(t, logText, accessToken, "access token must never be logged")
	assert.NotContains(t, logText, refreshToken, "refresh token must never be logged")

	assert.Contains(t, logText, "yazio: oauth request", "oauth request should still be logged (redacted)")
	assert.Contains(t, logText, "yazio: oauth response", "oauth response should still be logged (redacted)")
	assert.Contains(t, logText, "yazio: request", "regular API request should be logged")
	assert.Contains(t, logText, "yazio: response", "regular API response should be logged")
	assert.Contains(t, logText, "Test product", "regular API response body is logged verbatim (no secrets in it)")
}

// TestDebugLogging_RoutesToDebugLogFileNotStdout is a lighter-weight sanity
// check that Debug-level records are distinguishable from Info-level ones
// by level alone, matching cmd/miranda-yazio/main.go's levelSplitHandler
// which routes anything below LevelInfo to logs/debug.log instead of
// stdout — this package doesn't own that routing, but its logging must
// use slog.Debug (not Info) for main.go's split to work as intended.
func TestDebugLogging_RoutesToDebugLogFileNotStdout(t *testing.T) {
	var logBuf bytes.Buffer
	// Deliberately set to Info level, like buildLogger's stdout handler —
	// if request/response logging used the wrong level, this would leak
	// the raw request/response into what main.go treats as the journal.
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api, func(o *Options) { o.Logger = logger })

	_, err := client.SearchProducts(t.Context(), "test")
	require.NoError(t, err)

	assert.Empty(t, logBuf.String(), "request/response logging must be Debug-level so it's excluded at Info level")
}
