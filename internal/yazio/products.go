package yazio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// productCache holds every Product this account's Client has already
// fetched via GetProduct, keyed by product_id, so a diary walk across many
// days/tools (get_daily_summary, get_consumed_items_range, create_recipe)
// doesn't refetch the same handful of everyday foods over and over. It is
// unbounded and never invalidated: YAZIO's per-product nutrition facts are
// static reference data that doesn't change once published, and a single
// household's set of distinct logged foods stays small (tens to low
// hundreds over a service's lifetime), so unbounded growth isn't a real
// memory concern at this scale. It's in-memory only and doesn't survive a
// restart, unlike the token cache (token.go) — that's fine here, since
// this is purely a repeat-request optimization, not something correctness
// depends on. Scoped per Client (i.e. per configured user) rather than
// shared globally, since each user already has an independent Client
// instance and there's no shared state between them today.
type productCache struct {
	mu   sync.Mutex
	byID map[string]*Product
}

func newProductCache() *productCache {
	return &productCache{byID: make(map[string]*Product)}
}

// get returns the cached product for id, if any. The returned pointer is
// shared across every caller that has ever fetched this product — callers
// must treat it as read-only.
func (c *productCache) get(id string) (*Product, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.byID[id]
	return p, ok
}

// set stores p under id. Only called after a successful fetch — a failed
// lookup (e.g. ErrNotFound) is never cached, so a transient error doesn't
// poison future calls for the same id.
func (c *productCache) set(id string, p *Product) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[id] = p
}

// SearchProducts searches YAZIO's product database for query, scoped to
// the client's configured country/locale/sex (see Options). Results are
// ranked by YAZIO's own relevance score and include each product's
// default suggested serving — call GetProduct for the full list of
// serving types a product supports.
func (c *Client) SearchProducts(ctx context.Context, query string) ([]Product, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("yazio: SearchProducts: query must not be empty")
	}

	q := url.Values{
		"query":     {query},
		"countries": {c.country},
		"locales":   {c.locales},
		"sex":       {c.sex},
	}

	body, err := c.do(ctx, http.MethodGet, "/products/search", q, nil)
	if err != nil {
		return nil, err
	}

	var results []Product
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("yazio: SearchProducts: decode response (API may have changed shape): %w", err)
	}
	return results, nil
}

// GetProduct fetches the full detail for a single product, including
// every serving type it supports and its per-gram nutrients. Results are
// cached in memory by product_id for the lifetime of the Client (see
// productCache) — a repeated id is served from cache without hitting
// YAZIO again.
func (c *Client) GetProduct(ctx context.Context, id string) (*Product, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("yazio: GetProduct: id must not be empty")
	}

	if p, ok := c.products.get(id); ok {
		return p, nil
	}

	body, err := c.do(ctx, http.MethodGet, "/products/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return nil, err
	}

	var p Product
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("yazio: GetProduct: decode response (API may have changed shape): %w", err)
	}
	// The detail endpoint doesn't echo the ID back in its response body —
	// fill it in from what was requested, same as every other open-source
	// YAZIO client does.
	p.ID = id

	c.products.set(id, &p)
	return &p, nil
}
