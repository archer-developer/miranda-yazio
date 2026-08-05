package yazio

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_GetConsumedItems_SendsDateAndParsesResponse(t *testing.T) {
	var gotDate string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			gotDate = r.URL.Query().Get("date")
			// Real YAZIO responses wrap the list in an object with
			// "products" (plus recipe_portions/simple_products), not a
			// bare array — confirmed against the live API, which
			// disagrees with yazio_public_api's swagger doc here.
			w.Write([]byte(`{"products":[{"id":"i1","product_id":"p1","date":"2024-01-15 08:30:00","daytime":"lunch","amount":140,"serving":"piece","serving_quantity":2}],"recipe_portions":[{"id":"r1","recipe_id":"rec1","date":"2024-01-15 12:00:00","daytime":"lunch","portion_count":1.5}],"simple_products":[]}`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	products, portions, err := client.GetConsumedItems(t.Context(), time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, "2024-01-15", gotDate)
	require.Len(t, products, 1)
	assert.Equal(t, ConsumedItem{ID: "i1", ProductID: "p1", Date: "2024-01-15 08:30:00", Daytime: "lunch", Amount: 140, Serving: "piece", ServingQuantity: 2}, products[0])
	require.Len(t, portions, 1)
	assert.Equal(t, ConsumedRecipePortion{ID: "r1", RecipeID: "rec1", Date: "2024-01-15 12:00:00", Daytime: "lunch", PortionCount: 1.5}, portions[0])
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestClient_AddConsumedItem_SendsExpectedBody(t *testing.T) {
	type addReqBody struct {
		Products []struct {
			ID        string  `json:"id"`
			ProductID string  `json:"product_id"`
			Date      string  `json:"date"`
			Daytime   string  `json:"daytime"`
			Amount    float64 `json:"amount"`
		} `json:"products"`
		RecipePortions []any `json:"recipe_portions"`
		SimpleProducts []any `json:"simple_products"`
	}

	var got addReqBody
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.WriteHeader(http.StatusOK)
		},
	}
	client := newTestClient(t, api)

	date := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
	err := client.AddConsumedItem(t.Context(), "cutlet-id", 140, "Lunch", date)
	require.NoError(t, err)

	require.Len(t, got.Products, 1)
	assert.Regexp(t, uuidPattern, got.Products[0].ID)
	assert.Equal(t, "cutlet-id", got.Products[0].ProductID)
	assert.Equal(t, "2024-01-15 13:00:00", got.Products[0].Date)
	assert.Equal(t, "lunch", got.Products[0].Daytime, "meal type is normalized to lowercase")
	assert.Equal(t, 140.0, got.Products[0].Amount)
	assert.NotNil(t, got.RecipePortions)
	assert.NotNil(t, got.SimpleProducts)
}

func TestClient_AddConsumedItem_RejectsInvalidMealType(t *testing.T) {
	api := &fakeAPI{oauthHandler: failIfCalled(t), apiHandler: failIfCalled(t)}
	client := newTestClient(t, api)

	err := client.AddConsumedItem(t.Context(), "p1", 100, "brunch", time.Now())
	assert.Error(t, err)
}

func TestClient_AddConsumedItem_RejectsNonPositiveAmount(t *testing.T) {
	api := &fakeAPI{oauthHandler: failIfCalled(t), apiHandler: failIfCalled(t)}
	client := newTestClient(t, api)

	err := client.AddConsumedItem(t.Context(), "p1", 0, MealLunch, time.Now())
	assert.Error(t, err)
}

func TestClient_RemoveConsumedItem_SendsIDArray(t *testing.T) {
	var gotBody []string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			w.WriteHeader(http.StatusOK)
		},
	}
	client := newTestClient(t, api)

	err := client.RemoveConsumedItem(t.Context(), "entry-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"entry-1"}, gotBody)
}

func TestClient_AddConsumedRecipePortion_SendsExpectedBody(t *testing.T) {
	type recipePortionEntry struct {
		ID           string  `json:"id"`
		RecipeID     string  `json:"recipe_id"`
		PortionCount float64 `json:"portion_count"`
		Daytime      string  `json:"daytime"`
		Date         string  `json:"date"`
	}
	type reqBody struct {
		Products       []any                `json:"products"`
		RecipePortions []recipePortionEntry `json:"recipe_portions"`
		SimpleProducts []any                `json:"simple_products"`
	}

	var got reqBody
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.WriteHeader(http.StatusNoContent)
		},
	}
	client := newTestClient(t, api)

	date := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
	err := client.AddConsumedRecipePortion(t.Context(), "rec-uuid-1", 1.5, "lunch", date)
	require.NoError(t, err)

	assert.Empty(t, got.Products)
	assert.Empty(t, got.SimpleProducts)
	require.Len(t, got.RecipePortions, 1)
	assert.Regexp(t, uuidPattern, got.RecipePortions[0].ID, "entry ID must be a UUIDv4")
	assert.Equal(t, "rec-uuid-1", got.RecipePortions[0].RecipeID)
	assert.Equal(t, 1.5, got.RecipePortions[0].PortionCount)
	assert.Equal(t, "lunch", got.RecipePortions[0].Daytime)
	assert.Equal(t, "2024-01-15 13:00:00", got.RecipePortions[0].Date)
}

func TestClient_AddConsumedRecipePortion_RejectsInvalidInputs(t *testing.T) {
	api := &fakeAPI{oauthHandler: failIfCalled(t), apiHandler: failIfCalled(t)}
	client := newTestClient(t, api)

	assert.Error(t, client.AddConsumedRecipePortion(t.Context(), "", 1, MealLunch, time.Now()))
	assert.Error(t, client.AddConsumedRecipePortion(t.Context(), "rec1", 0, MealLunch, time.Now()))
	assert.Error(t, client.AddConsumedRecipePortion(t.Context(), "rec1", 1, "brunch", time.Now()))
}

func TestClient_RemoveConsumedRecipePortion_SendsObjectBody(t *testing.T) {
	var gotBody map[string]string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			w.WriteHeader(http.StatusNoContent)
		},
	}
	client := newTestClient(t, api)

	err := client.RemoveConsumedRecipePortion(t.Context(), "entry-rp-1")
	require.NoError(t, err)
	// Must be {"recipe_portions": "<id>"}, not a bare array — YAZIO uses
	// different delete body shapes for products vs recipe portions.
	assert.Equal(t, map[string]string{"recipe_portions": "entry-rp-1"}, gotBody)
}

func TestNormalizeMealType(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"breakfast", "breakfast", false},
		{"LUNCH", "lunch", false},
		{"  dinner  ", "dinner", false},
		{"Snack", "snack", false},
		{"brunch", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := normalizeMealType(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
