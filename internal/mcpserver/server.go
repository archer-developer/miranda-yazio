// Package mcpserver registers this service's MCP tools and wires them to
// internal/yazio, the YAZIO API client.
//
// The five tools mirror a natural conversation flow: search_products finds
// candidate foods, get_product exposes the serving types a chosen food
// supports (so a caller can convert "2 pieces" or "a cup" into grams
// without the end user ever typing a gram figure), add_consumed_item logs
// the computed gram amount to the diary, get_consumed_items reads today's
// (or any day's) log back, and remove_consumed_item corrects a mistake.
//
// This service is not single-tenant: it can hold a live YazioClient for
// several YAZIO accounts at once (one per configured household member),
// so every tool takes a required "user" parameter selecting which
// account's diary to read or write. New takes a map keyed by that user
// name rather than a single client — see resolveClient.
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
	SearchProducts(ctx context.Context, query string) ([]yazio.Product, error)
	GetProduct(ctx context.Context, id string) (*yazio.Product, error)
	GetConsumedItems(ctx context.Context, date time.Time) ([]yazio.ConsumedItem, error)
	AddConsumedItem(ctx context.Context, productID string, amount float64, mealType string, date time.Time) error
	RemoveConsumedItem(ctx context.Context, itemID string) error
}

// New builds and returns the MCP server with all five YAZIO tools
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
		Name: "get_consumed_items",
		Description: "Get the food diary entries logged for a given date (defaults to today if date is omitted). " +
			"Each entry's \"id\" field is what remove_consumed_item expects — it is not the same as product_id." +
			userHint,
	}, getConsumedItemsHandler(clients, users, logger))

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
		Description: "Delete one diary entry by its item ID, as returned by get_consumed_items. " +
			"Use this to correct a mistaken add_consumed_item call." +
			userHint,
	}, removeConsumedItemHandler(clients, users, logger))

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

// --- get_consumed_items ---

type GetConsumedItemsInput struct {
	User string `json:"user" jsonschema:"Which configured YAZIO account's diary to read."`
	Date string `json:"date,omitempty" jsonschema:"Date to fetch entries for, YYYY-MM-DD. Defaults to today if omitted."`
}

type ConsumedItemInfo struct {
	ID              string  `json:"id"`
	ProductID       string  `json:"product_id"`
	Daytime         string  `json:"daytime"`
	AmountGrams     float64 `json:"amount_grams"`
	Serving         string  `json:"serving,omitempty"`
	ServingQuantity float64 `json:"serving_quantity,omitempty"`
}

type GetConsumedItemsOutput struct {
	Date  string             `json:"date"`
	Items []ConsumedItemInfo `json:"items"`
	Total int                `json:"total"`
}

func getConsumedItemsHandler(clients map[string]YazioClient, users []string, logger *slog.Logger) mcp.ToolHandlerFor[GetConsumedItemsInput, GetConsumedItemsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetConsumedItemsInput) (*mcp.CallToolResult, GetConsumedItemsOutput, error) {
		client, err := resolveClient(clients, users, "get_consumed_items", in.User)
		if err != nil {
			return nil, GetConsumedItemsOutput{}, err
		}

		date, err := parseDateOrToday(in.Date)
		if err != nil {
			return nil, GetConsumedItemsOutput{}, fmt.Errorf("get_consumed_items: %w", err)
		}

		items, err := client.GetConsumedItems(ctx, date)
		if err != nil {
			return nil, GetConsumedItemsOutput{}, friendlyYazioError("get_consumed_items", in.User, err)
		}

		infos := make([]ConsumedItemInfo, len(items))
		for i, it := range items {
			infos[i] = ConsumedItemInfo{
				ID:              it.ID,
				ProductID:       it.ProductID,
				Daytime:         it.Daytime,
				AmountGrams:     it.Amount,
				Serving:         it.Serving,
				ServingQuantity: it.ServingQuantity,
			}
		}

		dateStr := date.Format(dateLayout)
		logger.Info("get_consumed_items", "user", in.User, "date", dateStr, "found", len(infos))

		out := GetConsumedItemsOutput{Date: dateStr, Items: infos, Total: len(infos)}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatConsumedItems(dateStr, infos)}}}, out, nil
	}
}

func formatConsumedItems(date string, items []ConsumedItemInfo) string {
	if len(items) == 0 {
		return fmt.Sprintf("No diary entries for %s.", date)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d entr(y/ies) for %s:\n", len(items), date)
	for _, it := range items {
		fmt.Fprintf(&b, "\n- id: %s\n  product_id: %s\n  meal: %s\n  amount: %gg", it.ID, it.ProductID, it.Daytime, it.AmountGrams)
		if it.Serving != "" {
			fmt.Fprintf(&b, " (%g x %q)", it.ServingQuantity, it.Serving)
		}
		b.WriteString("\n")
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
	ItemID string `json:"item_id" jsonschema:"The diary entry ID to delete, as returned by get_consumed_items's \"id\" field (not product_id)."`
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
