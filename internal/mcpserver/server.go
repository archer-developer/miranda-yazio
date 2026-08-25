// Package mcpserver registers this service's MCP tools and wires them to
// internal/yazio, the YAZIO API client.
//
// Tools are grouped into three flows:
//
//   - Product diary flow (4 tools): search_products → get_product →
//     add_consumed_item logs a meal; remove_consumed_item corrects a
//     mistake. Reading the diary back is handled by get_consumed_items_range
//     (see Summary below), not a flow-specific tool — see that tool's own
//     doc comment for why a standalone raw-ID read tool was removed.
//
//   - Recipe flow (6 tools): create_recipe builds a saved multi-ingredient
//     dish; list_recipes / get_recipe browse what exists; add_consumed_recipe
//     logs portions of a recipe to the diary; remove_consumed_recipe corrects
//     a mistake; delete_recipe removes the recipe itself.
//
//   - Summary (2 tools): get_daily_summary fetches the full diary and daily
//     goals for one date and returns consumed / goals / remaining macros
//     computed server-side; get_consumed_items_range does the same kind of
//     server-side resolution across a multi-day date range, keeping each
//     entry's own resolved name and macros plus per-day and grand totals.
//     Both avoid the slow LLM tool-loop that would otherwise be needed to
//     read back 20-30+ diary entries one product/recipe fetch at a time.
//
// This service is not single-tenant: it holds a live YazioClient for each
// configured household member, and every tool takes a required "user"
// parameter selecting which account's diary to read or write. New takes a
// map keyed by that user name rather than a single client — see resolveClient.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-yazio/internal/yazio"
)

const (
	serverName    = "miranda-yazio"
	serverVersion = "0.1.0"

	// dateLayout is the YYYY-MM-DD form every tool accepts/returns for
	// dates — matches what YAZIO's own API uses for the date query param.
	dateLayout = "2006-01-02"
)

// YazioClient is the subset of *yazio.Client this package depends on.
// Defining it locally (rather than depending on the concrete type
// everywhere) lets tests substitute a fake without hitting the real
// YAZIO API — the same pattern miranda-diary uses for its Embedder.
type YazioClient interface {
	// Product search and diary (product entries).
	SearchProducts(ctx context.Context, query string) ([]yazio.Product, error)
	GetProduct(ctx context.Context, id string) (*yazio.Product, error)
	GetConsumedItems(ctx context.Context, date time.Time) ([]yazio.ConsumedItem, []yazio.ConsumedRecipePortion, error)
	AddConsumedItem(ctx context.Context, productID string, amount float64, mealType string, date time.Time) error
	RemoveConsumedItem(ctx context.Context, itemID string) error

	// Recipes.
	ListRecipes(ctx context.Context) ([]string, error)
	GetRecipe(ctx context.Context, id string) (*yazio.Recipe, error)
	CreateRecipe(ctx context.Context, name string, portionCount int, ingredients []yazio.RecipeIngredient, totalNutrients yazio.Nutrients, instructions []string) (string, error)
	DeleteRecipe(ctx context.Context, id string) error
	AddConsumedRecipePortion(ctx context.Context, recipeID string, portions float64, mealType string, date time.Time) error
	RemoveConsumedRecipePortion(ctx context.Context, entryID string) error

	// Goals and summary.
	GetDailyGoals(ctx context.Context, date time.Time) (yazio.Nutrients, error)
}

// New builds and returns the MCP server with all twelve YAZIO tools
// registered. clients maps a configured user name (as set in
// config.yaml's yazio.users[].name) to the YazioClient acting on that
// account — every tool's "user" input is resolved against this map. A nil
// logger falls back to slog.Default().
func New(clients map[string]YazioClient, logger *slog.Logger) *mcp.Server {
	if logger == nil {
		logger = slog.Default()
	}

	// Computed once and reused for every tool's description and every
	// resolveClient call — clients (and therefore its key set) doesn't
	// change for the life of the process, so there's no reason to
	// re-sort it on every failed tool call.
	users := userKeys(clients)
	userHint := fmt.Sprintf(" Must be one of the configured users: %s.", strings.Join(users, ", "))

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_products",
		Description: "Search YAZIO's food database by name or brand (e.g. \"chicken soup\", \"Kashtan ice cream\"). " +
			"Returns candidate products with their product_id, producer, base unit (g or ml), " +
			"per-gram macros, and a default suggested serving. " +
			"This is normally the first step before logging food: find the product here, then call " +
			"get_product on the chosen product_id to see all serving types it supports before calling add_consumed_item." +
			userHint,
	}, searchProductsHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_product",
		Description: "Get full detail for one product by product_id, including every serving type it supports " +
			"(e.g. \"piece\", \"portion\", \"glass\", \"gram\") and how many grams one unit of that serving weighs. " +
			"Use this to convert a household quantity into grams before calling add_consumed_item: " +
			"amount_grams = serving.amount_grams * quantity. " +
			"For example, if the user says \"2 cutlets\" and a serving named \"piece\" weighs 70g, amount_grams is 140. " +
			"If the user already gave a gram amount directly, amount_grams is just that number and this step can be skipped." +
			userHint,
	}, getProductHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "add_consumed_item",
		Description: "Log one food item to the diary for a given meal and date (date defaults to today). " +
			"amount_grams must already be a gram (or milliliter, for liquids) figure — if the user described the " +
			"amount in household units (\"2 cutlets\", \"a cup\", \"400 g\"), first call search_products and " +
			"get_product to find the matching serving size and compute amount_grams yourself; do not guess. " +
			"Call this once per distinct food item — e.g. a meal of soup, mashed potatoes, and cutlets is three calls." +
			userHint,
	}, addConsumedItemHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "remove_consumed_item",
		Description: "Delete one product diary entry by its item ID, as returned by get_consumed_items_range's items list. " +
			"Use this to correct a mistaken add_consumed_item call. " +
			"For recipe portions use remove_consumed_recipe instead." +
			userHint,
	}, removeConsumedItemHandler(clients, users, logger))

	// --- recipe tools ---

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_recipes",
		Description: "List all recipes this user has created in YAZIO, with their IDs and per-portion nutrition. " +
			"Use the recipe_id from this list to log a recipe with add_consumed_recipe or to inspect it with get_recipe." +
			userHint,
	}, listRecipesHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_recipe",
		Description: "Get full detail for one recipe by recipe_id: ingredient list, portion count, instructions, and total nutrition. " +
			"Useful to confirm which recipe to log before calling add_consumed_recipe." +
			userHint,
	}, getRecipeHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "create_recipe",
		Description: "Create a new recipe from a list of YAZIO products. " +
			"Each ingredient needs a product_id (from search_products) and an amount_grams. " +
			"At least two ingredients are required — YAZIO rejects single-ingredient recipes. " +
			"Nutrients are computed automatically from the ingredient amounts. " +
			"The returned recipe_id can be passed to add_consumed_recipe to log the recipe to the diary." +
			userHint,
	}, createRecipeHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "delete_recipe",
		Description: "Delete one of the user's own recipes. " +
			"Diary entries that already logged portions of this recipe are not affected — " +
			"use remove_consumed_recipe to remove those." +
			userHint,
	}, deleteRecipeHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "add_consumed_recipe",
		Description: "Log one or more portions of a saved recipe to the diary for a given meal and date (defaults to today). " +
			"Use list_recipes to find the recipe_id. " +
			"portions may be fractional (e.g. 0.5 for half a portion, 2 for a double helping)." +
			userHint,
	}, addConsumedRecipeHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "remove_consumed_recipe",
		Description: "Delete one recipe-portion diary entry by its entry_id, as returned by get_consumed_items_range's recipe_portions list. " +
			"Use this to correct a mistaken add_consumed_recipe call." +
			userHint,
	}, removeConsumedRecipeHandler(clients, users, logger))

	// --- summary ---

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_daily_summary",
		Description: "Returns calories and macros (protein, fat, carb) consumed today (or a given date), " +
			"the user's configured daily goals, and what remains — all computed server-side in a single call. " +
			"Use this instead of get_consumed_items_range plus manually summing every entry's macros yourself: " +
			"the server fetches the full diary, goals, and product/recipe nutrition and does the arithmetic, " +
			"which is much faster than doing it in an LLM tool loop over 20–30 diary entries." +
			userHint,
	}, getDailySummaryHandler(clients, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_consumed_items_range",
		Description: "Get every diary entry (products and recipe portions) logged over a date range, " +
			"each already resolved to its name and absolute macros (energy_kcal/protein_g/fat_g/carb_g), " +
			"plus a total for each day and a grand total for the whole range. " +
			"This is the tool to use for \"what did I eat today/yesterday/this week/last week\", " +
			"or to find the item ID/entry ID of something to correct with remove_consumed_item/remove_consumed_recipe. " +
			"Doing this manually — one lookup per product/recipe per day — is slow and token-expensive over " +
			"several days; this call resolves everything server-side in one round trip. " +
			"start_date and end_date are both optional, YYYY-MM-DD: if both are omitted they default to today; " +
			"if only start_date is given, end_date defaults to it too (a single day). " +
			"The range is inclusive on both ends and capped at 31 days. " +
			"Every day in range is included even if nothing was logged that day, so gaps are visible." +
			userHint,
	}, getConsumedItemsRangeHandler(clients, users, logger))

	return server
}

