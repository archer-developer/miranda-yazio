package yazio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// tokenRefreshBuffer treats a token as expired this long before its actual
// expiry, so a call that starts just before expiry doesn't race a token
// that dies mid-request.
const tokenRefreshBuffer = 5 * time.Minute

// Token is an OAuth access/refresh token pair as cached on disk and held
// in memory by authenticator.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// usable reports whether t can be used right now without refreshing first.
func (t Token) usable() bool {
	return t.AccessToken != "" && time.Now().Before(t.ExpiresAt.Add(-tokenRefreshBuffer))
}

// defaultTokenCacheDir returns $XDG_CONFIG_HOME/yazio-mcp, or
// ~/.config/yazio-mcp if XDG_CONFIG_HOME is unset. This follows the XDG
// base directory spec explicitly rather than os.UserConfigDir() (which
// returns a different, platform-specific path on macOS) since the service
// is deployed to a Linux host under systemd --user regardless of what
// platform it's developed on. Shared by DefaultTokenCachePath and
// TokenCachePathForUser so there's one place that resolves this default.
func defaultTokenCacheDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "yazio-mcp")
}

// DefaultTokenCachePath returns the legacy single-account token cache
// file path (defaultTokenCacheDir()/token.json) — kept for callers that
// pass Options.TokenCachePath directly (see yazio.Client.New) rather than
// going through the per-user TokenCachePathForUser.
func DefaultTokenCachePath() string {
	return filepath.Join(defaultTokenCacheDir(), "token.json")
}

// TokenCachePathForUser returns the token cache file for one configured
// YAZIO account under dir, or under defaultTokenCacheDir() if dir is
// empty. Each account gets its own file — token pairs can't be shared
// between accounts — named after the account's config key so a service
// restart can find the right cache without any extra per-user config.
func TokenCachePathForUser(dir, name string) string {
	if dir == "" {
		dir = defaultTokenCacheDir()
	}
	return filepath.Join(dir, "token-"+name+".json")
}

// TokenStore persists a Token to a single JSON file with 0600 permissions,
// since it contains a live refresh token.
type TokenStore struct {
	path string
}

// NewTokenStore returns a TokenStore backed by the file at path.
func NewTokenStore(path string) *TokenStore {
	return &TokenStore{path: path}
}

// Load reads the cached token. A missing file is not an error — it just
// means there's nothing cached yet — and returns the zero Token.
func (s *TokenStore) Load() (Token, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Token{}, nil
		}
		return Token{}, fmt.Errorf("yazio: read token cache %s: %w", s.path, err)
	}

	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, fmt.Errorf("yazio: parse token cache %s: %w", s.path, err)
	}
	return t, nil
}

// Save writes t to disk atomically (write to a temp file, then rename)
// with 0600 permissions, creating the parent directory (0700) if needed.
func (s *TokenStore) Save(t Token) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("yazio: create token cache dir %s: %w", dir, err)
	}

	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("yazio: encode token cache: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".token-*.json.tmp")
	if err != nil {
		return fmt.Errorf("yazio: create temp token file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("yazio: write temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("yazio: close temp token file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("yazio: chmod temp token file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("yazio: rename temp token file into place: %w", err)
	}
	return nil
}
