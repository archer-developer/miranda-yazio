package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-yazio/internal/yazio"
)

// fakeYazioClient is a narrow hand-written fake implementing YazioClient,
// used instead of a mocking framework per project convention — it's
// simple enough that generated mocks would add nothing.
type fakeYazioClient struct {
	searchResults []yazio.Product
	searchErr     error
	gotQuery      string

	product    *yazio.Product
	productErr error
	gotID      string

	consumedItems  []yazio.ConsumedItem
	recipePortions []yazio.ConsumedRecipePortion
	consumedErr    error
	gotDate        time.Time

	addErr       error
	gotProductID string
	gotAmount    float64
	gotMealType  string
	gotAddDate   time.Time
	addCalled    bool
	removeErr    error
	gotRemoveID  string
	removeCalled bool

	// Recipe fields.
	recipeIDs        []string
	listRecipesErr   error
	recipe           *yazio.Recipe
	getRecipeErr     error
	gotRecipeID      string
	createdRecipeID  string
	createRecipeErr  error
	gotRecipeName    string
	gotPortionCount  int
	gotIngredients   []yazio.RecipeIngredient
	gotTotalNutrients yazio.Nutrients
	deleteRecipeErr  error
	deleteRecipeCalled bool
	gotDeleteRecipeID  string

	addRecipePortionErr     error
	addRecipePortionCalled  bool
	gotAddRecipeID          string
	gotAddPortions          float64
	gotAddRecipeMealType    string
	gotAddRecipeDate        time.Time

	removeRecipePortionErr     error
	removeRecipePortionCalled  bool
	gotRemoveRecipeEntryID     string
}

func (f *fakeYazioClient) SearchProducts(_ context.Context, query string) ([]yazio.Product, error) {
	f.gotQuery = query
	return f.searchResults, f.searchErr
}

func (f *fakeYazioClient) GetProduct(_ context.Context, id string) (*yazio.Product, error) {
	f.gotID = id
	return f.product, f.productErr
}

func (f *fakeYazioClient) GetConsumedItems(_ context.Context, date time.Time) ([]yazio.ConsumedItem, []yazio.ConsumedRecipePortion, error) {
	f.gotDate = date
	return f.consumedItems, f.recipePortions, f.consumedErr
}

func (f *fakeYazioClient) AddConsumedItem(_ context.Context, productID string, amount float64, mealType string, date time.Time) error {
	f.addCalled = true
	f.gotProductID = productID
	f.gotAmount = amount
	f.gotMealType = mealType
	f.gotAddDate = date
	return f.addErr
}

func (f *fakeYazioClient) RemoveConsumedItem(_ context.Context, itemID string) error {
	f.removeCalled = true
	f.gotRemoveID = itemID
	return f.removeErr
}

func (f *fakeYazioClient) ListRecipes(_ context.Context) ([]string, error) {
	return f.recipeIDs, f.listRecipesErr
}

func (f *fakeYazioClient) GetRecipe(_ context.Context, id string) (*yazio.Recipe, error) {
	f.gotRecipeID = id
	return f.recipe, f.getRecipeErr
}

func (f *fakeYazioClient) CreateRecipe(_ context.Context, name string, portionCount int, ingredients []yazio.RecipeIngredient, totalNutrients yazio.Nutrients, _ []string) (string, error) {
	f.gotRecipeName = name
	f.gotPortionCount = portionCount
	f.gotIngredients = ingredients
	f.gotTotalNutrients = totalNutrients
	return f.createdRecipeID, f.createRecipeErr
}

func (f *fakeYazioClient) DeleteRecipe(_ context.Context, id string) error {
	f.deleteRecipeCalled = true
	f.gotDeleteRecipeID = id
	return f.deleteRecipeErr
}

func (f *fakeYazioClient) AddConsumedRecipePortion(_ context.Context, recipeID string, portions float64, mealType string, date time.Time) error {
	f.addRecipePortionCalled = true
	f.gotAddRecipeID = recipeID
	f.gotAddPortions = portions
	f.gotAddRecipeMealType = mealType
	f.gotAddRecipeDate = date
	return f.addRecipePortionErr
}