// --- shared helpers ---

// userKeys returns clients' keys sorted for stable, human-readable error
// messages and tool descriptions.
func userKeys(clients map[string]YazioClient) []string {
	return slices.Sorted(maps.Keys(clients))
}

// resolveClient looks up the YazioClient for a tool call's "user" input,
// wrapping any failure with action so callers don't each repeat that
// prefix. users is the same sorted list New() already computed once — an
// empty or unknown user is a caller error, not something worth guessing a
// default for, since accounts belong to different people.
func resolveClient(clients map[string]YazioClient, users []string, action, user string) (YazioClient, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, fmt.Errorf("%s: user is required; configured users: %s", action, strings.Join(users, ", "))
	}
	c, ok := clients[user]
	if !ok {
		return nil, fmt.Errorf("%s: unknown user %q; configured users: %s", action, user, strings.Join(users, ", "))
	}
	return c, nil
}

// ProductInfo is the MCP-facing shape of a yazio.Product: flattened
// per-gram macros instead of a raw nutrient map, and grams-first naming
// so the calling model doesn't have to know YAZIO's internal field names.
type ProductInfo struct {
	ProductID  string `json:"product_id"`
	Name       string `json:"name"`
	Producer   string `json:"producer,omitempty"`
	Category   string `json:"category,omitempty"`
	BaseUnit   string `json:"base_unit"`
	IsVerified bool   `json:"is_verified,omitempty"`

	EnergyKcalPerGram float64 `json:"energy_kcal_per_gram"`
	ProteinPerGram    float64 `json:"protein_per_gram"`
	FatPerGram        float64 `json:"fat_per_gram"`
	CarbPerGram       float64 `json:"carb_per_gram"`

	// Populated by search_products: the single default serving YAZIO
	// suggests for this result.
	DefaultServing         string  `json:"default_serving,omitempty"`
	DefaultServingQuantity float64 `json:"default_serving_quantity,omitempty"`
	DefaultAmountGrams     float64 `json:"default_amount_grams,omitempty"`

	// Populated by get_product: every serving type this product supports.
	Servings []ServingInfo `json:"servings,omitempty"`
}

// ServingInfo names one way a product can be measured and how many grams
// (or milliliters) one unit of it weighs.
type ServingInfo struct {
	Type        string  `json:"type"`
	AmountGrams float64 `json:"amount_grams"`
}

func toProductInfo(p yazio.Product) ProductInfo {
	info := ProductInfo{
		ProductID:  p.ID,
		Name:       p.Name,
		Producer:   p.Producer,
		Category:   p.Category,
		BaseUnit:   p.BaseUnit,
		IsVerified: p.IsVerified,

		EnergyKcalPerGram: p.Nutrients.EnergyKcalPerGram(),
		ProteinPerGram:    p.Nutrients.ProteinPerGram(),
		FatPerGram:        p.Nutrients.FatPerGram(),
		CarbPerGram:       p.Nutrients.CarbPerGram(),

		DefaultServing:         p.Serving,
		DefaultServingQuantity: p.ServingQuantity,
		DefaultAmountGrams:     p.DefaultAmount,
	}

	if len(p.Servings) > 0 {
		info.Servings = make([]ServingInfo, len(p.Servings))
		for i, s := range p.Servings {
			info.Servings[i] = ServingInfo{Type: s.Type, AmountGrams: s.Amount}
		}
	}

	return info
}

// parseDateOrToday parses a YYYY-MM-DD string, defaulting to the server's
// current local date when raw is empty — every tool that takes a date
// treats it as optional for exactly this reason.
func parseDateOrToday(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now(), nil
	}
	d, err := time.Parse(dateLayout, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("date must be in YYYY-MM-DD format, got %q", raw)
	}
	return d, nil
}

// maxConsumedItemsRangeDays caps get_consumed_items_range's span (inclusive)
// so one call can't fan out into an unbounded number of per-day
// GetConsumedItems calls (each of which may itself trigger several
// per-product/per-recipe fetches) — YAZIO has no batch/range diary
// endpoint, so every day in the range costs a separate round trip.
const maxConsumedItemsRangeDays = 31

// truncateToDate drops the time-of-day component, keeping only the
// calendar date in t's own location — needed so the day-loop
// (AddDate(0, 0, 1)) and the range-length check below aren't thrown off
// by a non-midnight time-of-day inherited from parseDateOrToday's
// time.Now() default.
func truncateToDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// parseDateRange parses the optional start_date/end_date pair for
// get_consumed_items_range:
//   - both omitted: both default to today (a single-day range)
//   - only start_date given: end_date defaults to start_date (also a
//     single day)
//   - only end_date given: rejected — there's no sensible start_date to
//     infer, and silently picking one (e.g. 7 days back) would be a
//     surprising default for a caller who explicitly named an end
//   - end_date before start_date: rejected
//   - range spanning more than maxConsumedItemsRangeDays days inclusive:
//     rejected
func parseDateRange(rawStart, rawEnd string) (start, end time.Time, err error) {
	rawStart = strings.TrimSpace(rawStart)
	rawEnd = strings.TrimSpace(rawEnd)

	if rawStart == "" && rawEnd != "" {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"end_date %q given without start_date — provide start_date too, or omit end_date to default it to start_date", rawEnd)
	}

	start, err = parseDateOrToday(rawStart)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start = truncateToDate(start)

	if rawEnd == "" {
		end = start
	} else {
		end, err = time.Parse(dateLayout, rawEnd)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end_date must be in YYYY-MM-DD format, got %q", rawEnd)
		}
		end = truncateToDate(end)
	}

	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"end_date %s is before start_date %s", end.Format(dateLayout), start.Format(dateLayout))
	}

	// Count days by walking rather than dividing Sub() by 24h, so DST
	// transitions in the server's local zone can't throw the count off by
	// one — this also doubles as a cheap, iteration-bounded cap check.
	days := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days++
		if days > maxConsumedItemsRangeDays {
			return time.Time{}, time.Time{}, fmt.Errorf(
				"date range too large: %s to %s spans more than %d days — narrow the range",
				start.Format(dateLayout), end.Format(dateLayout), maxConsumedItemsRangeDays)
		}
	}

	return start, end, nil
}

