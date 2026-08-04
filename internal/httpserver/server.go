// Package httpserver wires the service's HTTP handler behind a single mux
// with bearer-token auth and an unauthenticated /healthz endpoint. It is
// transport-agnostic about what mcpHandler actually is — callers pass in
// whatever handler serves MCP over Streamable HTTP.
package httpserver

import (
	"net/http"
)

// New builds the top-level HTTP handler:
//   - GET /healthz — unauthenticated liveness check.
//   - /mcp        — the MCP endpoint, bearer-auth-gated.
//
// token is the shared secret that MCP clients must present.
func New(mcpHandler http.Handler, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", requireBearerToken(token, mcpHandler))
	return mux
}
