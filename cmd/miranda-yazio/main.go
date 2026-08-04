// Command miranda-yazio is an MCP server that lets Miranda read and write
// a YAZIO food diary on behalf of one account. YAZIO has no official
// public API — internal/yazio talks to the same unofficial v15 REST API
// the YAZIO mobile app uses.
//
// Bootstrap: envfile.Load(.env) -> config.Load(config/config.yaml) -> build
// the real logger -> check required secrets are set (this service's own
// MCP bearer token, plus the YAZIO account's username/password) -> build
// the yazio.Client -> build the MCP server -> wrap it in a Streamable HTTP
// handler -> mount it behind bearer auth and /healthz -> serve until
// SIGINT/SIGTERM, then shut down gracefully.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-yazio/internal/config"
	"github.com/archer-developer/miranda-yazio/internal/envfile"
	"github.com/archer-developer/miranda-yazio/internal/httpserver"
	"github.com/archer-developer/miranda-yazio/internal/mcpserver"
	"github.com/archer-developer/miranda-yazio/internal/yazio"
)

const (
	dotEnvPath        = ".env"
	defaultConfigPath = "config/config.yaml"
	configPathEnv     = "YAZIO_MCP_CONFIG"
	shutdownTimeout   = 10 * time.Second
	debugLogDir       = "logs"
	debugLogFile      = "debug.log"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		logger.Warn("failed to load .env, continuing with process environment", "error", err)
	}

	cfgPath := defaultConfigPath
	if v := os.Getenv(configPathEnv); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	realLogger, closeLogger, err := buildLogger(cfg.Logging)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
	defer closeLogger()
	logger = realLogger

	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	token := os.Getenv(cfg.AuthTokenEnv)
	if token == "" {
		return fmt.Errorf("main: environment variable %s (named by auth_token_env) is not set — refusing to start with no auth token", cfg.AuthTokenEnv)
	}

	username := os.Getenv(cfg.Yazio.UsernameEnv)
	if username == "" {
		return fmt.Errorf("main: environment variable %s (named by yazio.username_env) is not set — YAZIO account username required", cfg.Yazio.UsernameEnv)
	}
	password := os.Getenv(cfg.Yazio.PasswordEnv)
	if password == "" {
		return fmt.Errorf("main: environment variable %s (named by yazio.password_env) is not set — YAZIO account password required", cfg.Yazio.PasswordEnv)
	}

	yazioClient, err := yazio.New(yazio.Options{
		Username:       username,
		Password:       password,
		TokenCachePath: cfg.Yazio.TokenCachePath,
		RequestTimeout: time.Duration(cfg.Yazio.RequestTimeoutSeconds) * time.Second,
		DefaultCountry: cfg.Yazio.DefaultCountry,
		DefaultLocales: cfg.Yazio.DefaultLocales,
		DefaultSex:     cfg.Yazio.DefaultSex,
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("main: build yazio client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := mcpserver.New(yazioClient, logger)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	handler := httpserver.New(mcpHandler, token)
	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: handler}

	logger.Info("service ready", "addr", cfg.HTTPAddr, "yazio_username_env", cfg.Yazio.UsernameEnv)

	return serveUntilInterrupted(ctx, httpServer, logger)
}

func serveUntilInterrupted(ctx context.Context, httpServer *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// buildLogger returns a logger that writes info-and-above to stdout (what
// systemd/journalctl sees) and, at debug level, routes debug-and-below
// records to logs/debug.log instead — keeping the journal readable while
// still capturing verbose per-request detail on disk when needed.
func buildLogger(cfg config.LoggingConfig) (*slog.Logger, func(), error) {
	noop := func() {}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, noop, fmt.Errorf("main: invalid logging.level %q: %w", cfg.Level, err)
	}

	stdoutLevel := level
	if stdoutLevel < slog.LevelInfo {
		stdoutLevel = slog.LevelInfo
	}
	stdoutHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: stdoutLevel})

	if level > slog.LevelDebug {
		return slog.New(stdoutHandler), noop, nil
	}

	if err := os.MkdirAll(debugLogDir, 0o755); err != nil {
		return nil, noop, fmt.Errorf("main: create debug log dir: %w", err)
	}
	debugPath := filepath.Join(debugLogDir, debugLogFile)
	debugWriter, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, noop, fmt.Errorf("main: open debug log %s: %w", debugPath, err)
	}
	debugHandler := slog.NewTextHandler(debugWriter, &slog.HandlerOptions{Level: slog.LevelDebug})

	handler := &levelSplitHandler{stdout: stdoutHandler, debugFile: debugHandler}
	return slog.New(handler), func() { _ = debugWriter.Close() }, nil
}

type levelSplitHandler struct {
	stdout    slog.Handler
	debugFile slog.Handler
}

func (h *levelSplitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.stdout.Enabled(ctx, level) || h.debugFile.Enabled(ctx, level)
}

func (h *levelSplitHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo {
		return h.debugFile.Handle(ctx, r)
	}
	return h.stdout.Handle(ctx, r)
}

func (h *levelSplitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithAttrs(attrs),
		debugFile: h.debugFile.WithAttrs(attrs),
	}
}

func (h *levelSplitHandler) WithGroup(name string) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithGroup(name),
		debugFile: h.debugFile.WithGroup(name),
	}
}