// friendlyYazioError adds a short, actionable prefix for the sentinel
// errors internal/yazio can return, since the underlying HTTP status
// alone isn't meaningful to whoever (or whatever) reads the tool error.
// user names which configured account the call was for, so an auth
// failure among several configured users points at the right one instead
// of leaving the operator to guess.
func friendlyYazioError(action, user string, err error) error {
	switch {
	case errors.Is(err, yazio.ErrInvalidCredentials), errors.Is(err, yazio.ErrUnauthorized):
		return fmt.Errorf("%s: YAZIO authentication failed for user %q — check that user's configured username/password env vars and that the account isn't locked: %w", action, user, err)
	case errors.Is(err, yazio.ErrRateLimited):
		return fmt.Errorf("%s: YAZIO is rate-limiting requests — wait a bit and try again: %w", action, err)
	case errors.Is(err, yazio.ErrServiceUnavailable):
		return fmt.Errorf("%s: YAZIO's API is unavailable right now — this is YAZIO's side, try again later: %w", action, err)
	case errors.Is(err, yazio.ErrNotFound):
		return fmt.Errorf("%s: not found on YAZIO — double check the product_id/item_id: %w", action, err)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}

// --- search_products ---

type SearchProductsInput struct {
	User  string `json:"user" jsonschema:"Which configured YAZIO account to search products for."`
	Query string `json:"query" jsonschema:"Search text for the food product, e.g. a dish name, ingredient, or brand. Works best in the account's configured language/region."`
}

type SearchProductsOutput struct {
	Products []ProductInfo `json:"products"`
	Total    int           `json:"total"`
}

func searchProductsHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[SearchProductsInput, SearchProductsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchProductsInput) (*mcp.CallToolResult, SearchProductsOutput, error) {
		client, err := resolveClient(clients, users, "search_products", in.User)
		if err != nil {
			return nil, SearchProductsOutput{}, err
		}
		if strings.TrimSpace(in.Query) == "" {
			return nil, SearchProductsOutput{}, fmt.Errorf("search_products: query must not be empty")
		}

		results, err := client.SearchProducts(ctx, in.Query)
		if err != nil {
			return nil, SearchProductsOutput{}, friendlyYazioError("search_products", in.User, err)
		}

		products := make([]ProductInfo, len(results))
		for i, p := range results {
			products[i] = toProductInfo(p)
		}

		logger.Info("search_products", "user", in.User, "query", in.Query, "found", len(products))

		out := SearchProductsOutput{Products: products, Total: len(products)}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatSearchResults(products)}}}, out, nil
	}
}

func formatSearchResults(products []ProductInfo) string {
	if len(products) == 0 {
		return "No matching products found."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d product(s):\n", len(products))
	for i, p := range products {
		fmt.Fprintf(&b, "\n--- #%d: %s", i+1, p.Name)
		if p.Producer != "" {
			fmt.Fprintf(&b, " (%s)", p.Producer)
		}
		b.WriteString(" ---\n")
		fmt.Fprintf(&b, "product_id: %s\n", p.ProductID)
		fmt.Fprintf(&b, "default serving: %g x %q ≈ %g%s\n", p.DefaultServingQuantity, p.DefaultServing, p.DefaultAmountGrams, p.BaseUnit)
		fmt.Fprintf(&b, "per gram: %.2f kcal, %.2fg protein, %.2fg fat, %.2fg carb\n", p.EnergyKcalPerGram, p.ProteinPerGram, p.FatPerGram, p.CarbPerGram)
	}
	return b.String()
}

// --- get_product ---

type GetProductInput struct {
	User      string `json:"user" jsonschema:"Which configured YAZIO account to look up the product for."`
	ProductID string `json:"product_id" jsonschema:"The product_id returned by search_products."`
}

type GetProductOutput struct {
	Product ProductInfo `json:"product"`
}

func getProductHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[GetProductInput, GetProductOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetProductInput) (*mcp.CallToolResult, GetProductOutput, error) {
		client, err := resolveClient(clients, users, "get_product", in.User)
		if err != nil {
			return nil, GetProductOutput{}, err
		}
		if strings.TrimSpace(in.ProductID) == "" {
			return nil, GetProductOutput{}, fmt.Errorf("get_product: product_id must not be empty")
		}

		p, err := client.GetProduct(ctx, in.ProductID)
		if err != nil {
			return nil, GetProductOutput{}, friendlyYazioError("get_product", in.User, err)
		}

		info := toProductInfo(*p)
		logger.Info("get_product", "user", in.User, "product_id", in.ProductID, "servings", len(info.Servings))

		out := GetProductOutput{Product: info}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatProductDetail(info)}}}, out, nil
	}
}

func formatProductDetail(p ProductInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", p.Name)
	if p.Producer != "" {
		fmt.Fprintf(&b, " (%s)", p.Producer)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "base unit: %s\n", p.BaseUnit)
	fmt.Fprintf(&b, "per gram: %.2f kcal, %.2fg protein, %.2fg fat, %.2fg carb\n", p.EnergyKcalPerGram, p.ProteinPerGram, p.FatPerGram, p.CarbPerGram)
	if len(p.Servings) == 0 {
		b.WriteString("no named servings — log amount_grams directly.\n")
		return b.String()
	}
	b.WriteString("servings (amount_grams = this weight * quantity):\n")
	for _, s := range p.Servings {
		fmt.Fprintf(&b, "  - %q ≈ %g%s each\n", s.Type, s.AmountGrams, p.BaseUnit)
	}
	return b.String()
}

// --- add_consumed_item ---

type AddConsumedItemInput struct {
	User        string  `json:"user" jsonschema:"Which configured YAZIO account's diary to log this item to."`
	ProductID   string  `json:"product_id" jsonschema:"The product_id returned by search_products or get_product."`
	AmountGrams float64 `json:"amount_grams" jsonschema:"Amount consumed, in grams (or milliliters for liquids). Must already be converted from any household unit using get_product's servings — see that tool's description."`
	MealType    string  `json:"meal_type" jsonschema:"One of breakfast, lunch, dinner, snack."`
	Date        string  `json:"date,omitempty" jsonschema:"Date the item was consumed, YYYY-MM-DD. Defaults to today if omitted."`
}

type AddConsumedItemOutput struct {
	Logged      bool    `json:"logged"`
	ProductID   string  `json:"product_id"`
	AmountGrams float64 `json:"amount_grams"`
	MealType    string  `json:"meal_type"`
	Date        string  `json:"date"`
}

func addConsumedItemHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[AddConsumedItemInput, AddConsumedItemOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AddConsumedItemInput) (*mcp.CallToolResult, AddConsumedItemOutput, error) {
		client, err := resolveClient(clients, users, "add_consumed_item", in.User)
		if err != nil {
			return nil, AddConsumedItemOutput{}, err
		}
		if strings.TrimSpace(in.ProductID) == "" {
			return nil, AddConsumedItemOutput{}, fmt.Errorf("add_consumed_item: product_id must not be empty")
		}
		if in.AmountGrams <= 0 {
			return nil, AddConsumedItemOutput{}, fmt.Errorf("add_consumed_item: amount_grams must be positive, got %v", in.AmountGrams)
		}
		if strings.TrimSpace(in.MealType) == "" {
			return nil, AddConsumedItemOutput{}, fmt.Errorf("add_consumed_item: meal_type must not be empty")
		}

		date, err := parseDateOrToday(in.Date)
		if err != nil {
			return nil, AddConsumedItemOutput{}, fmt.Errorf("add_consumed_item: %w", err)
		}

		mealType := strings.ToLower(strings.TrimSpace(in.MealType))
		if err := client.AddConsumedItem(ctx, in.ProductID, in.AmountGrams, mealType, date); err != nil {
			return nil, AddConsumedItemOutput{}, friendlyYazioError("add_consumed_item", in.User, err)
		}

		dateStr := date.Format(dateLayout)
		logger.Info("add_consumed_item", "user", in.User, "product_id", in.ProductID, "amount_grams", in.AmountGrams, "meal_type", mealType, "date", dateStr)

		out := AddConsumedItemOutput{Logged: true, ProductID: in.ProductID, AmountGrams: in.AmountGrams, MealType: mealType, Date: dateStr}
		text := fmt.Sprintf("Logged %gg of %s as %s on %s.", in.AmountGrams, in.ProductID, mealType, dateStr)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- remove_consumed_item ---

type RemoveConsumedItemInput struct {
	User   string `json:"user" jsonschema:"Which configured YAZIO account's diary to delete the entry from."`
	ItemID string `json:"item_id" jsonschema:"The diary entry ID to delete, as returned by get_consumed_items_range's \"id\" field (not product_id)."`
}

type RemoveConsumedItemOutput struct {
	Removed bool   `json:"removed"`
	ItemID  string `json:"item_id"`
}

func removeConsumedItemHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[RemoveConsumedItemInput, RemoveConsumedItemOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RemoveConsumedItemInput) (*mcp.CallToolResult, RemoveConsumedItemOutput, error) {
		client, err := resolveClient(clients, users, "remove_consumed_item", in.User)
		if err != nil {
			return nil, RemoveConsumedItemOutput{}, err
		}
		if strings.TrimSpace(in.ItemID) == "" {
			return nil, RemoveConsumedItemOutput{}, fmt.Errorf("remove_consumed_item: item_id must not be empty")
		}

		if err := client.RemoveConsumedItem(ctx, in.ItemID); err != nil {
			return nil, RemoveConsumedItemOutput{}, friendlyYazioError("remove_consumed_item", in.User, err)
		}

		logger.Info("remove_consumed_item", "user", in.User, "item_id", in.ItemID)

		out := RemoveConsumedItemOutput{Removed: true, ItemID: in.ItemID}
		text := fmt.Sprintf("Deleted diary entry %s.", in.ItemID)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- list_recipes ---

type ListRecipesInput struct {
	User string `json:"user" jsonschema:"Which configured YAZIO account's recipes to list."`
}

type RecipeSummary struct {
	RecipeID     string  `json:"recipe_id"`
	Name         string  `json:"name"`
	PortionCount float64 `json:"portion_count"`
	EnergyKcal   float64 `json:"energy_kcal_per_portion"`
	ProteinG     float64 `json:"protein_g_per_portion"`
	FatG         float64 `json:"fat_g_per_portion"`
	CarbG        float64 `json:"carb_g_per_portion"`
}

type ListRecipesOutput struct {
	Recipes []RecipeSummary `json:"recipes"`
	Total   int             `json:"total"`
}

func listRecipesHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[ListRecipesInput, ListRecipesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ListRecipesInput) (*mcp.CallToolResult, ListRecipesOutput, error) {
		client, err := resolveClient(clients, users, "list_recipes", in.User)
		if err != nil {
			return nil, ListRecipesOutput{}, err
		}

		ids, err := client.ListRecipes(ctx)
		if err != nil {
			return nil, ListRecipesOutput{}, friendlyYazioError("list_recipes", in.User, err)
		}

		summaries := make([]RecipeSummary, 0, len(ids))
		for _, id := range ids {
			r, err := client.GetRecipe(ctx, id)
			if err != nil {
				// A recipe that 404s between list and fetch is not fatal —
				// skip it rather than aborting the whole listing.
				if errors.Is(err, yazio.ErrNotFound) {
					logger.Warn("list_recipes: recipe disappeared between list and fetch", "user", in.User, "recipe_id", id)
					continue
				}
				return nil, ListRecipesOutput{}, friendlyYazioError("list_recipes", in.User, err)
			}
			portions := max(r.PortionCount, 1)
			summaries = append(summaries, RecipeSummary{
				RecipeID:     r.ID,
				Name:         r.Name,
				PortionCount: r.PortionCount,
				EnergyKcal:   r.Nutrients.EnergyKcalPerGram() / portions,
				ProteinG:     r.Nutrients.ProteinPerGram() / portions,
				FatG:         r.Nutrients.FatPerGram() / portions,
				CarbG:        r.Nutrients.CarbPerGram() / portions,
			})
		}

		logger.Info("list_recipes", "user", in.User, "found", len(summaries))

		out := ListRecipesOutput{Recipes: summaries, Total: len(summaries)}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatRecipeList(summaries)}}}, out, nil
	}
}

func formatRecipeList(recipes []RecipeSummary) string {
	if len(recipes) == 0 {
		return "No recipes found."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d recipe(s):\n", len(recipes))
	for i, r := range recipes {
		fmt.Fprintf(&b, "\n--- #%d: %s ---\n", i+1, r.Name)
		fmt.Fprintf(&b, "recipe_id: %s\n", r.RecipeID)
		fmt.Fprintf(&b, "portions: %g  |  per portion: %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb\n",
			r.PortionCount, r.EnergyKcal, r.ProteinG, r.FatG, r.CarbG)
	}
	return b.String()
}

// --- get_recipe ---

type GetRecipeInput struct {
	User     string `json:"user" jsonschema:"Which configured YAZIO account to look up the recipe for."`
	RecipeID string `json:"recipe_id" jsonschema:"The recipe UUID, from list_recipes."`
}

type RecipeDetail struct {
	RecipeID        string          `json:"recipe_id"`
	Name            string          `json:"name"`
	PortionCount    float64         `json:"portion_count"`
	EnergyKcal      float64         `json:"energy_kcal_total"`
	ProteinG        float64         `json:"protein_g_total"`
	FatG            float64         `json:"fat_g_total"`
	CarbG           float64         `json:"carb_g_total"`
	Ingredients     []IngredientInfo `json:"ingredients"`
	Instructions    []string        `json:"instructions,omitempty"`
}

type IngredientInfo struct {
	ProductID   string  `json:"product_id"`
	Name        string  `json:"name"`
	Producer    string  `json:"producer,omitempty"`
	AmountGrams float64 `json:"amount_grams"`
}

type GetRecipeOutput struct {
	Recipe RecipeDetail `json:"recipe"`
}

func getRecipeHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[GetRecipeInput, GetRecipeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetRecipeInput) (*mcp.CallToolResult, GetRecipeOutput, error) {
		client, err := resolveClient(clients, users, "get_recipe", in.User)
		if err != nil {
			return nil, GetRecipeOutput{}, err
		}
		if strings.TrimSpace(in.RecipeID) == "" {
			return nil, GetRecipeOutput{}, fmt.Errorf("get_recipe: recipe_id must not be empty")
		}

		r, err := client.GetRecipe(ctx, in.RecipeID)
		if err != nil {
			return nil, GetRecipeOutput{}, friendlyYazioError("get_recipe", in.User, err)
		}

		ings := make([]IngredientInfo, len(r.Servings))
		for i, s := range r.Servings {
			ings[i] = IngredientInfo{ProductID: s.ProductID, Name: s.Name, Producer: s.Producer, AmountGrams: s.Amount}
		}

		detail := RecipeDetail{
			RecipeID:     r.ID,
			Name:         r.Name,
			PortionCount: r.PortionCount,
			EnergyKcal:   r.Nutrients.EnergyKcalPerGram(),
			ProteinG:     r.Nutrients.ProteinPerGram(),
			FatG:         r.Nutrients.FatPerGram(),
			CarbG:        r.Nutrients.CarbPerGram(),
			Ingredients:  ings,
			Instructions: r.Instructions,
		}

		logger.Info("get_recipe", "user", in.User, "recipe_id", in.RecipeID, "ingredients", len(ings))

		out := GetRecipeOutput{Recipe: detail}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatRecipeDetail(detail)}}}, out, nil
	}
}

