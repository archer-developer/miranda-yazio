package yazio

import "errors"

// Sentinel errors returned by Client methods. Callers should use
// errors.Is against these rather than matching error strings — the
// underlying HTTP status/body text is not a stable contract since YAZIO's
// API is unofficial and undocumented.
var (
	// ErrUnauthorized means YAZIO rejected the access token even after an
	// automatic refresh attempt. Treat this as "the account needs
	// re-authentication" — e.g. the YAZIO password changed or the refresh
	// token was revoked.
	ErrUnauthorized = errors.New("yazio: unauthorized")

	// ErrInvalidCredentials means the configured username/password were
	// rejected outright by the password OAuth grant (login, not a
	// refresh).
	ErrInvalidCredentials = errors.New("yazio: invalid username or password")

	// ErrRateLimited means YAZIO returned 429. Callers should back off
	// before retrying rather than hammering the API.
	ErrRateLimited = errors.New("yazio: rate limited")

	// ErrServiceUnavailable means YAZIO returned a 5xx status — YAZIO's
	// problem, not the request's. Safe to retry later.
	ErrServiceUnavailable = errors.New("yazio: service unavailable")

	// ErrNotFound means the requested product or resource does not exist.
	ErrNotFound = errors.New("yazio: not found")
)