func (f *fakeYazioClient) RemoveConsumedRecipePortion(_ context.Context, entryID string) error {
	f.removeRecipePortionCalled = true
	f.gotRemoveRecipeEntryID = entryID
	return f.removeRecipePortionErr
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testUser is the user key every test resolves through in a single-client
// map, unless a test specifically exercises multi-user resolution.
const testUser = "u1"

func clientsOf(c YazioClient) map[string]YazioClient {
	return map[string]YazioClient{testUser: c}
}

// usersOf mirrors what New() computes once and passes to every handler —
// tests call handlers directly, so they need to supply the same
// precomputed sorted key list resolveClient now expects.
func usersOf(clients map[string]YazioClient) []string {
	return userKeys(clients)
}

func TestNew_ReturnsServer(t *testing.T) {
	server := New(clientsOf(&fakeYazioClient{}), nil)
	assert.NotNil(t, server)
}

// --- resolveClient ---

func TestResolveClient_RejectsEmptyUser(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	_, err := resolveClient(clients, usersOf(clients), "some_action", "  ")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "some_action")
}

func TestResolveClient_RejectsUnknownUser(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	_, err := resolveClient(clients, usersOf(clients), "some_action", "nobody")
	assert.ErrorContains(t, err, "nobody")
	assert.ErrorContains(t, err, testUser)
}

func TestResolveClient_ReturnsConfiguredClient(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	got, err := resolveClient(clients, usersOf(clients), "some_action", testUser)
	require.NoError(t, err)
	assert.Same(t, client, got)
}

// --- search_products ---

func TestSearchProductsHandler_RejectsEmptyQuery(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := searchProductsHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, SearchProductsInput{User: testUser, Query: "  "})
	assert.Error(t, err)
}

func TestSearchProductsHandler_RejectsUnknownUser(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := searchProductsHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, SearchProductsInput{User: "nobody", Query: "soup"})
	assert.Error(t, err)
	assert.Empty(t, client.gotQuery)
}

func TestSearchProductsHandler_MapsResults(t *testing.T) {
	client := &fakeYazioClient{searchResults: []yazio.Product{
		{
			ID: "p1", Name: "Chicken soup", Producer: "Acme", BaseUnit: "g",
			Serving: "portion", ServingQuantity: 1, DefaultAmount: 350,
			Nutrients: yazio.Nutrients{yazio.NutrientEnergyKcal: 0.4},
		},
	}}
	clients := clientsOf(client)
	handler := searchProductsHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, SearchProductsInput{User: testUser, Query: "chicken soup"})
	require.NoError(t, err)
	assert.Equal(t, "chicken soup", client.gotQuery)
	require.Len(t, out.Products, 1)
	assert.Equal(t, "p1", out.Products[0].ProductID)
	assert.InDelta(t, 0.4, out.Products[0].EnergyKcalPerGram, 0.0001)
	assert.Equal(t, 1, out.Total)
}

func TestSearchProductsHandler_WrapsClientError(t *testing.T) {
	client := &fakeYazioClient{searchErr: yazio.ErrRateLimited}
	clients := clientsOf(client)
	handler := searchProductsHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, SearchProductsInput{User: testUser, Query: "a"})
	assert.ErrorIs(t, err, yazio.ErrRateLimited)
}

// --- get_product ---

func TestGetProductHandler_RejectsEmptyID(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := getProductHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, GetProductInput{User: testUser, ProductID: " "})
	assert.Error(t, err)
}

func TestGetProductHandler_MapsServings(t *testing.T) {
	client := &fakeYazioClient{product: &yazio.Product{
		ID: "cutlet-id", Name: "Cutlet", BaseUnit: "g",
		Servings: []yazio.Serving{{Type: "piece", Amount: 70}},
	}}
	clients := clientsOf(client)
	handler := getProductHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, GetProductInput{User: testUser, ProductID: "cutlet-id"})
	require.NoError(t, err)
	assert.Equal(t, "cutlet-id", client.gotID)
	require.Len(t, out.Product.Servings, 1)
	assert.Equal(t, ServingInfo{Type: "piece", AmountGrams: 70}, out.Product.Servings[0])
}

