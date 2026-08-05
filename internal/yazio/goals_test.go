package yazio

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_GetDailyGoals_SendsDateAndParsesResponse(t *testing.T) {
	var gotPath, gotDate string
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotDate = r.URL.Query().Get("date")
			w.Write([]byte(`{"energy.energy":2000,"nutrient.protein":150,"nutrient.fat":65,"nutrient.carb":250,"activity.step":8000,"water":2000}`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	goals, err := client.GetDailyGoals(t.Context(), time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, "/user/goals/unmodified", gotPath)
	assert.Equal(t, "2024-01-15", gotDate)
	assert.InDelta(t, 2000.0, goals[NutrientEnergyKcal], 0.001)
	assert.InDelta(t, 150.0, goals[NutrientProtein], 0.001)
	assert.InDelta(t, 65.0, goals[NutrientFat], 0.001)
	assert.InDelta(t, 250.0, goals[NutrientCarb], 0.001)
}

func TestClient_GetDailyGoals_ReturnsEmptyGoalsWhenResponseIsEmpty(t *testing.T) {
	api := &fakeAPI{
		oauthHandler: tokenReply("access-1", "refresh-1"),
		apiHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`)) //nolint:errcheck
		},
	}
	client := newTestClient(t, api)

	goals, err := client.GetDailyGoals(t.Context(), time.Now())
	require.NoError(t, err)
	assert.InDelta(t, 0.0, goals[NutrientEnergyKcal], 0.001)
}
