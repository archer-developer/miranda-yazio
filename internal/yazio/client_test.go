package yazio

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a minimal stand-in for yzapi.yazio.com: it answers
// /oauth/token itself (so tests can assert on grant_type/credentials) and
// delegates every other path to a per-test handler.
type fakeAPI struct {
	oauthCalls   atomic.Int32
	oauthHandler http.HandlerFunc
	apiHandler   http.HandlerFunc
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth/token" {
		f.oauthCalls.Add(1)
		f.oauthHandler(w, r)
		return
	}
	f.apiHandler(w, r)
}

// tokenReply writes a valid AuthResponse-shaped body for grant_type=password
// or grant_type=refresh_token requests. accessToken lets a test tell two
// successive logins/refreshes apart by inspecting the token later used in
// an Authorization header.
func tokenReply(accessToken, refreshToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"token_type":    "bearer",
			"expires_in":    172800,
		})
	}
}

func newTestClient(t *testing.T, api *fakeAPI, mutate ...func(*Options)) *Client {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	opts := Options{
		Username:       "alexander",
		Password:       "hunter2",
		TokenCachePath: filepath.Join(t.TempDir(), "token.json"),
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
		Logger:         slog.New(slog.DiscardHandler),
	}
	for _, m := range mutate {
		m(&opts)
	}

	client, err := New(opts)
	require.NoError(t, err)
	return client
}

func TestClient_SearchProducts_LoginsAndSendsExpectedQuery(t *testing.T) {
	var gotQuery url.Values
	var gotAuth string

	api := &fakeAPI{
		oauthHandler: func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			assert.Equal(t, "password", r.FormValue("grant_type"))
			assert.Equal(t, "alexander", r.FormValue("username"))
			assert.Equal(t, "hunter2", r.FormValue("password"))
			assert.NotEmpty(t, r.FormValue("client_id"))
			tokenReply("access-1", "refresh-1")(w, r)
		},
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"product_id":"p1","name":"Chicken soup","producer":"Acme","serving":"portion","serving_quantity":1,"amount":350,"base_unit":"g","is_verified":true,"nutrients":{"energy.energy":0.4,"nutrient.protein":0.05,"nutrient.fat":0.02,"nutrient.carb":0.03}}]`))
		},
	}

	client := newTestClient(t, api, func(o *Options) {
		o.DefaultCountry = "BY"
		o.DefaultLocales = []string{"by_BY", "ru_RU", "en_EN"}
		o.DefaultSex = "male"
	})

	results, err := client.SearchProducts(t.Context(), "chicken soup")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "p1", results[0].ID)
	assert.Equal(t, "Chicken soup", results[0].Name)
	assert.InDelta(t, 0.4, results[0].Nutrients.EnergyKcalPerGram(), 0.0001)

	assert.Equal(t, "chicken soup", gotQuery.Get("query"))
	assert.Equal(t, "BY", gotQuery.Get("countries"))
	assert.Equal(t, "by_BY,ru_RU,en_EN", gotQuery.Get("locales"), "locale priority list is joined with commas on the wire")
	assert.Equal(t, "male", gotQuery.Get("sex"))
	assert.Equal(t, "Bearer access-1", gotAuth)
	assert.Equal(t, int32(1), api.oauthCalls.Load())
}

func TestClient_SearchProducts_RejectsEmptyQuery(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: failIfCalled(t),
		apiHandler:   failIfCalled(t),
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "   ")
	assert.Error(t, err)
}

func TestClient_TokenIsCachedInMemoryAcrossCalls(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "a")
	require.NoError(t, err)
	_, err = client.SearchProducts(t.Context(), "b")
	require.NoError(t, err)

	assert.Equal(t, int32(1), api.oauthCalls.Load(), "second call should reuse the cached token, not log in again")
}

func TestClient_TokenPersistsAcrossClientInstances(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`)) //nolint:errcheck
		},
	}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cachePath := filepath.Join(t.TempDir(), "token.json")

	clientA, err := New(Options{Username: "alexander", Password: "hunter2", TokenCachePath: cachePath, BaseURL: srv.URL, HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	_, err = clientA.SearchProducts(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, int32(1), api.oauthCalls.Load())

	// A second, independent Client pointed at the same cache file should
	// load the token from disk instead of logging in again.
	clientB, err := New(Options{Username: "alexander", Password: "hunter2", TokenCachePath: cachePath, BaseURL: srv.URL, HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	_, err = clientB.SearchProducts(t.Context(), "b")
	require.NoError(t, err)

	assert.Equal(t, int32(1), api.oauthCalls.Load(), "second client should reuse the persisted token")
}

func TestClient_RefreshOnUnauthorized_RetriesOnceWithNewToken(t *testing.T) {
	var apiCalls atomic.Int32

	api := &fakeAPI{
		oauthHandler: func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			switch r.FormValue("grant_type") {
			case "password":
				tokenReply("access-1", "refresh-1")(w, r)
			case "refresh_token":
				assert.Equal(t, "refresh-1", r.FormValue("refresh_token"))
				tokenReply("access-2", "refresh-2")(w, r)
			default:
				t.Fatalf("unexpected grant_type %q", r.FormValue("grant_type"))
			}
		},
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			n := apiCalls.Add(1)
			if n == 1 {
				assert.Equal(t, "Bearer access-1", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			assert.Equal(t, "Bearer access-2", r.Header.Get("Authorization"), "retry should use the refreshed token")
			w.Write([]byte(`[]`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "a")
	require.NoError(t, err)
	assert.Equal(t, int32(2), apiCalls.Load())
	assert.Equal(t, int32(2), api.oauthCalls.Load(), "one login plus one refresh")
}

func TestClient_PersistentUnauthorized_ReturnsErrUnauthorized(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "a")
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestClient_RateLimited(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "a")
	assert.ErrorIs(t, err, ErrRateLimited)
}

func TestClient_ServiceUnavailable(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "a")
	assert.ErrorIs(t, err, ErrServiceUnavailable)
}

func TestClient_InvalidCredentials(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		},
		apiHandler: failIfCalled(t),
	}
	client := newTestClient(t, api)

	_, err := client.SearchProducts(t.Context(), "a")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestClient_GetProduct_NotFound(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	}
	client := newTestClient(t, api)

	_, err := client.GetProduct(t.Context(), "missing-id")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClient_GetProduct_FillsIDFromRequestAndParsesServings(t *testing.T) {
	var gotPath string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			// Real YAZIO product-detail responses don't echo the id back.
			w.Write([]byte(`{"name":"Cutlet","producer":"Acme","category":"meat","base_unit":"g","nutrients":{"energy.energy":2.5},"servings":[{"serving":"piece","amount":70},{"serving":"gram","amount":1}]}`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	p, err := client.GetProduct(t.Context(), "cutlet-id")
	require.NoError(t, err)
	assert.Equal(t, "/products/cutlet-id", gotPath)
	assert.Equal(t, "cutlet-id", p.ID)
	require.Len(t, p.Servings, 2)
	assert.Equal(t, Serving{Type: "piece", Amount: 70}, p.Servings[0])
}

func failIfCalled(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request to %s %s", r.Method, r.URL.Path)
	}
}
