package yazio

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dateLayout and dateTimeLayout match the formats YAZIO's diary endpoints
// expect — a bare date for querying, a full timestamp for logging an
// entry.
const (
	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02 15:04:05"
)

// GetConsumedItems returns the diary entries logged for the given date
// (only the day portion of date is used).
func (c *Client) GetConsumedItems(ctx context.Context, date time.Time) ([]ConsumedItem, error) {
	q := url.Values{"date": {date.Format(dateLayout)}}

	body, err := c.do(ctx, http.MethodGet, "/user/consumed-items", q, nil)
	if err != nil {
		return nil, err
	}

	// Despite yazio_public_api's swagger doc describing this response as a
	// bare array, the live API returns an object with "products" (plus
	// "recipe_portions"/"simple_products", which this client doesn't
	// support logging — see AddConsumedItem). Confirmed against the real
	// API rather than trusted from the doc, since it's unofficial.
	var resp struct {
		Products []ConsumedItem `json:"products"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("yazio: GetConsumedItems: decode response (API may have changed shape): %w", err)
	}
	items := resp.Products
	return items, nil
}

// AddConsumedItem logs amount grams (or milliliters, for a liquid
// product) of productID against mealType ("breakfast"/"lunch"/"dinner"/
// "snack") on date. The caller is responsible for converting household
// quantities ("2 pieces", "1 cup") into grams first — GetProduct's
// Servings list gives the per-unit weight needed to do that
// (amount = serving.Amount * quantity).
//
// YAZIO does not validate productID against its product database: an
// unknown ID is silently discarded rather than rejected, so callers
// should only pass IDs returned by SearchProducts or GetProduct.
func (c *Client) AddConsumedItem(ctx context.Context, productID string, amount float64, mealType string, date time.Time) error {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return fmt.Errorf("yazio: AddConsumedItem: productID must not be empty")
	}
	if amount <= 0 {
		return fmt.Errorf("yazio: AddConsumedItem: amount must be positive, got %v", amount)
	}
	daytime, err := normalizeMealType(mealType)
	if err != nil {
		return err
	}

	itemID, err := newItemID()
	if err != nil {
		return fmt.Errorf("yazio: AddConsumedItem: %w", err)
	}

	reqBody := map[string]any{
		"products": []map[string]any{
			{
				"id":         itemID,
				"product_id": productID,
				"date":       date.Format(dateTimeLayout),
				"daytime":    daytime,
				"amount":     amount,
			},
		},
		// YAZIO's endpoint accepts logging recipes and "simple" (unbranded
		// quantity-only) products in the same call; we only ever log a
		// known product, but the API requires these keys to be present.
		"recipe_portions": []any{},
		"simple_products": []any{},
	}

	_, err = c.do(ctx, http.MethodPost, "/user/consumed-items", nil, reqBody)
	return err
}

// RemoveConsumedItem deletes one diary entry by its item ID (the ID
// returned in ConsumedItem, not a product ID).
func (c *Client) RemoveConsumedItem(ctx context.Context, itemID string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("yazio: RemoveConsumedItem: itemID must not be empty")
	}

	_, err := c.do(ctx, http.MethodDelete, "/user/consumed-items", nil, []string{itemID})
	return err
}

func normalizeMealType(mealType string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(mealType))
	switch normalized {
	case MealBreakfast, MealLunch, MealDinner, MealSnack:
		return normalized, nil
	default:
		return "", fmt.Errorf("yazio: mealType must be one of breakfast|lunch|dinner|snack, got %q", mealType)
	}
}

// newItemID generates a random UUIDv4 for the "id" field YAZIO requires
// on every logged diary entry (it identifies the entry itself, distinct
// from the product being logged).
func newItemID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate item id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
