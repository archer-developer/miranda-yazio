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

	consumedItems []yazio.ConsumedItem
	consumedErr   error
	gotDate       time.Time

	addErr       error
	gotProductID string
	gotAmount    float64
	gotMealType  string
	gotAddDate   time.Time
	addCalled    bool
	removeErr    error
	gotRemoveID  string
	removeCalled bool
}

func (f *fakeYazioClient) SearchProducts(_ context.Context, query string) ([]yazio.Product, error) {
	f.gotQuery = query
	return f.searchResults, f.searchErr
}

func (f *fakeYazioClient) GetProduct(_ context.Context, id string) (*yazio.Product, error) {
	f.gotID = id
	return f.product, f.productErr
}

func (f *fakeYazioClient) GetConsumedItems(_ context.Context, date time.Time) ([]yazio.ConsumedItem, error) {
	f.gotDate = date
	return f.consumedItems, f.consumedErr
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

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestNew_ReturnsServer(t *testing.T) {
	server := New(&fakeYazioClient{}, nil)
	assert.NotNil(t, server)
}

// --- search_products ---

func TestSearchProductsHandler_RejectsEmptyQuery(t *testing.T) {
	handler := searchProductsHandler(&fakeYazioClient{}, testLogger())

	_, _, err := handler(context.Background(), nil, SearchProductsInput{Query: "  "})
	assert.Error(t, err)
}

func TestSearchProductsHandler_MapsResults(t *testing.T) {
	client := &fakeYazioClient{searchResults: []yazio.Product{
		{
			ID: "p1", Name: "Chicken soup", Producer: "Acme", BaseUnit: "g",
			Serving: "portion", ServingQuantity: 1, DefaultAmount: 350,
			Nutrients: yazio.Nutrients{yazio.NutrientEnergyKcal: 0.4},
		},
	}}
	handler := searchProductsHandler(client, testLogger())

	_, out, err := handler(context.Background(), nil, SearchProductsInput{Query: "chicken soup"})
	require.NoError(t, err)
	assert.Equal(t, "chicken soup", client.gotQuery)
	require.Len(t, out.Products, 1)
	assert.Equal(t, "p1", out.Products[0].ProductID)
	assert.InDelta(t, 0.4, out.Products[0].EnergyKcalPerGram, 0.0001)
	assert.Equal(t, 1, out.Total)
}

func TestSearchProductsHandler_WrapsClientError(t *testing.T) {
	client := &fakeYazioClient{searchErr: yazio.ErrRateLimited}
	handler := searchProductsHandler(client, testLogger())

	_, _, err := handler(context.Background(), nil, SearchProductsInput{Query: "a"})
	assert.ErrorIs(t, err, yazio.ErrRateLimited)
}

// --- get_product ---

func TestGetProductHandler_RejectsEmptyID(t *testing.T) {
	handler := getProductHandler(&fakeYazioClient{}, testLogger())

	_, _, err := handler(context.Background(), nil, GetProductInput{ProductID: " "})
	assert.Error(t, err)
}

func TestGetProductHandler_MapsServings(t *testing.T) {
	client := &fakeYazioClient{product: &yazio.Product{
		ID: "cutlet-id", Name: "Cutlet", BaseUnit: "g",
		Servings: []yazio.Serving{{Type: "piece", Amount: 70}},
	}}
	handler := getProductHandler(client, testLogger())

	_, out, err := handler(context.Background(), nil, GetProductInput{ProductID: "cutlet-id"})
	require.NoError(t, err)
	assert.Equal(t, "cutlet-id", client.gotID)
	require.Len(t, out.Product.Servings, 1)
	assert.Equal(t, ServingInfo{Type: "piece", AmountGrams: 70}, out.Product.Servings[0])
}

func TestGetProductHandler_WrapsNotFound(t *testing.T) {
	client := &fakeYazioClient{productErr: yazio.ErrNotFound}
	handler := getProductHandler(client, testLogger())

	_, _, err := handler(context.Background(), nil, GetProductInput{ProductID: "missing"})
	assert.ErrorIs(t, err, yazio.ErrNotFound)
}

// --- get_consumed_items ---

func TestGetConsumedItemsHandler_DefaultsDateToToday(t *testing.T) {
	client := &fakeYazioClient{}
	handler := getConsumedItemsHandler(client, testLogger())

	before := time.Now()
	_, out, err := handler(context.Background(), nil, GetConsumedItemsInput{})
	require.NoError(t, err)
	assert.Equal(t, before.Format(dateLayout), out.Date)
	assert.Equal(t, before.Format(dateLayout), client.gotDate.Format(dateLayout))
}

func TestGetConsumedItemsHandler_ParsesGivenDate(t *testing.T) {
	client := &fakeYazioClient{consumedItems: []yazio.ConsumedItem{
		{ID: "i1", ProductID: "p1", Daytime: "lunch", Amount: 140, Serving: "piece", ServingQuantity: 2},
	}}
	handler := getConsumedItemsHandler(client, testLogger())

	_, out, err := handler(context.Background(), nil, GetConsumedItemsInput{Date: "2024-01-15"})
	require.NoError(t, err)
	assert.Equal(t, "2024-01-15", out.Date)
	assert.Equal(t, "2024-01-15", client.gotDate.Format(dateLayout))
	require.Len(t, out.Items, 1)
	assert.Equal(t, 140.0, out.Items[0].AmountGrams)
}

func TestGetConsumedItemsHandler_RejectsMalformedDate(t *testing.T) {
	handler := getConsumedItemsHandler(&fakeYazioClient{}, testLogger())

	_, _, err := handler(context.Background(), nil, GetConsumedItemsInput{Date: "15/01/2024"})
	assert.Error(t, err)
}

// --- add_consumed_item ---

func TestAddConsumedItemHandler_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		in   AddConsumedItemInput
	}{
		{"empty product_id", AddConsumedItemInput{ProductID: " ", AmountGrams: 100, MealType: "lunch"}},
		{"zero amount", AddConsumedItemInput{ProductID: "p1", AmountGrams: 0, MealType: "lunch"}},
		{"negative amount", AddConsumedItemInput{ProductID: "p1", AmountGrams: -5, MealType: "lunch"}},
		{"empty meal_type", AddConsumedItemInput{ProductID: "p1", AmountGrams: 100, MealType: " "}},
		{"malformed date", AddConsumedItemInput{ProductID: "p1", AmountGrams: 100, MealType: "lunch", Date: "not-a-date"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeYazioClient{}
			handler := addConsumedItemHandler(client, testLogger())

			_, _, err := handler(context.Background(), nil, tt.in)
			assert.Error(t, err)
			assert.False(t, client.addCalled)
		})
	}
}