func TestGetProductHandler_WrapsNotFound(t *testing.T) {
	client := &fakeYazioClient{productErr: yazio.ErrNotFound}
	clients := clientsOf(client)
	handler := getProductHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, GetProductInput{User: testUser, ProductID: "missing"})
	assert.ErrorIs(t, err, yazio.ErrNotFound)
}

// --- get_consumed_items ---

func TestGetConsumedItemsHandler_DefaultsDateToToday(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := getConsumedItemsHandler(clients, usersOf(clients), testLogger())

	before := time.Now()
	_, out, err := handler(context.Background(), nil, GetConsumedItemsInput{User: testUser})
	require.NoError(t, err)
	assert.Equal(t, before.Format(dateLayout), out.Date)
	assert.Equal(t, before.Format(dateLayout), client.gotDate.Format(dateLayout))
}

func TestGetConsumedItemsHandler_ParsesGivenDate(t *testing.T) {
	client := &fakeYazioClient{
		consumedItems: []yazio.ConsumedItem{
			{ID: "i1", ProductID: "p1", Daytime: "lunch", Amount: 140, Serving: "piece", ServingQuantity: 2},
		},
		recipePortions: []yazio.ConsumedRecipePortion{
			{ID: "r1", RecipeID: "rec1", Daytime: "lunch", PortionCount: 1.5},
		},
	}
	clients := clientsOf(client)
	handler := getConsumedItemsHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, GetConsumedItemsInput{User: testUser, Date: "2024-01-15"})
	require.NoError(t, err)
	assert.Equal(t, "2024-01-15", out.Date)
	assert.Equal(t, "2024-01-15", client.gotDate.Format(dateLayout))
	require.Len(t, out.Items, 1)
	assert.Equal(t, 140.0, out.Items[0].AmountGrams)
	require.Len(t, out.RecipePortions, 1)
	assert.Equal(t, "r1", out.RecipePortions[0].EntryID)
	assert.Equal(t, "rec1", out.RecipePortions[0].RecipeID)
	assert.Equal(t, 1.5, out.RecipePortions[0].PortionCount)
	assert.Equal(t, 2, out.Total, "total counts both products and recipe portions")
}

func TestGetConsumedItemsHandler_RejectsMalformedDate(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := getConsumedItemsHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, GetConsumedItemsInput{User: testUser, Date: "15/01/2024"})
	assert.Error(t, err)
}

func TestGetConsumedItemsHandler_RejectsUnknownUser(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := getConsumedItemsHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, GetConsumedItemsInput{User: "nobody"})
	assert.Error(t, err)
}

// --- add_consumed_item ---

func TestAddConsumedItemHandler_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		in   AddConsumedItemInput
	}{
		{"empty user", AddConsumedItemInput{User: " ", ProductID: "p1", AmountGrams: 100, MealType: "lunch"}},
		{"unknown user", AddConsumedItemInput{User: "nobody", ProductID: "p1", AmountGrams: 100, MealType: "lunch"}},
		{"empty product_id", AddConsumedItemInput{User: testUser, ProductID: " ", AmountGrams: 100, MealType: "lunch"}},
		{"zero amount", AddConsumedItemInput{User: testUser, ProductID: "p1", AmountGrams: 0, MealType: "lunch"}},
		{"negative amount", AddConsumedItemInput{User: testUser, ProductID: "p1", AmountGrams: -5, MealType: "lunch"}},
		{"empty meal_type", AddConsumedItemInput{User: testUser, ProductID: "p1", AmountGrams: 100, MealType: " "}},
		{"malformed date", AddConsumedItemInput{User: testUser, ProductID: "p1", AmountGrams: 100, MealType: "lunch", Date: "not-a-date"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeYazioClient{}
			clients := clientsOf(client)
			handler := addConsumedItemHandler(clients, usersOf(clients), testLogger())

			_, _, err := handler(context.Background(), nil, tt.in)
			assert.Error(t, err)
			assert.False(t, client.addCalled)
		})
	}
}

func TestAddConsumedItemHandler_CallsClientWithNormalizedMealType(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := addConsumedItemHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, AddConsumedItemInput{
		User: testUser, ProductID: "cutlet-id", AmountGrams: 140, MealType: "Lunch", Date: "2024-01-15",
	})
	require.NoError(t, err)
	assert.True(t, client.addCalled)
	assert.Equal(t, "cutlet-id", client.gotProductID)
	assert.Equal(t, 140.0, client.gotAmount)
	assert.Equal(t, "lunch", client.gotMealType)
	assert.Equal(t, "2024-01-15", client.gotAddDate.Format(dateLayout))
	assert.True(t, out.Logged)
}

