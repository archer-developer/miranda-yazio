package yazio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

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
// every serving type it supports and its per-gram nutrients.
func (c *Client) GetProduct(ctx context.Context, id string) (*Product, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("yazio: GetProduct: id must not be empty")
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
	return &p, nil
}