func TestAddConsumedItemHandler_CallsClientWithNormalizedMealType(t *testing.T) {
	client := &fakeYazioClient{}
	handler := addConsumedItemHandler(client, testLogger())

	_, out, err := handler(context.Background(), nil, AddConsumedItemInput{
		ProductID: "cutlet-id", AmountGrams: 140, MealType: "Lunch", Date: "2024-01-15",
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
	handler := addConsumedItemHandler(client, testLogger())

	_, _, err := handler(context.Background(), nil, AddConsumedItemInput{ProductID: "p1", AmountGrams: 100, MealType: "lunch"})
	assert.ErrorIs(t, err, yazio.ErrServiceUnavailable)
}

// --- remove_consumed_item ---

func TestRemoveConsumedItemHandler_RejectsEmptyID(t *testing.T) {
	handler := removeConsumedItemHandler(&fakeYazioClient{}, testLogger())

	_, _, err := handler(context.Background(), nil, RemoveConsumedItemInput{ItemID: " "})
	assert.Error(t, err)
}

func TestRemoveConsumedItemHandler_CallsClient(t *testing.T) {
	client := &fakeYazioClient{}
	handler := removeConsumedItemHandler(client, testLogger())

	_, out, err := handler(context.Background(), nil, RemoveConsumedItemInput{ItemID: "entry-1"})
	require.NoError(t, err)
	assert.True(t, client.removeCalled)
	assert.Equal(t, "entry-1", client.gotRemoveID)
	assert.True(t, out.Removed)
}

// --- friendlyYazioError ---

func TestFriendlyYazioError_PreservesErrorsIs(t *testing.T) {
	wrapped := friendlyYazioError("some_action", yazio.ErrRateLimited)
	assert.ErrorIs(t, wrapped, yazio.ErrRateLimited)
	assert.True(t, errors.Is(wrapped, yazio.ErrRateLimited))
}