func formatRecipeDetail(r RecipeDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (recipe_id: %s)\n", r.Name, r.RecipeID)
	fmt.Fprintf(&b, "portions: %g  |  total: %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb\n",
		r.PortionCount, r.EnergyKcal, r.ProteinG, r.FatG, r.CarbG)
	if len(r.Ingredients) > 0 {
		b.WriteString("ingredients:\n")
		for _, ing := range r.Ingredients {
			fmt.Fprintf(&b, "  - %s", ing.Name)
			if ing.Producer != "" {
				fmt.Fprintf(&b, " (%s)", ing.Producer)
			}
			fmt.Fprintf(&b, ": %gg  [product_id: %s]\n", ing.AmountGrams, ing.ProductID)
		}
	}
	if len(r.Instructions) > 0 {
		b.WriteString("instructions:\n")
		for i, step := range r.Instructions {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
		}
	}
	return b.String()
}

// --- create_recipe ---

type IngredientInput struct {
	ProductID   string  `json:"product_id" jsonschema:"Product UUID from search_products or get_product."`
	AmountGrams float64 `json:"amount_grams" jsonschema:"Amount of this ingredient in the whole recipe, in grams (or ml for liquids)."`
}

type CreateRecipeInput struct {
	User         string            `json:"user" jsonschema:"Which configured YAZIO account to create the recipe under."`
	Name         string            `json:"name" jsonschema:"Name of the recipe, e.g. \"Borscht\" or \"Chicken with rice\"."`
	Ingredients  []IngredientInput `json:"ingredients" jsonschema:"List of ingredients (at least 2). Each needs a product_id from search_products and the amount in grams used in the whole recipe."`
	PortionCount int               `json:"portion_count,omitempty" jsonschema:"How many portions the recipe makes (defaults to 1). Whole numbers only."`
	Instructions []string          `json:"instructions,omitempty" jsonschema:"Optional preparation steps, one string per step."`
}

type CreateRecipeOutput struct {
	Created         bool    `json:"created"`
	RecipeID        string  `json:"recipe_id"`
	Name            string  `json:"name"`
	PortionCount    int     `json:"portion_count"`
	IngredientCount int     `json:"ingredient_count"`
	EnergyKcal      float64 `json:"energy_kcal_total"`
	ProteinG        float64 `json:"protein_g_total"`
	FatG            float64 `json:"fat_g_total"`
	CarbG           float64 `json:"carb_g_total"`
}

func createRecipeHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[CreateRecipeInput, CreateRecipeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in CreateRecipeInput) (*mcp.CallToolResult, CreateRecipeOutput, error) {
		client, err := resolveClient(clients, users, "create_recipe", in.User)
		if err != nil {
			return nil, CreateRecipeOutput{}, err
		}
		if strings.TrimSpace(in.Name) == "" {
			return nil, CreateRecipeOutput{}, fmt.Errorf("create_recipe: name must not be empty")
		}
		if len(in.Ingredients) < 2 {
			return nil, CreateRecipeOutput{}, fmt.Errorf("create_recipe: at least 2 ingredients required, got %d", len(in.Ingredients))
		}
		portionCount := in.PortionCount
		if portionCount <= 0 {
			portionCount = 1
		}

		// Resolve each ingredient: fetch its product to get name/producer/base_unit
		// and scale its per-gram nutrients by the amount used in this recipe.
		resolved := make([]yazio.RecipeIngredient, 0, len(in.Ingredients))
		totalNutrients := yazio.Nutrients{}
		for i, ing := range in.Ingredients {
			if strings.TrimSpace(ing.ProductID) == "" {
				return nil, CreateRecipeOutput{}, fmt.Errorf("create_recipe: ingredient %d: product_id must not be empty", i+1)
			}
			if ing.AmountGrams <= 0 {
				return nil, CreateRecipeOutput{}, fmt.Errorf("create_recipe: ingredient %d: amount_grams must be positive, got %v", i+1, ing.AmountGrams)
			}
			p, err := client.GetProduct(ctx, ing.ProductID)
			if err != nil {
				return nil, CreateRecipeOutput{}, friendlyYazioError(fmt.Sprintf("create_recipe: ingredient %d (product_id %s)", i+1, ing.ProductID), in.User, err)
			}
			// Scale per-gram nutrient values by the amount this ingredient
			// contributes to the recipe — YAZIO's recipe endpoint stores the
			// client-submitted totals verbatim and does not recompute them.
			for k, perGram := range p.Nutrients {
				totalNutrients[k] += perGram * ing.AmountGrams
			}
			resolved = append(resolved, yazio.RecipeIngredient{
				ProductID: ing.ProductID,
				Name:      p.Name,
				Producer:  p.Producer,
				BaseUnit:  p.BaseUnit,
				Amount:    ing.AmountGrams,
			})
		}

		recipeID, err := client.CreateRecipe(ctx, in.Name, portionCount, resolved, totalNutrients, in.Instructions)
		if err != nil {
			return nil, CreateRecipeOutput{}, friendlyYazioError("create_recipe", in.User, err)
		}

		logger.Info("create_recipe", "user", in.User, "recipe_id", recipeID, "name", in.Name, "ingredients", len(resolved))

		out := CreateRecipeOutput{
			Created:         true,
			RecipeID:        recipeID,
			Name:            in.Name,
			PortionCount:    portionCount,
			IngredientCount: len(resolved),
			EnergyKcal:      totalNutrients.EnergyKcalPerGram(),
			ProteinG:        totalNutrients.ProteinPerGram(),
			FatG:            totalNutrients.FatPerGram(),
			CarbG:           totalNutrients.CarbPerGram(),
		}
		text := fmt.Sprintf("Created recipe %q (recipe_id: %s) with %d ingredient(s). Total: %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb.",
			in.Name, recipeID, len(resolved), out.EnergyKcal, out.ProteinG, out.FatG, out.CarbG)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- delete_recipe ---

type DeleteRecipeInput struct {
	User     string `json:"user" jsonschema:"Which configured YAZIO account owns the recipe."`
	RecipeID string `json:"recipe_id" jsonschema:"The recipe UUID to delete, from list_recipes."`
}

type DeleteRecipeOutput struct {
	Deleted  bool   `json:"deleted"`
	RecipeID string `json:"recipe_id"`
}

func deleteRecipeHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[DeleteRecipeInput, DeleteRecipeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in DeleteRecipeInput) (*mcp.CallToolResult, DeleteRecipeOutput, error) {
		client, err := resolveClient(clients, users, "delete_recipe", in.User)
		if err != nil {
			return nil, DeleteRecipeOutput{}, err
		}
		if strings.TrimSpace(in.RecipeID) == "" {
			return nil, DeleteRecipeOutput{}, fmt.Errorf("delete_recipe: recipe_id must not be empty")
		}

		if err := client.DeleteRecipe(ctx, in.RecipeID); err != nil {
			return nil, DeleteRecipeOutput{}, friendlyYazioError("delete_recipe", in.User, err)
		}

		logger.Info("delete_recipe", "user", in.User, "recipe_id", in.RecipeID)

		out := DeleteRecipeOutput{Deleted: true, RecipeID: in.RecipeID}
		text := fmt.Sprintf("Deleted recipe %s.", in.RecipeID)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- add_consumed_recipe ---

type AddConsumedRecipeInput struct {
	User     string  `json:"user" jsonschema:"Which configured YAZIO account's diary to log this recipe to."`
	RecipeID string  `json:"recipe_id" jsonschema:"The recipe UUID, from list_recipes."`
	Portions float64 `json:"portions" jsonschema:"How many portions to log. May be fractional (e.g. 0.5 for half, 2 for double)."`
	MealType string  `json:"meal_type" jsonschema:"One of breakfast, lunch, dinner, snack."`
	Date     string  `json:"date,omitempty" jsonschema:"Date the recipe was eaten, YYYY-MM-DD. Defaults to today if omitted."`
}

type AddConsumedRecipeOutput struct {
	Logged   bool    `json:"logged"`
	RecipeID string  `json:"recipe_id"`
	Portions float64 `json:"portions"`
	MealType string  `json:"meal_type"`
	Date     string  `json:"date"`
}

func addConsumedRecipeHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[AddConsumedRecipeInput, AddConsumedRecipeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AddConsumedRecipeInput) (*mcp.CallToolResult, AddConsumedRecipeOutput, error) {
		client, err := resolveClient(clients, users, "add_consumed_recipe", in.User)
		if err != nil {
			return nil, AddConsumedRecipeOutput{}, err
		}
		if strings.TrimSpace(in.RecipeID) == "" {
			return nil, AddConsumedRecipeOutput{}, fmt.Errorf("add_consumed_recipe: recipe_id must not be empty")
		}
		if in.Portions <= 0 {
			return nil, AddConsumedRecipeOutput{}, fmt.Errorf("add_consumed_recipe: portions must be positive, got %v", in.Portions)
		}
		if strings.TrimSpace(in.MealType) == "" {
			return nil, AddConsumedRecipeOutput{}, fmt.Errorf("add_consumed_recipe: meal_type must not be empty")
		}

		date, err := parseDateOrToday(in.Date)
		if err != nil {
			return nil, AddConsumedRecipeOutput{}, fmt.Errorf("add_consumed_recipe: %w", err)
		}

		mealType := strings.ToLower(strings.TrimSpace(in.MealType))
		if err := client.AddConsumedRecipePortion(ctx, in.RecipeID, in.Portions, mealType, date); err != nil {
			return nil, AddConsumedRecipeOutput{}, friendlyYazioError("add_consumed_recipe", in.User, err)
		}

		dateStr := date.Format(dateLayout)
		logger.Info("add_consumed_recipe", "user", in.User, "recipe_id", in.RecipeID, "portions", in.Portions, "meal_type", mealType, "date", dateStr)

		out := AddConsumedRecipeOutput{Logged: true, RecipeID: in.RecipeID, Portions: in.Portions, MealType: mealType, Date: dateStr}
		text := fmt.Sprintf("Logged %g portion(s) of recipe %s as %s on %s.", in.Portions, in.RecipeID, mealType, dateStr)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- remove_consumed_recipe ---

type RemoveConsumedRecipeInput struct {
	User    string `json:"user" jsonschema:"Which configured YAZIO account's diary to delete the entry from."`
	EntryID string `json:"entry_id" jsonschema:"The recipe-portion diary entry ID to delete, as returned by get_consumed_items_range's recipe_portions list (entry_id field, not recipe_id)."`
}

type RemoveConsumedRecipeOutput struct {
	Removed bool   `json:"removed"`
	EntryID string `json:"entry_id"`
}

func removeConsumedRecipeHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[RemoveConsumedRecipeInput, RemoveConsumedRecipeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RemoveConsumedRecipeInput) (*mcp.CallToolResult, RemoveConsumedRecipeOutput, error) {
		client, err := resolveClient(clients, users, "remove_consumed_recipe", in.User)
		if err != nil {
			return nil, RemoveConsumedRecipeOutput{}, err
		}
		if strings.TrimSpace(in.EntryID) == "" {
			return nil, RemoveConsumedRecipeOutput{}, fmt.Errorf("remove_consumed_recipe: entry_id must not be empty")
		}

		if err := client.RemoveConsumedRecipePortion(ctx, in.EntryID); err != nil {
			return nil, RemoveConsumedRecipeOutput{}, friendlyYazioError("remove_consumed_recipe", in.User, err)
		}

		logger.Info("remove_consumed_recipe", "user", in.User, "entry_id", in.EntryID)

		out := RemoveConsumedRecipeOutput{Removed: true, EntryID: in.EntryID}
		text := fmt.Sprintf("Deleted recipe diary entry %s.", in.EntryID)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- get_daily_summary ---

// NutrientTotals holds the four key macros as absolute amounts (kcal / g),
// used for consumed totals, daily goals, and the remaining difference.
type NutrientTotals struct {
	EnergyKcal float64 `json:"energy_kcal"`
	ProteinG   float64 `json:"protein_g"`
	FatG       float64 `json:"fat_g"`
	CarbG      float64 `json:"carb_g"`
}

type GetDailySummaryInput struct {
	User string `json:"user" jsonschema:"Which configured YAZIO account to summarize."`
	Date string `json:"date,omitempty" jsonschema:"Date to summarize, YYYY-MM-DD. Defaults to today if omitted."`
}

type GetDailySummaryOutput struct {
	Date      string         `json:"date"`
	Consumed  NutrientTotals `json:"consumed"`
	Goals     NutrientTotals `json:"goals"`
	Remaining NutrientTotals `json:"remaining"`
}

func getDailySummaryHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[GetDailySummaryInput, GetDailySummaryOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetDailySummaryInput) (*mcp.CallToolResult, GetDailySummaryOutput, error) {
		client, err := resolveClient(clients, users, "get_daily_summary", in.User)
		if err != nil {
			return nil, GetDailySummaryOutput{}, err
		}

		date, err := parseDateOrToday(in.Date)
		if err != nil {
			return nil, GetDailySummaryOutput{}, fmt.Errorf("get_daily_summary: %w", err)
		}

		productEntries, recipePortions, err := client.GetConsumedItems(ctx, date)
		if err != nil {
			return nil, GetDailySummaryOutput{}, friendlyYazioError("get_daily_summary", in.User, err)
		}

		// Fetch each unique product once and accumulate consumed nutrients.
		// Products that appear multiple times in the diary (same product logged
		// at different meals) are deduplicated to avoid redundant API calls.
		consumed := yazio.Nutrients{}
		productCache := make(map[string]*yazio.Product, len(productEntries))
		for _, item := range productEntries {
			if _, ok := productCache[item.ProductID]; !ok {
				p, err := client.GetProduct(ctx, item.ProductID)
				if err != nil {
					return nil, GetDailySummaryOutput{}, friendlyYazioError(
						fmt.Sprintf("get_daily_summary: fetching product %s", item.ProductID), in.User, err)
				}
				productCache[item.ProductID] = p
			}
			p := productCache[item.ProductID]
			for k, perGram := range p.Nutrients {
				consumed[k] += perGram * item.Amount
			}
		}

		// Same deduplication for recipes.
		recipeCache := make(map[string]*yazio.Recipe, len(recipePortions))
		for _, portion := range recipePortions {
			if _, ok := recipeCache[portion.RecipeID]; !ok {
				r, err := client.GetRecipe(ctx, portion.RecipeID)
				if err != nil {
					return nil, GetDailySummaryOutput{}, friendlyYazioError(
						fmt.Sprintf("get_daily_summary: fetching recipe %s", portion.RecipeID), in.User, err)
				}
				recipeCache[portion.RecipeID] = r
			}
			r := recipeCache[portion.RecipeID]
			// Recipe.Nutrients holds whole-recipe totals; scale by the logged
			// fraction of portions (portionCount / recipe.PortionCount).
			divisor := r.PortionCount
			if divisor <= 0 {
				divisor = 1
			}
			for k, recipeTotal := range r.Nutrients {
				consumed[k] += recipeTotal * portion.PortionCount / divisor
			}
		}

		goals, err := client.GetDailyGoals(ctx, date)
		if err != nil {
			return nil, GetDailySummaryOutput{}, friendlyYazioError("get_daily_summary: fetching goals", in.User, err)
		}

		dateStr := date.Format(dateLayout)
		consumedTotals := NutrientTotals{
			EnergyKcal: consumed.EnergyKcalPerGram(),
			ProteinG:   consumed.ProteinPerGram(),
			FatG:       consumed.FatPerGram(),
			CarbG:      consumed.CarbPerGram(),
		}
		// goals is a Nutrients map whose values are absolute daily targets
		// (same dotted keys, but kcal/g totals, not per-gram rates).
		goalTotals := NutrientTotals{
			EnergyKcal: goals.EnergyKcalPerGram(),
			ProteinG:   goals.ProteinPerGram(),
			FatG:       goals.FatPerGram(),
			CarbG:      goals.CarbPerGram(),
		}
		remaining := NutrientTotals{
			EnergyKcal: goalTotals.EnergyKcal - consumedTotals.EnergyKcal,
			ProteinG:   goalTotals.ProteinG - consumedTotals.ProteinG,
			FatG:       goalTotals.FatG - consumedTotals.FatG,
			CarbG:      goalTotals.CarbG - consumedTotals.CarbG,
		}

		logger.Info("get_daily_summary", "user", in.User, "date", dateStr,
			"products", len(productEntries), "recipe_portions", len(recipePortions),
			"consumed_kcal", consumedTotals.EnergyKcal, "goal_kcal", goalTotals.EnergyKcal)

		out := GetDailySummaryOutput{Date: dateStr, Consumed: consumedTotals, Goals: goalTotals, Remaining: remaining}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatDailySummary(out)}}}, out, nil
	}
}