func TestAddConsumedItemHandler_WrapsClientError(t *testing.T) {
	client := &fakeYazioClient{addErr: yazio.ErrServiceUnavailable}
	clients := clientsOf(client)
	handler := addConsumedItemHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, AddConsumedItemInput{User: testUser, ProductID: "p1", AmountGrams: 100, MealType: "lunch"})
	assert.ErrorIs(t, err, yazio.ErrServiceUnavailable)
}

func TestAddConsumedItemHandler_UsesTheRightUsersClient(t *testing.T) {
	clientA := &fakeYazioClient{}
	clientB := &fakeYazioClient{}
	clients := map[string]YazioClient{"a": clientA, "b": clientB}
	handler := addConsumedItemHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, AddConsumedItemInput{User: "b", ProductID: "p1", AmountGrams: 100, MealType: "lunch"})
	require.NoError(t, err)
	assert.False(t, clientA.addCalled)
	assert.True(t, clientB.addCalled)
}

// --- remove_consumed_item ---

func TestRemoveConsumedItemHandler_RejectsEmptyID(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := removeConsumedItemHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, RemoveConsumedItemInput{User: testUser, ItemID: " "})
	assert.Error(t, err)
}

func TestRemoveConsumedItemHandler_CallsClient(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := removeConsumedItemHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, RemoveConsumedItemInput{User: testUser, ItemID: "entry-1"})
	require.NoError(t, err)
	assert.True(t, client.removeCalled)
	assert.Equal(t, "entry-1", client.gotRemoveID)
	assert.True(t, out.Removed)
}

// --- list_recipes ---

func TestListRecipesHandler_FetchesDetailForEachID(t *testing.T) {
	client := &fakeYazioClient{
		recipeIDs: []string{"r1", "r2"},
		recipe: &yazio.Recipe{
			ID:           "r1",
			Name:         "Borscht",
			PortionCount: 4,
			Nutrients:    yazio.Nutrients{yazio.NutrientEnergyKcal: 1200, yazio.NutrientProtein: 80},
		},
	}
	clients := clientsOf(client)
	handler := listRecipesHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, ListRecipesInput{User: testUser})
	require.NoError(t, err)
	// Both IDs are fetched; fake returns the same recipe for every GetRecipe call.
	assert.Equal(t, 2, out.Total)
	require.Len(t, out.Recipes, 2)
	assert.Equal(t, "Borscht", out.Recipes[0].Name)
	// Per-portion energy = total / portionCount = 1200 / 4 = 300
	assert.InDelta(t, 300.0, out.Recipes[0].EnergyKcal, 0.01)
}

func TestListRecipesHandler_SkipsNotFoundRecipes(t *testing.T) {
	client := &fakeYazioClient{
		recipeIDs:    []string{"missing"},
		getRecipeErr: yazio.ErrNotFound,
	}
	clients := clientsOf(client)
	handler := listRecipesHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, ListRecipesInput{User: testUser})
	require.NoError(t, err)
	assert.Empty(t, out.Recipes)
}

// --- get_recipe ---

func TestGetRecipeHandler_RejectsEmptyID(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := getRecipeHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, GetRecipeInput{User: testUser, RecipeID: "  "})
	assert.Error(t, err)
}

func TestGetRecipeHandler_MapsRecipeDetail(t *testing.T) {
	client := &fakeYazioClient{
		recipe: &yazio.Recipe{
			ID:           "rec-1",
			Name:         "Borscht",
			PortionCount: 4,
			Nutrients:    yazio.Nutrients{yazio.NutrientEnergyKcal: 1200, yazio.NutrientProtein: 80},
			Servings: []yazio.RecipeServing{
				{ProductID: "p1", Name: "Chicken", Producer: "Acme", BaseUnit: "g", Amount: 400},
			},
			Instructions: []string{"Boil", "Serve"},
		},
	}
	clients := clientsOf(client)
	handler := getRecipeHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, GetRecipeInput{User: testUser, RecipeID: "rec-1"})
	require.NoError(t, err)
	assert.Equal(t, "rec-1", client.gotRecipeID)
	assert.Equal(t, "Borscht", out.Recipe.Name)
	assert.InDelta(t, 1200.0, out.Recipe.EnergyKcal, 0.01)
	require.Len(t, out.Recipe.Ingredients, 1)
	assert.Equal(t, "Chicken", out.Recipe.Ingredients[0].Name)
	assert.Equal(t, []string{"Boil", "Serve"}, out.Recipe.Instructions)
}

