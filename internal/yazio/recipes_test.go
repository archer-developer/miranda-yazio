package yazio

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)


func TestClient_ListRecipes_ParsesIDArray(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/user/recipes", r.URL.Path)
			w.Write([]byte(`["ID-A","ID-B","ID-C"]`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	ids, err := client.ListRecipes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"ID-A", "ID-B", "ID-C"}, ids)
}

func TestClient_ListRecipes_ReturnsEmptySliceWhenNoRecipes(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	ids, err := client.ListRecipes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestClient_GetRecipe_ParsesResponse(t *testing.T) {
	var gotPath string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			// The live API does not echo the id back in the body.
			w.Write([]byte(`{"name":"Borscht","portion_count":4,"nutrients":{"energy.energy":320,"nutrient.protein":25},"servings":[{"product_id":"p1","name":"Chicken","producer":"Acme","base_unit":"g","amount":400},{"product_id":"p2","name":"Beet","producer":"","base_unit":"g","amount":200}],"instructions":["Boil chicken","Add beet"],"is_yazio_recipe":false}`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	r, err := client.GetRecipe(t.Context(), "recipe-uuid")
	require.NoError(t, err)
	assert.Equal(t, "/recipes/recipe-uuid", gotPath)
	assert.Equal(t, "recipe-uuid", r.ID, "ID is filled in from the request, not the response")
	assert.Equal(t, "Borscht", r.Name)
	assert.InDelta(t, 4.0, r.PortionCount, 0.001)
	assert.InDelta(t, 320.0, r.Nutrients[NutrientEnergyKcal], 0.001)
	require.Len(t, r.Servings, 2)
	assert.Equal(t, RecipeServing{ProductID: "p1", Name: "Chicken", Producer: "Acme", BaseUnit: "g", Amount: 400}, r.Servings[0])
	assert.Equal(t, []string{"Boil chicken", "Add beet"}, r.Instructions)
	assert.False(t, r.IsYazioRecipe)
}

func TestClient_GetRecipe_RejectsEmptyID(t *testing.T) {
	api := &fakeAPI{oauthHandler: failIfCalled(t), apiHandler: failIfCalled(t)}
	client := newTestClient(t, api)

	_, err := client.GetRecipe(t.Context(), "  ")
	assert.Error(t, err)
}

func TestClient_GetRecipe_NotFound(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	}
	client := newTestClient(t, api)

	_, err := client.GetRecipe(t.Context(), "missing-id")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClient_CreateRecipe_SendsExpectedBody(t *testing.T) {
	type reqBody struct {
		ID           string         `json:"id"`
		Name         string         `json:"name"`
		PortionCount int            `json:"portion_count"`
		Instructions []string       `json:"instructions"`
		Servings     []map[string]any `json:"servings"`
		Nutrients    map[string]float64 `json:"nutrients"`
	}

	var got reqBody
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/user/recipes", r.URL.Path)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.WriteHeader(http.StatusOK)
		},
	}
	client := newTestClient(t, api)

	ingredients := []RecipeIngredient{
		{ProductID: "p1", Name: "Chicken", Producer: "Acme", BaseUnit: "g", Amount: 400},
		{ProductID: "p2", Name: "Beet", Producer: "", BaseUnit: "g", Amount: 200},
	}
	totals := Nutrients{
		NutrientEnergyKcal: 380,
		NutrientProtein:    40,
		NutrientFat:        8,
		NutrientCarb:       12,
	}

	recipeID, err := client.CreateRecipe(t.Context(), "Borscht", 4, ingredients, totals, []string{"Boil", "Serve"})
	require.NoError(t, err)

	assert.Regexp(t, uuidPattern, recipeID)
	assert.Equal(t, recipeID, got.ID)
	assert.Equal(t, "Borscht", got.Name)
	assert.Equal(t, 4, got.PortionCount, "portion_count must be an integer, not a float")
	assert.Equal(t, []string{"Boil", "Serve"}, got.Instructions)
	require.Len(t, got.Servings, 2)
	assert.Equal(t, "p1", got.Servings[0]["product_id"])
	assert.Equal(t, "Chicken", got.Servings[0]["name"])
	assert.InDelta(t, 380.0, got.Nutrients[NutrientEnergyKcal], 0.001)
}

func TestClient_CreateRecipe_RejectsInvalidInputs(t *testing.T) {
	api := &fakeAPI{oauthHandler: failIfCalled(t), apiHandler: failIfCalled(t)}
	client := newTestClient(t, api)
	twoIngredients := []RecipeIngredient{
		{ProductID: "p1", Name: "a", BaseUnit: "g", Amount: 100},
		{ProductID: "p2", Name: "b", BaseUnit: "g", Amount: 100},
	}

	_, err := client.CreateRecipe(t.Context(), "", 1, twoIngredients, nil, nil)
	assert.Error(t, err, "empty name")

	_, err = client.CreateRecipe(t.Context(), "ok", 1, twoIngredients[:1], nil, nil)
	assert.Error(t, err, "only one ingredient")

	_, err = client.CreateRecipe(t.Context(), "ok", 0, twoIngredients, nil, nil)
	assert.Error(t, err, "zero portionCount")
}

func TestClient_CreateRecipe_NilInstructionsBecomesEmptySlice(t *testing.T) {
	type reqBody struct {
		Instructions []string `json:"instructions"`
	}
	var got reqBody
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.WriteHeader(http.StatusOK)
		},
	}
	client := newTestClient(t, api)

	twoIngredients := []RecipeIngredient{
		{ProductID: "p1", Name: "a", BaseUnit: "g", Amount: 100},
		{ProductID: "p2", Name: "b", BaseUnit: "g", Amount: 100},
	}
	_, err := client.CreateRecipe(t.Context(), "name", 1, twoIngredients, nil, nil)
	require.NoError(t, err)
	assert.NotNil(t, got.Instructions, "YAZIO requires the field to be present, not null")
}

func TestClient_DeleteRecipe_SendsDeleteRequest(t *testing.T) {
	var gotMethod, gotPath string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		},
	}
	client := newTestClient(t, api)

	err := client.DeleteRecipe(t.Context(), "recipe-uuid")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/user/recipes/recipe-uuid", gotPath)
}

func TestClient_DeleteRecipe_RejectsEmptyID(t *testing.T) {
	api := &fakeAPI{oauthHandler: failIfCalled(t), apiHandler: failIfCalled(t)}
	client := newTestClient(t, api)

	assert.Error(t, client.DeleteRecipe(t.Context(), "  "))
}
