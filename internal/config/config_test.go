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

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid default", func(*Config) {}, false},
		{"empty http_addr", func(c *Config) { c.HTTPAddr = "" }, true},
		{"empty auth_token_env", func(c *Config) { c.AuthTokenEnv = "" }, true},
		{"empty yazio.username_env", func(c *Config) { c.Yazio.UsernameEnv = "" }, true},
		{"empty yazio.password_env", func(c *Config) { c.Yazio.PasswordEnv = "" }, true},
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
