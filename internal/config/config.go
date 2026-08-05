// Package config loads the service's YAML configuration and merges it over
// built-in defaults, so the service runs with sane behavior even with no
// config files at all. Default() populates every field, Load() applies
// yaml.Unmarshal on top — once per file, in order — for a cheap partial
// merge, validate() rejects values that Default() would never produce but
// a hand-edited file could.
//
// config/config.yaml.dist (checked into git) documents every field with its
// default value; the actual config/*.yaml files Load reads are gitignored
// per-deployment overrides — see cmd/miranda-yazio/main.go, which globs
// config/*.yaml and passes every match to Load.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// validUserNameRe restricts yazio.users[].name to characters that are safe
// to use verbatim as a token-cache filename component (see
// internal/yazio.TokenCachePathForUser, "token-<name>.json") — without
// this, a name containing "/" or ".." could write the cache outside the
// intended token-cache directory.
var validUserNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

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
	// Users lists every YAZIO account this service can act on behalf of.
	// Every MCP tool takes a required `user` parameter matched against
	// YazioUser.Name here — this service is not single-tenant: each
	// household member gets their own entry (and their own env vars
	// holding their YAZIO login), not a separate deployment.
	Users []YazioUser `yaml:"users"`

	// TokenCacheDir is the directory holding one cached access/refresh
	// token file per user (named token-<name>.json, mode 0600). Empty
	// means "compute the default at startup":
	// $XDG_CONFIG_HOME/yazio-mcp/, falling back to ~/.config/yazio-mcp/.
	// Left empty here rather than resolved via os.UserHomeDir() in
	// Default() so config stays independent of whatever $HOME happens to
	// be when the config package is imported (e.g. under `go test`) —
	// internal/yazio resolves the empty case.
	TokenCacheDir string `yaml:"token_cache_dir"`

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

// YazioUser is one YAZIO account this service can act on behalf of.
// UsernameEnv/PasswordEnv name the environment variables holding that
// account's login credentials for the password OAuth grant — the values
// themselves are never stored here, see the top-level Conventions on
// secrets. Name is the key MCP tool callers pass as `user` to select this
// account, and must be unique among the configured users.
type YazioUser struct {
	Name        string `yaml:"name"`
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`
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
			Users: []YazioUser{
				{Name: "archer", UsernameEnv: "YAZIO_USERNAME_ARCHER", PasswordEnv: "YAZIO_PASSWORD_ARCHER"},
			},
			TokenCacheDir:         "",
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

// Load reads each YAML file in paths and merges it over Default(), applying
// them in order — a later file's fields win over an earlier file's for
// anything both set. A missing file is skipped, not an error, so a
// deployment only needs to provide the config/*.yaml files that actually
// override something. yaml.Unmarshal only overwrites fields present in the
// document, so unmarshaling repeatedly into the same struct gives a cheap
// partial merge without a separate deep-merge step — note this means a
// slice field (e.g. yazio.users, yazio.default_locales) is replaced whole
// by whichever file last sets it, not appended to, so a given slice should
// live in exactly one file.
func Load(paths ...string) (Config, error) {
	cfg := Default()

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}

		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	// A hand-edited YAML scalar can carry stray leading/trailing
	// whitespace (e.g. a quoted "archer " value). Every MCP tool caller's
	// "user" input is trimmed before the account lookup (see
	// mcpserver.resolveClient), so an untrimmed name here would make that
	// account permanently unreachable — normalize before validate() so
	// the trimmed form is also what duplicate-detection compares.
	for i := range cfg.Yazio.Users {
		cfg.Yazio.Users[i].Name = strings.TrimSpace(cfg.Yazio.Users[i].Name)
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
	if len(c.Yazio.Users) == 0 {
		return fmt.Errorf("config: yazio.users must not be empty")
	}
	seenNames := make(map[string]bool, len(c.Yazio.Users))
	seenUsernameEnvs := make(map[string]bool, len(c.Yazio.Users))
	seenPasswordEnvs := make(map[string]bool, len(c.Yazio.Users))
	for i, u := range c.Yazio.Users {
		if u.Name == "" {
			return fmt.Errorf("config: yazio.users[%d].name must not be empty", i)
		}
		if !validUserNameRe.MatchString(u.Name) {
			return fmt.Errorf("config: yazio.users[%d].name %q must contain only letters, digits, underscore, or hyphen — it's used as a token-cache filename", i, u.Name)
		}
		if seenNames[u.Name] {
			return fmt.Errorf("config: yazio.users[%d].name %q is a duplicate — user names must be unique", i, u.Name)
		}
		seenNames[u.Name] = true
		if u.UsernameEnv == "" {
			return fmt.Errorf("config: yazio.users[%d] (%q).username_env must not be empty", i, u.Name)
		}
		if seenUsernameEnvs[u.UsernameEnv] {
			return fmt.Errorf("config: yazio.users[%d] (%q).username_env %q is already used by another configured user — each user must read a distinct env var", i, u.Name, u.UsernameEnv)
		}
		seenUsernameEnvs[u.UsernameEnv] = true
		if u.PasswordEnv == "" {
			return fmt.Errorf("config: yazio.users[%d] (%q).password_env must not be empty", i, u.Name)
		}
		if seenPasswordEnvs[u.PasswordEnv] {
			return fmt.Errorf("config: yazio.users[%d] (%q).password_env %q is already used by another configured user — each user must read a distinct env var", i, u.Name, u.PasswordEnv)
		}
		seenPasswordEnvs[u.PasswordEnv] = true
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
