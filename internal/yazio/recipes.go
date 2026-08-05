package yazio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ListRecipes returns the IDs of all recipes the authenticated user owns.
// Each ID can be passed to GetRecipe to load the full recipe detail.
//
// Note: YAZIO's recipe endpoints were reverse-engineered at v22
// (https://yzapi.yazio.com/v22). If this client is configured with a v15
// base URL and the endpoint returns 404, bump defaultBaseURL to /v22.
func (c *Client) ListRecipes(ctx context.Context) ([]string, error) {
	body, err := c.do(ctx, http.MethodGet, "/user/recipes", nil, nil)
	if err != nil {
		return nil, err
	}

	var ids []string
	if err := json.Unmarshal(body, &ids); err != nil {
		return nil, fmt.Errorf("yazio: ListRecipes: decode response (API may have changed shape): %w", err)
	}
	return ids, nil
}

// GetRecipe fetches the full detail for one recipe by ID, including its
// ingredient list and total nutrient values.
//
// The recipe_id from ListRecipes is what YAZIO needs here; the response
// does not echo the ID back, so it is filled in from the request.
func (c *Client) GetRecipe(ctx context.Context, id string) (*Recipe, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("yazio: GetRecipe: id must not be empty")
	}

	// /recipes/{id} (not /user/recipes/{id}) serves both user-created and
	// editorial recipes — confirmed against the community API spec.
	body, err := c.do(ctx, http.MethodGet, "/recipes/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return nil, err
	}

	// Raw response shape — a superset of the RecipeDraft we send on create.
	var dto struct {
		Name          string            `json:"name"`
		PortionCount  float64           `json:"portion_count"`
		Nutrients     Nutrients         `json:"nutrients"`
		Servings      []recipeServing   `json:"servings"`
		Instructions  []json.RawMessage `json:"instructions"` // API may send strings or objects
		IsYazioRecipe bool              `json:"is_yazio_recipe"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return nil, fmt.Errorf("yazio: GetRecipe: decode response (API may have changed shape): %w", err)
	}

	recipe := &Recipe{
		ID:            id,
		Name:          dto.Name,
		PortionCount:  dto.PortionCount,
		Nutrients:     dto.Nutrients,
		IsYazioRecipe: dto.IsYazioRecipe,
	}

	// Parse instructions: the live API may return strings or null items; keep
	// only valid non-empty strings.
	for _, raw := range dto.Instructions {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			recipe.Instructions = append(recipe.Instructions, s)
		}
	}

	if len(dto.Servings) > 0 {
		recipe.Servings = make([]RecipeServing, len(dto.Servings))
		for i, s := range dto.Servings {
			recipe.Servings[i] = RecipeServing{
				ProductID: s.ProductID,
				Name:      s.Name,
				Producer:  s.Producer,
				BaseUnit:  s.BaseUnit,
				Amount:    s.Amount,
			}
		}
	}

	return recipe, nil
}

// recipeServing is the wire shape of one ingredient in a GET /recipes/{id}
// response — a superset of what we send in RecipeDraft.servings.
type recipeServing struct {
	ProductID string  `json:"product_id"`
	Name      string  `json:"name"`
	Producer  string  `json:"producer"`
	BaseUnit  string  `json:"base_unit"`
	Amount    float64 `json:"amount"`
}

// CreateRecipe creates a new recipe under the authenticated user's account
// and returns the client-generated recipe ID.
//
// ingredients must contain at least two items — YAZIO rejects single-
// ingredient recipes with a 400. portionCount must be at least 1.
//
// YAZIO does not compute nutrients from the ingredient list: the client is
// responsible for scaling each ingredient's per-gram nutrients (from
// GetProduct) by its amount and summing them into totalNutrients before
// calling CreateRecipe. The values are stored verbatim; passing totals that
// don't match the ingredients results in silently wrong nutritional data.
func (c *Client) CreateRecipe(ctx context.Context, name string, portionCount int, ingredients []RecipeIngredient, totalNutrients Nutrients, instructions []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("yazio: CreateRecipe: name must not be empty")
	}
	if len(ingredients) < 2 {
		return "", fmt.Errorf("yazio: CreateRecipe: at least 2 ingredients required (YAZIO rejects single-ingredient recipes), got %d", len(ingredients))
	}
	if portionCount <= 0 {
		return "", fmt.Errorf("yazio: CreateRecipe: portionCount must be at least 1, got %d", portionCount)
	}
	if instructions == nil {
		instructions = []string{}
	}

	recipeID, err := newItemID()
	if err != nil {
		return "", fmt.Errorf("yazio: CreateRecipe: %w", err)
	}

	servings := make([]map[string]any, len(ingredients))
	for i, ing := range ingredients {
		servings[i] = map[string]any{
			"product_id": ing.ProductID,
			"name":       ing.Name,
			"producer":   ing.Producer,
			"base_unit":  ing.BaseUnit,
			"amount":     ing.Amount,
		}
	}

	reqBody := map[string]any{
		"id":   recipeID,
		"name": name,
		// portion_count must serialize as an integer (not 2.0 or 2.5) —
		// YAZIO answers 500 to a fractional value. Using Go int guarantees
		// json.Marshal produces "2", not "2.0".
		"portion_count": portionCount,
		"instructions":  instructions,
		"servings":      servings,
		"nutrients":     totalNutrients,
	}

	_, err = c.do(ctx, http.MethodPost, "/user/recipes", nil, reqBody)
	if err != nil {
		return "", err
	}
	return recipeID, nil
}

// DeleteRecipe deletes one of the authenticated user's recipes. Existing
// diary entries that logged portions of this recipe are not affected — use
// RemoveConsumedRecipePortion to remove those.
func (c *Client) DeleteRecipe(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("yazio: DeleteRecipe: id must not be empty")
	}

	_, err := c.do(ctx, http.MethodDelete, "/user/recipes/"+url.PathEscape(id), nil, nil)
	return err
}