// --- create_recipe ---

func TestCreateRecipeHandler_RejectsInvalidInput(t *testing.T) {
	twoIngredients := []IngredientInput{
		{ProductID: "p1", AmountGrams: 100},
		{ProductID: "p2", AmountGrams: 200},
	}
	tests := []struct {
		name string
		in   CreateRecipeInput
	}{
		{"empty user", CreateRecipeInput{User: " ", Name: "ok", Ingredients: twoIngredients}},
		{"empty name", CreateRecipeInput{User: testUser, Name: "  ", Ingredients: twoIngredients}},
		{"one ingredient", CreateRecipeInput{User: testUser, Name: "ok", Ingredients: twoIngredients[:1]}},
		{"zero amount", CreateRecipeInput{User: testUser, Name: "ok", Ingredients: []IngredientInput{{ProductID: "p1", AmountGrams: 0}, {ProductID: "p2", AmountGrams: 100}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clients := clientsOf(&fakeYazioClient{product: &yazio.Product{ID: "p1", Name: "x", BaseUnit: "g"}})
			handler := createRecipeHandler(clients, usersOf(clients), testLogger())
			_, _, err := handler(context.Background(), nil, tt.in)
			assert.Error(t, err)
		})
	}
}

func TestCreateRecipeHandler_FetchesProductsAndComputesNutrients(t *testing.T) {
	// Two products with known per-gram values; the handler should scale and sum them.
	client := &fakeYazioClient{
		createdRecipeID: "new-recipe-id",
		product: &yazio.Product{
			ID:       "p1",
			Name:     "Chicken",
			Producer: "Acme",
			BaseUnit: "g",
			Nutrients: yazio.Nutrients{
				yazio.NutrientEnergyKcal: 2.0, // 2 kcal/g
				yazio.NutrientProtein:    0.2, // 0.2 g protein/g
			},
		},
	}
	clients := clientsOf(client)
	handler := createRecipeHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, CreateRecipeInput{
		User:  testUser,
		Name:  "Chicken Bake",
		Ingredients: []IngredientInput{
			{ProductID: "p1", AmountGrams: 300},
			{ProductID: "p1", AmountGrams: 200}, // same product twice to keep fake simple
		},
		PortionCount: 2,
	})
	require.NoError(t, err)
	assert.True(t, out.Created)
	assert.Equal(t, "new-recipe-id", out.RecipeID)
	assert.Equal(t, "Chicken Bake", out.Name)
	assert.Equal(t, 2, out.PortionCount)
	assert.Equal(t, 2, out.IngredientCount)
	// Total energy = (300+200) * 2.0 = 1000 kcal
	assert.InDelta(t, 1000.0, out.EnergyKcal, 0.01)
	// Nutrients passed to CreateRecipe
	assert.InDelta(t, 1000.0, client.gotTotalNutrients[yazio.NutrientEnergyKcal], 0.01)
	assert.Equal(t, 2, client.gotPortionCount)
}

func TestCreateRecipeHandler_DefaultsPortionCountToOne(t *testing.T) {
	client := &fakeYazioClient{
		createdRecipeID: "rec-id",
		product:         &yazio.Product{ID: "p1", Name: "x", BaseUnit: "g"},
	}
	clients := clientsOf(client)
	handler := createRecipeHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, CreateRecipeInput{
		User:        testUser,
		Name:        "ok",
		Ingredients: []IngredientInput{{ProductID: "p1", AmountGrams: 100}, {ProductID: "p1", AmountGrams: 100}},
		// PortionCount omitted → should default to 1
	})
	require.NoError(t, err)
	assert.Equal(t, 1, client.gotPortionCount)
}

// --- delete_recipe ---

