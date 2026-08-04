package yazio

// Well-known meal slots accepted by YAZIO's diary endpoints.
const (
	MealBreakfast = "breakfast"
	MealLunch     = "lunch"
	MealDinner    = "dinner"
	MealSnack     = "snack"
)

// Well-known nutrient keys as reported by the YAZIO API. Values are per
// gram (or per milliliter, for base_unit "ml" products) — multiply by the
// consumed amount to get the total for a diary entry.
const (
	NutrientEnergyKcal = "energy.energy"
	NutrientProtein    = "nutrient.protein"
	NutrientFat        = "nutrient.fat"
	NutrientCarb       = "nutrient.carb"
)

// Nutrients holds a product's nutrient values keyed by YAZIO's dotted
// nutrient IDs (e.g. "energy.energy", "nutrient.protein"). A map is used
// instead of a fixed struct because YAZIO's product-detail response can
// carry additional vitamin/mineral keys that vary by product and are not
// documented anywhere — a map degrades gracefully (missing keys just read
// as zero) instead of failing to decode if the API adds or removes fields.
type Nutrients map[string]float64

// EnergyKcalPerGram returns kcal per gram (or per ml for liquid products).
// Missing data reads as zero rather than panicking.
func (n Nutrients) EnergyKcalPerGram() float64 { return n[NutrientEnergyKcal] }

// ProteinPerGram returns grams of protein per gram of product.
func (n Nutrients) ProteinPerGram() float64 { return n[NutrientProtein] }

// FatPerGram returns grams of fat per gram of product.
func (n Nutrients) FatPerGram() float64 { return n[NutrientFat] }

// CarbPerGram returns grams of carbohydrate per gram of product.
func (n Nutrients) CarbPerGram() float64 { return n[NutrientCarb] }

// Serving is one way a product can be measured — e.g. "piece", "portion",
// "glass" — together with how many grams (or milliliters, for base_unit
// "ml" products) one unit of that serving weighs. Servings are how a
// household quantity like "2 pieces" or "1 cup" gets converted into the
// gram amount AddConsumedItem expects.
type Serving struct {
	Type   string  `json:"serving"`
	Amount float64 `json:"amount"`
}

// Product is a YAZIO food product. Search results and product-detail
// responses share most fields; Servings and Category are only populated
// by GetProduct, and Serving/ServingQuantity/DefaultAmount only by
// SearchProducts (they describe that search result's single suggested
// default serving).
type Product struct {
	ID         string    `json:"product_id"`
	Name       string    `json:"name"`
	Producer   string    `json:"producer"`
	Category   string    `json:"category,omitempty"`
	BaseUnit   string    `json:"base_unit"`
	IsVerified bool      `json:"is_verified"`
	Nutrients  Nutrients `json:"nutrients"`

	// Populated by SearchProducts: the single default serving YAZIO
	// suggests for this result.
	Serving         string  `json:"serving,omitempty"`
	ServingQuantity float64 `json:"serving_quantity,omitempty"`
	DefaultAmount   float64 `json:"amount,omitempty"`

	// Populated by GetProduct: every serving type this product supports.
	// To log "N servings of type T", compute
	// amount = servings[T].Amount * N and pass that to AddConsumedItem.
	Servings []Serving `json:"servings,omitempty"`
}

// ConsumedItem is one diary entry: a product logged against a date and
// meal slot.
type ConsumedItem struct {
	ID              string  `json:"id"`
	ProductID       string  `json:"product_id"`
	Date            string  `json:"date"`
	Daytime         string  `json:"daytime"`
	Amount          float64 `json:"amount"`
	Serving         string  `json:"serving,omitempty"`
	ServingQuantity float64 `json:"serving_quantity,omitempty"`
}
