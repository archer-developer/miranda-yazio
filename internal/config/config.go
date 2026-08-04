// Package config loads the service's YAML configuration and merges it over
// built-in defaults, so the service runs with sane behavior even with an
// empty or missing config.yaml. Default() populates every field, Load()
// applies yaml.Unmarshal on top for a cheap partial merge, validate() rejects
// values that Default() would never produce but a hand-edited file could.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of the service's configuration tree.
type Config struct {
	HTTPAddr     string        `yaml:"http_addr"`
	AuthTokenEnv string        `yaml:"auth_token_env"`
	Yazio        YazioConfig   `yaml:"yazio"`
	Logging      LoggingConfig `yaml:"logging"`
}

// YazioConfig controls how this service talks to YAZIO's unofficial
// v15 REST API.
type YazioConfig struct {
	// UsernameEnv/PasswordEnv name the environment variables holding the
	// YAZIO account credentials used for the password OAuth grant. The
	// values themselves are never stored here — see the top-level
	// Conventions on secrets.
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`

	// TokenCachePath is where the access/refresh token pair is persisted
	// between restarts (mode 0600). Empty means "compute the default at
	// startup": $XDG_CONFIG_HOME/yazio-mcp/token.json, falling back to
	// ~/.config/yazio-mcp/token.json. Left empty here rather than resolved
	// via os.UserHomeDir() in Default() so config stays independent of
	// whatever $HOME happens to be when the config package is imported
	// (e.g. under `go test`) — internal/yazio resolves the empty case.
	TokenCachePath string `yaml:"token_cache_path"`

	// RequestTimeoutSeconds bounds every individual HTTP call made to
	// YAZIO via context.WithTimeout — the unofficial API can hang or go
	// dark without warning.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`

	// DefaultCountry/DefaultLocales are sent on every product search.
	// YAZIO's product database is region-scoped, so searching for a local
	// brand (e.g. a Belarusian producer) without the matching
	// country/locale often returns no results at all. DefaultLocales is a
	// priority-ordered fallback list (most-preferred first) joined with
	// commas on the wire — mirrors how every open-source YAZIO client
	// passes multiple locales despite the API's swagger doc showing a
	// single example value.
	DefaultCountry string   `yaml:"default_country"`
	DefaultLocales []string `yaml:"default_locales"`

	// DefaultSex is sent on product search calls, mirroring what the
	// YAZIO mobile app sends for the logged-in account. It is not
	// exposed as a per-call MCP parameter since it is a stable account
	// attribute, not something Miranda decides per request.
	DefaultSex string `yaml:"default_sex"`
}

// LoggingConfig controls slog output level and verbosity.
type LoggingConfig struct {
	// Level is one of "debug", "info", "warn", "error". At "debug", verbose
	// per-request logs are routed to logs/debug.log instead of stdout to
	// avoid flooding the systemd journal — see cmd/miranda-yazio/main.go.
	Level string `yaml:"level"`
}

// Default returns the built-in configuration. Every field has a safe,
// runnable value so a missing or empty config.yaml still produces a working
// service.
func Default() Config {
	return Config{
		HTTPAddr:     ":8790",
		AuthTokenEnv: "YAZIO_MCP_TOKEN",
		Yazio: YazioConfig{
			UsernameEnv:           "YAZIO_USERNAME",
			PasswordEnv:           "YAZIO_PASSWORD",
			TokenCachePath:        "",
			RequestTimeoutSeconds: 15,
			DefaultCountry:        "BY",
			DefaultLocales:        []string{"by_BY", "ru_RU", "en_EN"},
			DefaultSex:            "male",
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads the YAML file at path and merges it over Default(). A missing
// file is not an error — defaults are used as-is.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (c Config) validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: http_addr must not be empty")
	}
	if c.AuthTokenEnv == "" {
		return fmt.Errorf("config: auth_token_env must not be empty")
	}
	if c.Yazio.UsernameEnv == "" {
		return fmt.Errorf("config: yazio.username_env must not be empty")
	}
	if c.Yazio.PasswordEnv == "" {
		return fmt.Errorf("config: yazio.password_env must not be empty")
	}
	if c.Yazio.RequestTimeoutSeconds < 1 {
		return fmt.Errorf("config: yazio.request_timeout_seconds must be at least 1")
	}
	if c.Yazio.DefaultCountry == "" {
		return fmt.Errorf("config: yazio.default_country must not be empty")
	}
	if len(c.Yazio.DefaultLocales) == 0 {
		return fmt.Errorf("config: yazio.default_locales must not be empty")
	}
	for _, l := range c.Yazio.DefaultLocales {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("config: yazio.default_locales must not contain an empty entry")
		}
	}
	switch c.Yazio.DefaultSex {
	case "male", "female":
	default:
		return fmt.Errorf("config: yazio.default_sex must be one of male|female, got %q", c.Yazio.DefaultSex)
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: logging.level must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}
	return nil
}
