package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefault_IsValid(t *testing.T) {
	assert.NoError(t, Default().validate())
}

func TestLoad_NoPathsReturnsDefaults(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, Default(), cfg)
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)
	assert.Equal(t, Default(), cfg)
}

func TestLoad_OverridesMergeOverDefaults(t *testing.T) {
	path := writeConfigFile(t, "http_addr: \":9999\"\n")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, Default().AuthTokenEnv, cfg.AuthTokenEnv, "unset fields keep their default")
}

func TestLoad_InvalidYAMLIsError(t *testing.T) {
	path := writeConfigFile(t, "http_addr: [not a string\n")

	_, err := Load(path)
	assert.Error(t, err)
}

func TestLoad_MergesMultipleFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "01-base.yaml")
	second := filepath.Join(dir, "02-override.yaml")
	require.NoError(t, os.WriteFile(first, []byte("http_addr: \":1111\"\nauth_token_env: \"FIRST_TOKEN\"\n"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("http_addr: \":2222\"\n"), 0o600))

	// A missing file mixed in should just be skipped, not break the merge.
	missing := filepath.Join(dir, "00-missing.yaml")

	cfg, err := Load(missing, first, second)
	require.NoError(t, err)
	assert.Equal(t, ":2222", cfg.HTTPAddr, "later file's field wins over an earlier file's")
	assert.Equal(t, "FIRST_TOKEN", cfg.AuthTokenEnv, "field only the earlier file sets is kept")
}

func TestLoad_TrimsUserNameWhitespace(t *testing.T) {
	path := writeConfigFile(t, "yazio:\n  users:\n    - name: \"archer \"\n      username_env: \"YAZIO_USERNAME_ARCHER\"\n      password_env: \"YAZIO_PASSWORD_ARCHER\"\n")

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Yazio.Users, 1)
	assert.Equal(t, "archer", cfg.Yazio.Users[0].Name, "trailing whitespace must be trimmed so the name matches what mcpserver.resolveClient trims from a caller's \"user\" input")
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid default", func(*Config) {}, false},
		{"empty http_addr", func(c *Config) { c.HTTPAddr = "" }, true},
		{"empty auth_token_env", func(c *Config) { c.AuthTokenEnv = "" }, true},
		{"empty yazio.users", func(c *Config) { c.Yazio.Users = nil }, true},
		{"empty yazio.users[0].name", func(c *Config) { c.Yazio.Users[0].Name = "" }, true},
		{"yazio.users[0].name contains a slash", func(c *Config) { c.Yazio.Users[0].Name = "../etc" }, true},
		{"yazio.users[0].name contains whitespace", func(c *Config) { c.Yazio.Users[0].Name = "arc her" }, true},
		{"duplicate yazio.users[].name", func(c *Config) { c.Yazio.Users = append(c.Yazio.Users, c.Yazio.Users[0]) }, true},
		{"empty yazio.users[0].username_env", func(c *Config) { c.Yazio.Users[0].UsernameEnv = "" }, true},
		{"empty yazio.users[0].password_env", func(c *Config) { c.Yazio.Users[0].PasswordEnv = "" }, true},
		{"duplicate yazio.users[].username_env", func(c *Config) {
			c.Yazio.Users = append(c.Yazio.Users, YazioUser{Name: "second", UsernameEnv: c.Yazio.Users[0].UsernameEnv, PasswordEnv: "SECOND_PASSWORD"})
		}, true},
		{"duplicate yazio.users[].password_env", func(c *Config) {
			c.Yazio.Users = append(c.Yazio.Users, YazioUser{Name: "second", UsernameEnv: "SECOND_USERNAME", PasswordEnv: c.Yazio.Users[0].PasswordEnv})
		}, true},
		{"zero yazio.request_timeout_seconds", func(c *Config) { c.Yazio.RequestTimeoutSeconds = 0 }, true},
		{"empty yazio.default_country", func(c *Config) { c.Yazio.DefaultCountry = "" }, true},
		{"empty yazio.default_locales", func(c *Config) { c.Yazio.DefaultLocales = nil }, true},
		{"blank entry in yazio.default_locales", func(c *Config) { c.Yazio.DefaultLocales = []string{"ru_RU", "  "} }, true},
		{"invalid yazio.default_sex", func(c *Config) { c.Yazio.DefaultSex = "other" }, true},
		{"invalid logging level", func(c *Config) { c.Logging.Level = "verbose" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
