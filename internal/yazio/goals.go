package yazio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// GetDailyGoals returns the user's configured daily nutritional targets for
// the given date. The returned Nutrients map uses the same dotted keys as
// Product.Nutrients (NutrientEnergyKcal, NutrientProtein, NutrientFat,
// NutrientCarb) but the values are absolute daily targets (kcal / g per day),
// not per-gram rates.
//
// The /unmodified suffix returns the raw configured goals without any activity-
// based adjustment, which is what a food diary context needs.
func (c *Client) GetDailyGoals(ctx context.Context, date time.Time) (Nutrients, error) {
	q := url.Values{"date": {date.Format(dateLayout)}}

	body, err := c.do(ctx, http.MethodGet, "/user/goals/unmodified", q, nil)
	if err != nil {
		return nil, err
	}

	var goals Nutrients
	if err := json.Unmarshal(body, &goals); err != nil {
		return nil, fmt.Errorf("yazio: GetDailyGoals: decode response (API may have changed shape): %w", err)
	}
	return goals, nil
}