func formatDailySummary(out GetDailySummaryOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Daily summary for %s\n\n", out.Date)
	fmt.Fprintf(&b, "Consumed:  %.0f kcal | protein %.1fg | fat %.1fg | carbs %.1fg\n",
		out.Consumed.EnergyKcal, out.Consumed.ProteinG, out.Consumed.FatG, out.Consumed.CarbG)
	fmt.Fprintf(&b, "Goals:     %.0f kcal | protein %.1fg | fat %.1fg | carbs %.1fg\n",
		out.Goals.EnergyKcal, out.Goals.ProteinG, out.Goals.FatG, out.Goals.CarbG)
	fmt.Fprintf(&b, "Remaining: %.0f kcal | protein %.1fg | fat %.1fg | carbs %.1fg\n",
		out.Remaining.EnergyKcal, out.Remaining.ProteinG, out.Remaining.FatG, out.Remaining.CarbG)
	return b.String()
}

// --- get_consumed_items_range ---

// RangeConsumedItemInfo is one product diary entry within
// get_consumed_items_range. It carries its own resolved name/producer and
// absolute macros for this specific logged amount (not just a raw
// product_id), so the caller never needs a follow-up get_product call —
// this is the whole reason get_consumed_items_range exists rather than a
// raw-ID-only diary reader (see the package doc comment's Product diary
// flow note for why a separate get_consumed_items was removed).
type RangeConsumedItemInfo struct {
	ID          string  `json:"id"`
	ProductID   string  `json:"product_id"`
	Name        string  `json:"name"`
	Producer    string  `json:"producer,omitempty"`
	Daytime     string  `json:"daytime"`
	AmountGrams float64 `json:"amount_grams"`
	EnergyKcal  float64 `json:"energy_kcal"`
	ProteinG    float64 `json:"protein_g"`
	FatG        float64 `json:"fat_g"`
	CarbG       float64 `json:"carb_g"`
}

// RangeRecipePortionInfo is one recipe-portion diary entry within
// get_consumed_items_range, resolved the same way: name plus absolute
// macros for the logged portion fraction (recipe total * portions /
// recipe.PortionCount), not just the raw portion_count.
type RangeRecipePortionInfo struct {
	EntryID      string  `json:"entry_id"`
	RecipeID     string  `json:"recipe_id"`
	Name         string  `json:"name"`
	Daytime      string  `json:"daytime"`
	PortionCount float64 `json:"portion_count"`
	EnergyKcal   float64 `json:"energy_kcal"`
	ProteinG     float64 `json:"protein_g"`
	FatG         float64 `json:"fat_g"`
	CarbG        float64 `json:"carb_g"`
}

// DayEntries is one calendar day's diary within a get_consumed_items_range
// response. One entry is returned per day in [start_date, end_date]
// inclusive, even for days with nothing logged (empty Items/RecipePortions,
// zero Totals), so a weekly view can show gaps rather than silently
// omitting them.
type DayEntries struct {
	Date           string                   `json:"date"`
	Items          []RangeConsumedItemInfo  `json:"items"`
	RecipePortions []RangeRecipePortionInfo `json:"recipe_portions"`
	Totals         NutrientTotals           `json:"totals"`
}

type GetConsumedItemsRangeInput struct {
	User      string `json:"user" jsonschema:"Which configured YAZIO account's diary to read."`
	StartDate string `json:"start_date,omitempty" jsonschema:"First date of the range, YYYY-MM-DD, inclusive. If both start_date and end_date are omitted, defaults to today (a single-day range). If given without end_date, end_date defaults to start_date (also a single day)."`
	EndDate   string `json:"end_date,omitempty" jsonschema:"Last date of the range, YYYY-MM-DD, inclusive. Defaults to start_date if omitted. Must not be given without start_date. The range is capped at 31 days total."`
}

type GetConsumedItemsRangeOutput struct {
	StartDate string         `json:"start_date"`
	EndDate   string         `json:"end_date"`
	Days      []DayEntries   `json:"days"`
	Total     NutrientTotals `json:"total"`
}

func getConsumedItemsRangeHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[GetConsumedItemsRangeInput, GetConsumedItemsRangeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetConsumedItemsRangeInput) (*mcp.CallToolResult, GetConsumedItemsRangeOutput, error) {
		client, err := resolveClient(clients, users, "get_consumed_items_range", in.User)
		if err != nil {
			return nil, GetConsumedItemsRangeOutput{}, err
		}

		start, end, err := parseDateRange(in.StartDate, in.EndDate)
		if err != nil {
			return nil, GetConsumedItemsRangeOutput{}, fmt.Errorf("get_consumed_items_range: %w", err)
		}

		// Caches scoped across the WHOLE range (built once, outside the day
		// loop) — the same product/recipe (e.g. a repeated breakfast) often
		// appears across several days of a week, and re-fetching it once
		// per day would defeat half the point of this tool. This
		// generalizes get_daily_summary's per-call productCache/recipeCache
		// from single-day to whole-range scope.
		productCache := make(map[string]*yazio.Product)
		recipeCache := make(map[string]*yazio.Recipe)

		var days []DayEntries
		grandTotal := yazio.Nutrients{}

		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			dateStr := d.Format(dateLayout)

			productEntries, recipePortions, err := client.GetConsumedItems(ctx, d)
			if err != nil {
				return nil, GetConsumedItemsRangeOutput{}, friendlyYazioError(
					fmt.Sprintf("get_consumed_items_range: fetching diary for %s", dateStr), in.User, err)
			}

			dayTotal := yazio.Nutrients{}
			items := make([]RangeConsumedItemInfo, 0, len(productEntries))
			for _, item := range productEntries {
				p, ok := productCache[item.ProductID]
				if !ok {
					p, err = client.GetProduct(ctx, item.ProductID)
					if err != nil {
						return nil, GetConsumedItemsRangeOutput{}, friendlyYazioError(
							fmt.Sprintf("get_consumed_items_range: fetching product %s", item.ProductID), in.User, err)
					}
					productCache[item.ProductID] = p
				}

				// Per-item absolute macros: per-gram * amount, accumulated
				// into both this day's total and the grand total in the
				// same pass (mirrors get_daily_summary's accumulation loop,
				// but keeps the per-item figure too).
				itemNutrients := yazio.Nutrients{}
				for k, perGram := range p.Nutrients {
					v := perGram * item.Amount
					itemNutrients[k] += v
					dayTotal[k] += v
					grandTotal[k] += v
				}
				items = append(items, RangeConsumedItemInfo{
					ID:          item.ID,
					ProductID:   item.ProductID,
					Name:        p.Name,
					Producer:    p.Producer,
					Daytime:     item.Daytime,
					AmountGrams: item.Amount,
					EnergyKcal:  itemNutrients.EnergyKcalPerGram(),
					ProteinG:    itemNutrients.ProteinPerGram(),
					FatG:        itemNutrients.FatPerGram(),
					CarbG:       itemNutrients.CarbPerGram(),
				})
			}

			portionsOut := make([]RangeRecipePortionInfo, 0, len(recipePortions))
			for _, portion := range recipePortions {
				r, ok := recipeCache[portion.RecipeID]
				if !ok {
					r, err = client.GetRecipe(ctx, portion.RecipeID)
					if err != nil {
						return nil, GetConsumedItemsRangeOutput{}, friendlyYazioError(
							fmt.Sprintf("get_consumed_items_range: fetching recipe %s", portion.RecipeID), in.User, err)
					}
					recipeCache[portion.RecipeID] = r
				}
				// Recipe.Nutrients holds whole-recipe totals; scale by the
				// logged fraction of portions (portionCount / recipe.PortionCount) —
				// same convention as get_daily_summary.
				divisor := r.PortionCount
				if divisor <= 0 {
					divisor = 1
				}
				fraction := portion.PortionCount / divisor

				portionNutrients := yazio.Nutrients{}
				for k, total := range r.Nutrients {
					v := total * fraction
					portionNutrients[k] += v
					dayTotal[k] += v
					grandTotal[k] += v
				}
				portionsOut = append(portionsOut, RangeRecipePortionInfo{
					EntryID:      portion.ID,
					RecipeID:     portion.RecipeID,
					Name:         r.Name,
					Daytime:      portion.Daytime,
					PortionCount: portion.PortionCount,
					EnergyKcal:   portionNutrients.EnergyKcalPerGram(),
					ProteinG:     portionNutrients.ProteinPerGram(),
					FatG:         portionNutrients.FatPerGram(),
					CarbG:        portionNutrients.CarbPerGram(),
				})
			}

			days = append(days, DayEntries{
				Date:           dateStr,
				Items:          items,
				RecipePortions: portionsOut,
				Totals: NutrientTotals{
					EnergyKcal: dayTotal.EnergyKcalPerGram(),
					ProteinG:   dayTotal.ProteinPerGram(),
					FatG:       dayTotal.FatPerGram(),
					CarbG:      dayTotal.CarbPerGram(),
				},
			})
		}

		out := GetConsumedItemsRangeOutput{
			StartDate: start.Format(dateLayout),
			EndDate:   end.Format(dateLayout),
			Days:      days,
			Total: NutrientTotals{
				EnergyKcal: grandTotal.EnergyKcalPerGram(),
				ProteinG:   grandTotal.ProteinPerGram(),
				FatG:       grandTotal.FatPerGram(),
				CarbG:      grandTotal.CarbPerGram(),
			},
		}

		logger.Info("get_consumed_items_range", "user", in.User, "start_date", out.StartDate, "end_date", out.EndDate,
			"days", len(days), "products_fetched", len(productCache), "recipes_fetched", len(recipeCache),
			"total_kcal", out.Total.EnergyKcal)

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatConsumedItemsRange(out)}}}, out, nil
	}
}

func formatConsumedItemsRange(out GetConsumedItemsRangeOutput) string {
	var b strings.Builder
	if out.StartDate == out.EndDate {
		fmt.Fprintf(&b, "Diary for %s\n\n", out.StartDate)
	} else {
		fmt.Fprintf(&b, "Diary from %s to %s\n\n", out.StartDate, out.EndDate)
	}
	for _, day := range out.Days {
		fmt.Fprintf(&b, "--- %s ---\n", day.Date)
		if len(day.Items) == 0 && len(day.RecipePortions) == 0 {
			b.WriteString("  (no entries)\n")
		}
		for _, it := range day.Items {
			fmt.Fprintf(&b, "  - %s", it.Name)
			if it.Producer != "" {
				fmt.Fprintf(&b, " (%s)", it.Producer)
			}
			fmt.Fprintf(&b, ": %gg, %s — %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb  [id: %s]\n",
				it.AmountGrams, it.Daytime, it.EnergyKcal, it.ProteinG, it.FatG, it.CarbG, it.ID)
		}
		for _, p := range day.RecipePortions {
			fmt.Fprintf(&b, "  - %s [recipe]: %g portion(s), %s — %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb  [entry_id: %s]\n",
				p.Name, p.PortionCount, p.Daytime, p.EnergyKcal, p.ProteinG, p.FatG, p.CarbG, p.EntryID)
		}
		fmt.Fprintf(&b, "  day total: %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb\n\n",
			day.Totals.EnergyKcal, day.Totals.ProteinG, day.Totals.FatG, day.Totals.CarbG)
	}
	fmt.Fprintf(&b, "Range total: %.0f kcal, %.1fg protein, %.1fg fat, %.1fg carb\n",
		out.Total.EnergyKcal, out.Total.ProteinG, out.Total.FatG, out.Total.CarbG)
	return b.String()
}