func TestDeleteRecipeHandler_CallsClient(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := deleteRecipeHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, DeleteRecipeInput{User: testUser, RecipeID: "rec-1"})
	require.NoError(t, err)
	assert.True(t, client.deleteRecipeCalled)
	assert.Equal(t, "rec-1", client.gotDeleteRecipeID)
	assert.True(t, out.Deleted)
}

func TestDeleteRecipeHandler_RejectsEmptyID(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := deleteRecipeHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, DeleteRecipeInput{User: testUser, RecipeID: " "})
	assert.Error(t, err)
}

// --- add_consumed_recipe ---

func TestAddConsumedRecipeHandler_CallsClientWithNormalizedMealType(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := addConsumedRecipeHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, AddConsumedRecipeInput{
		User:     testUser,
		RecipeID: "rec-1",
		Portions: 1.5,
		MealType: "Lunch",
		Date:     "2024-01-15",
	})
	require.NoError(t, err)
	assert.True(t, client.addRecipePortionCalled)
	assert.Equal(t, "rec-1", client.gotAddRecipeID)
	assert.InDelta(t, 1.5, client.gotAddPortions, 0.001)
	assert.Equal(t, "lunch", client.gotAddRecipeMealType)
	assert.Equal(t, "2024-01-15", client.gotAddRecipeDate.Format(dateLayout))
	assert.True(t, out.Logged)
}

func TestAddConsumedRecipeHandler_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		in   AddConsumedRecipeInput
	}{
		{"empty recipe_id", AddConsumedRecipeInput{User: testUser, RecipeID: " ", Portions: 1, MealType: "lunch"}},
		{"zero portions", AddConsumedRecipeInput{User: testUser, RecipeID: "r1", Portions: 0, MealType: "lunch"}},
		{"negative portions", AddConsumedRecipeInput{User: testUser, RecipeID: "r1", Portions: -1, MealType: "lunch"}},
		{"empty meal_type", AddConsumedRecipeInput{User: testUser, RecipeID: "r1", Portions: 1, MealType: " "}},
		{"malformed date", AddConsumedRecipeInput{User: testUser, RecipeID: "r1", Portions: 1, MealType: "lunch", Date: "not-a-date"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeYazioClient{}
			clients := clientsOf(client)
			handler := addConsumedRecipeHandler(clients, usersOf(clients), testLogger())
			_, _, err := handler(context.Background(), nil, tt.in)
			assert.Error(t, err)
			assert.False(t, client.addRecipePortionCalled)
		})
	}
}

// --- remove_consumed_recipe ---

func TestRemoveConsumedRecipeHandler_CallsClient(t *testing.T) {
	client := &fakeYazioClient{}
	clients := clientsOf(client)
	handler := removeConsumedRecipeHandler(clients, usersOf(clients), testLogger())

	_, out, err := handler(context.Background(), nil, RemoveConsumedRecipeInput{User: testUser, EntryID: "entry-rp-1"})
	require.NoError(t, err)
	assert.True(t, client.removeRecipePortionCalled)
	assert.Equal(t, "entry-rp-1", client.gotRemoveRecipeEntryID)
	assert.True(t, out.Removed)
}

func TestRemoveConsumedRecipeHandler_RejectsEmptyEntryID(t *testing.T) {
	clients := clientsOf(&fakeYazioClient{})
	handler := removeConsumedRecipeHandler(clients, usersOf(clients), testLogger())

	_, _, err := handler(context.Background(), nil, RemoveConsumedRecipeInput{User: testUser, EntryID: " "})
	assert.Error(t, err)
}

// --- friendlyYazioError ---

func TestFriendlyYazioError_PreservesErrorsIs(t *testing.T) {
	wrapped := friendlyYazioError("some_action", testUser, yazio.ErrRateLimited)
	assert.ErrorIs(t, wrapped, yazio.ErrRateLimited)
	assert.True(t, errors.Is(wrapped, yazio.ErrRateLimited))
}

func TestFriendlyYazioError_NamesTheUserOnAuthFailure(t *testing.T) {
	wrapped := friendlyYazioError("some_action", "archer", yazio.ErrInvalidCredentials)
	assert.ErrorContains(t, wrapped, "archer")
}
