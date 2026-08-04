package yazio

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNutrients_AccessorsReadMissingKeysAsZero(t *testing.T) {
	// A product-detail response missing an expected nutrient key (or
	// carrying only ones we don't recognize) shouldn't panic — this is
	// the map-based resilience the unofficial API needs, since YAZIO can
	// add/remove nutrient fields without notice.
	n := Nutrients{"vitamin.a": 12.3}

	assert.Zero(t, n.EnergyKcalPerGram())
	assert.Zero(t, n.ProteinPerGram())
	assert.Zero(t, n.FatPerGram())
	assert.Zero(t, n.CarbPerGram())
}

func TestNutrients_AccessorsReadKnownKeys(t *testing.T) {
	n := Nutrients{
		NutrientEnergyKcal: 2.5,
		NutrientProtein:    0.2,
		NutrientFat:        0.1,
		NutrientCarb:       0.3,
	}

	assert.Equal(t, 2.5, n.EnergyKcalPerGram())
	assert.Equal(t, 0.2, n.ProteinPerGram())
	assert.Equal(t, 0.1, n.FatPerGram())
	assert.Equal(t, 0.3, n.CarbPerGram())
}
