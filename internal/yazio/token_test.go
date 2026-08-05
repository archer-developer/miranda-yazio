package yazio

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenStore_LoadMissingFileReturnsZeroToken(t *testing.T) {
	store := NewTokenStore(filepath.Join(t.TempDir(), "does-not-exist.json"))

	tok, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, Token{}, tok)
}

func TestTokenStore_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token.json")
	store := NewTokenStore(path)

	want := Token{
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		ExpiresAt:    time.Now().Add(2 * time.Hour).Truncate(time.Second),
	}
	require.NoError(t, store.Save(want))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, want.AccessToken, got.AccessToken)
	assert.Equal(t, want.RefreshToken, got.RefreshToken)
	assert.True(t, want.ExpiresAt.Equal(got.ExpiresAt))
}

func TestTokenStore_SavesWithRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits don't apply on Windows")
	}

	path := filepath.Join(t.TempDir(), "token.json")
	store := NewTokenStore(path)
	require.NoError(t, store.Save(Token{AccessToken: "a", RefreshToken: "b", ExpiresAt: time.Now()}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestDefaultTokenCachePath_PrefersXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg-home")

	path := DefaultTokenCachePath()
	assert.Equal(t, filepath.Join("/xdg-home", "yazio-mcp", "token.json"), path)
}

func TestDefaultTokenCachePath_FallsBackToDotConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	path := DefaultTokenCachePath()
	assert.Equal(t, filepath.Join(home, ".config", "yazio-mcp", "token.json"), path)
}

func TestTokenCachePathForUser_UsesDefaultDirWhenEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg-home")

	path := TokenCachePathForUser("", "archer")
	assert.Equal(t, filepath.Join("/xdg-home", "yazio-mcp", "token-archer.json"), path)
}

func TestTokenCachePathForUser_HonorsExplicitDir(t *testing.T) {
	path := TokenCachePathForUser("/custom/dir", "archer")
	assert.Equal(t, filepath.Join("/custom/dir", "token-archer.json"), path)
}

func TestToken_UsableRespectsRefreshBuffer(t *testing.T) {
	tests := []struct {
		name   string
		token  Token
		usable bool
	}{
		{"empty token", Token{}, false},
		{"expires far in the future", Token{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}, true},
		{"expires within the refresh buffer", Token{AccessToken: "a", ExpiresAt: time.Now().Add(time.Minute)}, false},
		{"already expired", Token{AccessToken: "a", ExpiresAt: time.Now().Add(-time.Minute)}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.usable, tt.token.usable())
		})
	}
}
