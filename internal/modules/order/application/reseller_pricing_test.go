package application

import (
	"errors"
	"fmt"
	"testing"

	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	resellerdomain "github.com/dujiao-next/internal/modules/reseller/domain"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

type resellerPricingRepoStub struct {
	profile         *resellerdomain.Profile
	settings        []resellerdomain.ProductSetting
	related         map[uint]bool
	profileQueries  int
	settingsQueries int
	relatedQueries  int
}

func (r *resellerPricingRepoStub) GetProfileByID(id uint) (*resellerdomain.Profile, error) {
	r.profileQueries++
	if r.profile == nil || r.profile.ID != id {
		return nil, nil
	}
	profile := *r.profile
	return &profile, nil
}

func (r *resellerPricingRepoStub) ListProductSettingsForPricing(resellerID uint, productIDs []uint, skuIDs []uint) ([]resellerdomain.ProductSetting, error) {
	r.settingsQueries++
	rows := make([]resellerdomain.ProductSetting, len(r.settings))
	copy(rows, r.settings)
	return rows, nil
}

func (r *resellerPricingRepoStub) IsActiveRelatedAccount(resellerID uint, userID uint) (bool, error) {
	r.relatedQueries++
	return r.related[userID], nil
}

func testResellerProfile() *resellerdomain.Profile {
	return &resellerdomain.Profile{
		ID:                   10,
		UserID:               99,
		Status:               resellerdomain.ProfileStatusActive,
		DefaultMarkupPercent: money.FromDecimal(decimal.NewFromInt(20)),
		MaxMarkupPercent:     money.FromDecimal(decimal.NewFromInt(50)),
	}
}

func testResellerTenant() resellercontract.TenantContext {
	return resellercontract.ResellerTenantContext("alias.example.test", 10, 88, "primary.example.test")
}

func testOrderBuildResult(items ...struct {
	productID uint
	skuID     uint
	base      decimal.Decimal
	cost      decimal.Decimal
	quantity  int
}) *orderBuildResult {
	plans := make([]childOrderPlan, 0, len(items))
	for _, item := range items {
		product := &productdomain.Product{ID: item.productID, TitleJSON: jsonmap.JSON{"zh-CN": fmt.Sprintf("p%d", item.productID)}}
		sku := &productdomain.ProductSKU{
			ID:              item.skuID,
			ProductID:       item.productID,
			PriceAmount:     money.FromDecimal(item.base),
			CostPriceAmount: money.FromDecimal(item.cost),
			IsActive:        true,
		}
		baseTotal := item.base.Mul(decimal.NewFromInt(int64(item.quantity))).Round(2)
		orderItem := orderdomain.OrderItem{
			ProductID:          item.productID,
			SKUID:              item.skuID,
			TitleJSON:          product.TitleJSON,
			SKUSnapshotJSON:    jsonmap.JSON{"sku_id": item.skuID},
			OriginalUnitPrice:  money.FromDecimal(item.base),
			UnitPrice:          money.FromDecimal(item.base),
			CostPrice:          money.FromDecimal(item.cost),
			Quantity:           item.quantity,
			OriginalTotalPrice: money.FromDecimal(baseTotal),
			TotalPrice:         money.FromDecimal(baseTotal),
			FulfillmentType:    constants.FulfillmentTypeManual,
		}
		plans = append(plans, childOrderPlan{
			Product:     product,
			SKU:         sku,
			Item:        orderItem,
			TotalAmount: baseTotal,
			Currency:    "USD",
		})
	}
	total := decimal.Zero
	for _, plan := range plans {
		total = total.Add(plan.TotalAmount).Round(2)
	}
	return &orderBuildResult{
		Plans:          plans,
		OrderItems:     []orderdomain.OrderItem{},
		OriginalAmount: total,
		TotalAmount:    total,
		Currency:       "USD",
	}
}

func TestResellerPricingResolverMainTenantNoop(t *testing.T) {
	repo := &resellerPricingRepoStub{profile: testResellerProfile()}
	resolver := NewResellerPricingResolver(repo)
	result := testOrderBuildResult(struct {
		productID uint
		skuID     uint
		base      decimal.Decimal
		cost      decimal.Decimal
		quantity  int
	}{productID: 1, skuID: 11, base: decimal.NewFromInt(100), cost: decimal.NewFromInt(50), quantity: 1})

	ctx, err := resolver.ApplyToOrderBuildResult(resellercontract.MainTenantContext("main.example.test"), 123, result)
	if err != nil {
		t.Fatalf("ApplyToOrderBuildResult main failed: %v", err)
	}
	if ctx != nil {
		t.Fatalf("main tenant should not produce pricing context: %+v", ctx)
	}
	if repo.profileQueries != 0 || repo.settingsQueries != 0 || repo.relatedQueries != 0 {
		t.Fatalf("main tenant should not query reseller repo, got profile=%d settings=%d related=%d", repo.profileQueries, repo.settingsQueries, repo.relatedQueries)
	}
	if !result.TotalAmount.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("main total changed: %s", result.TotalAmount)
	}
}

func TestResellerPricingResolverAppliesPriorityAndDefaultMarkup(t *testing.T) {
	repo := &resellerPricingRepoStub{
		profile: testResellerProfile(),
		settings: []resellerdomain.ProductSetting{
			{
				ID:               1,
				ResellerID:       10,
				ProductID:        1,
				SKUID:            0,
				IsListed:         true,
				PricingMode:      resellerdomain.PricingModeMarkupPercent,
				MarkupPercent:    money.FromDecimal(decimal.NewFromInt(10)),
				FixedPriceAmount: money.FromDecimal(decimal.Zero),
			},
			{
				ID:               2,
				ResellerID:       10,
				ProductID:        1,
				SKUID:            11,
				IsListed:         true,
				PricingMode:      resellerdomain.PricingModeFixedPrice,
				FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(130)),
			},
			{
				ID:                3,
				ResellerID:        10,
				ProductID:         2,
				SKUID:             22,
				IsListed:          true,
				PricingMode:       resellerdomain.PricingModeFixedMarkup,
				FixedMarkupAmount: money.FromDecimal(decimal.NewFromInt(25)),
			},
		},
		related: map[uint]bool{},
	}
	resolver := NewResellerPricingResolver(repo)
	result := testOrderBuildResult(
		struct {
			productID uint
			skuID     uint
			base      decimal.Decimal
			cost      decimal.Decimal
			quantity  int
		}{productID: 1, skuID: 11, base: decimal.NewFromInt(100), cost: decimal.NewFromInt(50), quantity: 1},
		struct {
			productID uint
			skuID     uint
			base      decimal.Decimal
			cost      decimal.Decimal
			quantity  int
		}{productID: 2, skuID: 22, base: decimal.NewFromInt(80), cost: decimal.NewFromInt(40), quantity: 2},
		struct {
			productID uint
			skuID     uint
			base      decimal.Decimal
			cost      decimal.Decimal
			quantity  int
		}{productID: 3, skuID: 33, base: decimal.NewFromInt(50), cost: decimal.NewFromInt(25), quantity: 1},
	)

	ctx, err := resolver.ApplyToOrderBuildResult(testResellerTenant(), 123, result)
	if err != nil {
		t.Fatalf("ApplyToOrderBuildResult reseller failed: %v", err)
	}
	if ctx == nil {
		t.Fatal("expected reseller pricing context")
	}
	if repo.settingsQueries != 1 {
		t.Fatalf("expected one settings query, got %d", repo.settingsQueries)
	}
	if ctx.ResellerUserID != 99 {
		t.Fatalf("snapshot should use fresh profile user id, got %d", ctx.ResellerUserID)
	}
	if ctx.Domain != "primary.example.test" {
		t.Fatalf("expected primary domain snapshot, got %q", ctx.Domain)
	}
	if !result.Plans[0].TotalAmount.Equal(decimal.NewFromInt(130)) {
		t.Fatalf("fixed price plan total mismatch: %s", result.Plans[0].TotalAmount)
	}
	if !result.Plans[1].TotalAmount.Equal(decimal.NewFromInt(210)) {
		t.Fatalf("fixed markup plan total mismatch: %s", result.Plans[1].TotalAmount)
	}
	if !result.Plans[2].TotalAmount.Equal(decimal.NewFromInt(60)) {
		t.Fatalf("profile default markup plan total mismatch: %s", result.Plans[2].TotalAmount)
	}
	if !result.TotalAmount.Equal(decimal.NewFromInt(400)) || !result.OriginalAmount.Equal(decimal.NewFromInt(400)) {
		t.Fatalf("parent totals must be derived from rewritten plan totals, got original=%s total=%s", result.OriginalAmount, result.TotalAmount)
	}
	if !ctx.BaseAmount.Equal(decimal.NewFromInt(310)) || !ctx.ResellerAmount.Equal(decimal.NewFromInt(400)) || !ctx.ProfitAmount.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("snapshot totals mismatch base=%s reseller=%s profit=%s", ctx.BaseAmount, ctx.ResellerAmount, ctx.ProfitAmount)
	}
	if !ctx.EffectiveProfit.Equal(decimal.NewFromInt(90)) || !ctx.ProfitEligible {
		t.Fatalf("expected eligible effective profit 90, got eligible=%t effective=%s", ctx.ProfitEligible, ctx.EffectiveProfit)
	}
}

func TestResellerPricingResolverRuntimePrioritySnapshotSources(t *testing.T) {
	repo := &resellerPricingRepoStub{
		profile: testResellerProfile(),
		settings: []resellerdomain.ProductSetting{
			{
				ID:            1,
				ResellerID:    10,
				ProductID:     1,
				SKUID:         0,
				IsListed:      true,
				PricingMode:   resellerdomain.PricingModeMarkupPercent,
				MarkupPercent: money.FromDecimal(decimal.NewFromInt(10)),
			},
			{
				ID:               2,
				ResellerID:       10,
				ProductID:        1,
				SKUID:            11,
				IsListed:         true,
				PricingMode:      resellerdomain.PricingModeFixedPrice,
				FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(130)),
			},
			{
				ID:                3,
				ResellerID:        10,
				ProductID:         2,
				SKUID:             0,
				IsListed:          true,
				PricingMode:       resellerdomain.PricingModeFixedMarkup,
				FixedMarkupAmount: money.FromDecimal(decimal.NewFromInt(25)),
			},
		},
		related: map[uint]bool{},
	}
	resolver := NewResellerPricingResolver(repo)
	result := testOrderBuildResult(
		struct {
			productID uint
			skuID     uint
			base      decimal.Decimal
			cost      decimal.Decimal
			quantity  int
		}{productID: 1, skuID: 11, base: decimal.NewFromInt(100), cost: decimal.NewFromInt(50), quantity: 1},
		struct {
			productID uint
			skuID     uint
			base      decimal.Decimal
			cost      decimal.Decimal
			quantity  int
		}{productID: 2, skuID: 22, base: decimal.NewFromInt(80), cost: decimal.NewFromInt(40), quantity: 2},
		struct {
			productID uint
			skuID     uint
			base      decimal.Decimal
			cost      decimal.Decimal
			quantity  int
		}{productID: 3, skuID: 33, base: decimal.NewFromInt(50), cost: decimal.NewFromInt(25), quantity: 1},
	)

	ctx, err := resolver.ApplyToOrderBuildResult(testResellerTenant(), 123, result)
	if err != nil {
		t.Fatalf("ApplyToOrderBuildResult failed: %v", err)
	}
	if len(ctx.Items) != 3 {
		t.Fatalf("expected 3 pricing items, got %d", len(ctx.Items))
	}
	assertRuntimePricingItem := func(index int, source string, mode string, unit string, profit string) {
		t.Helper()
		item := ctx.Items[index]
		if item.RuleSource != source || item.PricingMode != mode {
			t.Fatalf("item %d source/mode mismatch: %+v", index, item)
		}
		if item.ResellerUnitAmount.StringFixed(2) != unit {
			t.Fatalf("item %d reseller unit want %s got %s", index, unit, item.ResellerUnitAmount.StringFixed(2))
		}
		if item.ProfitAmount.StringFixed(2) != profit {
			t.Fatalf("item %d profit want %s got %s", index, profit, item.ProfitAmount.StringFixed(2))
		}
	}
	assertRuntimePricingItem(0, resellerRuleSourceSKU, resellerdomain.PricingModeFixedPrice, "130.00", "30.00")
	assertRuntimePricingItem(1, resellerRuleSourceProduct, resellerdomain.PricingModeFixedMarkup, "105.00", "50.00")
	assertRuntimePricingItem(2, resellerRuleSourceProfile, resellerdomain.PricingModeMarkupPercent, "60.00", "10.00")

	if ctx.BaseAmount.StringFixed(2) != "310.00" || ctx.ResellerAmount.StringFixed(2) != "400.00" || ctx.ProfitAmount.StringFixed(2) != "90.00" {
		t.Fatalf("context totals mismatch base=%s reseller=%s profit=%s", ctx.BaseAmount, ctx.ResellerAmount, ctx.ProfitAmount)
	}
	items, ok := ctx.PricingSnapshot["items"].([]interface{})
	if !ok || len(items) != 3 {
		t.Fatalf("pricing snapshot items mismatch: %#v", ctx.PricingSnapshot["items"])
	}
	first, ok := items[0].(jsonmap.JSON)
	if !ok {
		t.Fatalf("pricing snapshot item type mismatch: %#v", items[0])
	}
	if first["rule_source"] != resellerRuleSourceSKU || first["pricing_mode"] != resellerdomain.PricingModeFixedPrice {
		t.Fatalf("pricing snapshot should record sku rule source and mode: %#v", first)
	}
}

func TestResellerPricingResolverBlocksHiddenProductAndSKU(t *testing.T) {
	tests := []struct {
		name    string
		setting resellerdomain.ProductSetting
	}{
		{
			name: "product",
			setting: resellerdomain.ProductSetting{
				ID:          1,
				ResellerID:  10,
				ProductID:   1,
				SKUID:       0,
				IsListed:    false,
				PricingMode: resellerdomain.PricingModeInherit,
			},
		},
		{
			name: "sku",
			setting: resellerdomain.ProductSetting{
				ID:          2,
				ResellerID:  10,
				ProductID:   1,
				SKUID:       11,
				IsListed:    false,
				PricingMode: resellerdomain.PricingModeInherit,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &resellerPricingRepoStub{profile: testResellerProfile(), settings: []resellerdomain.ProductSetting{tt.setting}}
			resolver := NewResellerPricingResolver(repo)
			result := testOrderBuildResult(struct {
				productID uint
				skuID     uint
				base      decimal.Decimal
				cost      decimal.Decimal
				quantity  int
			}{productID: 1, skuID: 11, base: decimal.NewFromInt(100), cost: decimal.NewFromInt(50), quantity: 1})

			_, err := resolver.ApplyToOrderBuildResult(testResellerTenant(), 123, result)
			if !errors.Is(err, ErrResellerProductNotListed) {
				t.Fatalf("expected ErrResellerProductNotListed, got %v", err)
			}
		})
	}
}

func TestResellerPricingResolverValidatesPriceRules(t *testing.T) {
	tests := []struct {
		name    string
		profile *resellerdomain.Profile
		setting resellerdomain.ProductSetting
		wantErr error
	}{
		{
			name:    "fixed price below base",
			profile: testResellerProfile(),
			setting: resellerdomain.ProductSetting{
				ID:               1,
				ResellerID:       10,
				ProductID:        1,
				SKUID:            11,
				IsListed:         true,
				PricingMode:      resellerdomain.PricingModeFixedPrice,
				FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(99)),
			},
			wantErr: ErrResellerPriceBelowBase,
		},
		{
			name:    "fixed markup below zero",
			profile: testResellerProfile(),
			setting: resellerdomain.ProductSetting{
				ID:                2,
				ResellerID:        10,
				ProductID:         1,
				SKUID:             11,
				IsListed:          true,
				PricingMode:       resellerdomain.PricingModeFixedMarkup,
				FixedMarkupAmount: money.FromDecimal(decimal.NewFromInt(-1)),
			},
			wantErr: ErrResellerPriceBelowBase,
		},
		{
			name:    "percent exceeds max",
			profile: testResellerProfile(),
			setting: resellerdomain.ProductSetting{
				ID:            3,
				ResellerID:    10,
				ProductID:     1,
				SKUID:         11,
				IsListed:      true,
				PricingMode:   resellerdomain.PricingModeMarkupPercent,
				MarkupPercent: money.FromDecimal(decimal.NewFromInt(60)),
			},
			wantErr: ErrResellerMarkupExceeded,
		},
		{
			name:    "fixed price implicit percent exceeds max",
			profile: testResellerProfile(),
			setting: resellerdomain.ProductSetting{
				ID:               4,
				ResellerID:       10,
				ProductID:        1,
				SKUID:            11,
				IsListed:         true,
				PricingMode:      resellerdomain.PricingModeFixedPrice,
				FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(151)),
			},
			wantErr: ErrResellerMarkupExceeded,
		},
		{
			name:    "fixed markup implicit percent exceeds max",
			profile: testResellerProfile(),
			setting: resellerdomain.ProductSetting{
				ID:                5,
				ResellerID:        10,
				ProductID:         1,
				SKUID:             11,
				IsListed:          true,
				PricingMode:       resellerdomain.PricingModeFixedMarkup,
				FixedMarkupAmount: money.FromDecimal(decimal.NewFromInt(51)),
			},
			wantErr: ErrResellerMarkupExceeded,
		},
		{
			name:    "unknown mode",
			profile: testResellerProfile(),
			setting: resellerdomain.ProductSetting{
				ID:          6,
				ResellerID:  10,
				ProductID:   1,
				SKUID:       11,
				IsListed:    true,
				PricingMode: "surprise",
			},
			wantErr: ErrResellerPricingModeInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &resellerPricingRepoStub{profile: tt.profile, settings: []resellerdomain.ProductSetting{tt.setting}}
			resolver := NewResellerPricingResolver(repo)
			result := testOrderBuildResult(struct {
				productID uint
				skuID     uint
				base      decimal.Decimal
				cost      decimal.Decimal
				quantity  int
			}{productID: 1, skuID: 11, base: decimal.NewFromInt(100), cost: decimal.NewFromInt(50), quantity: 1})

			_, err := resolver.ApplyToOrderBuildResult(testResellerTenant(), 123, result)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestResellerPricingResolverSelfDealingRiskSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		buyerID    uint
		related    map[uint]bool
		wantReason string
		wantElig   bool
	}{
		{name: "owner", buyerID: 99, related: map[uint]bool{}, wantReason: "self_dealing_owner", wantElig: false},
		{name: "related", buyerID: 123, related: map[uint]bool{123: true}, wantReason: "self_dealing_related_account", wantElig: false},
		{name: "guest", buyerID: 0, related: map[uint]bool{}, wantReason: "", wantElig: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &resellerPricingRepoStub{profile: testResellerProfile(), related: tt.related}
			resolver := NewResellerPricingResolver(repo)
			result := testOrderBuildResult(struct {
				productID uint
				skuID     uint
				base      decimal.Decimal
				cost      decimal.Decimal
				quantity  int
			}{productID: 1, skuID: 11, base: decimal.NewFromInt(100), cost: decimal.NewFromInt(50), quantity: 1})

			ctx, err := resolver.ApplyToOrderBuildResult(testResellerTenant(), tt.buyerID, result)
			if err != nil {
				t.Fatalf("ApplyToOrderBuildResult failed: %v", err)
			}
			if ctx.ProfitEligible != tt.wantElig || ctx.ProfitBlockReason != tt.wantReason {
				t.Fatalf("risk mismatch eligible=%t reason=%q", ctx.ProfitEligible, ctx.ProfitBlockReason)
			}
			if tt.wantElig && !ctx.EffectiveProfit.Equal(decimal.NewFromInt(20)) {
				t.Fatalf("expected effective profit 20, got %s", ctx.EffectiveProfit)
			}
			if !tt.wantElig && !ctx.EffectiveProfit.Equal(decimal.Zero) {
				t.Fatalf("blocked profit must be zero, got %s", ctx.EffectiveProfit)
			}
			if got := ctx.RiskSnapshot["buyer_user_id"]; got != tt.buyerID {
				t.Fatalf("risk snapshot buyer_user_id mismatch want %d got %#v", tt.buyerID, got)
			}
		})
	}
}

func TestResellerPricingResolverDisplayBatchUsesSingleSettingsLookup(t *testing.T) {
	repo := &resellerPricingRepoStub{
		profile: testResellerProfile(),
		settings: []resellerdomain.ProductSetting{
			{
				ID:               1,
				ResellerID:       10,
				ProductID:        1,
				SKUID:            11,
				IsListed:         true,
				PricingMode:      resellerdomain.PricingModeFixedPrice,
				FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(130)),
			},
			{
				ID:          2,
				ResellerID:  10,
				ProductID:   2,
				SKUID:       22,
				IsListed:    false,
				PricingMode: resellerdomain.PricingModeInherit,
			},
		},
	}
	resolver := NewResellerPricingResolver(repo)
	products := []productdomain.Product{
		{
			ID:          1,
			PriceAmount: money.FromDecimal(decimal.NewFromInt(100)),
			SKUs: []productdomain.ProductSKU{
				{ID: 11, ProductID: 1, PriceAmount: money.FromDecimal(decimal.NewFromInt(100)), CostPriceAmount: money.FromDecimal(decimal.NewFromInt(50)), IsActive: true},
			},
		},
		{
			ID:          2,
			PriceAmount: money.FromDecimal(decimal.NewFromInt(80)),
			SKUs: []productdomain.ProductSKU{
				{ID: 22, ProductID: 2, PriceAmount: money.FromDecimal(decimal.NewFromInt(80)), CostPriceAmount: money.FromDecimal(decimal.NewFromInt(40)), IsActive: true},
			},
		},
	}

	batch, err := resolver.LoadDisplayPricingBatch(testResellerTenant(), products)
	if err != nil {
		t.Fatalf("LoadDisplayPricingBatch failed: %v", err)
	}
	if repo.settingsQueries != 1 {
		t.Fatalf("expected one settings query for page, got %d", repo.settingsQueries)
	}
	first, err := resolver.ResolveDisplayPrices(testResellerTenant(), &products[0], batch)
	if err != nil {
		t.Fatalf("ResolveDisplayPrices first failed: %v", err)
	}
	second, err := resolver.ResolveDisplayPrices(testResellerTenant(), &products[1], batch)
	if err != nil {
		t.Fatalf("ResolveDisplayPrices second failed: %v", err)
	}
	if repo.settingsQueries != 1 {
		t.Fatalf("ResolveDisplayPrices must not query per product, got %d settings queries", repo.settingsQueries)
	}
	if !first.Visible || first.DisplaySKUID != 11 || !first.DisplayPrice.Decimal.Equal(decimal.NewFromInt(130)) {
		t.Fatalf("first display mismatch: %+v", first)
	}
	if second.Visible {
		t.Fatalf("all hidden sku product should not be visible: %+v", second)
	}
}

func TestResellerPricingResolverDisplayHidesInvalidSKUWithoutFailing(t *testing.T) {
	// 模拟保存后失效的脏配置：SKU 12 的固定价低于基准价（如基准价被上调）。
	repo := &resellerPricingRepoStub{
		profile: testResellerProfile(),
		settings: []resellerdomain.ProductSetting{
			{ID: 1, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeFixedPrice, FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(130))},
			{ID: 2, ResellerID: 10, ProductID: 1, SKUID: 12, IsListed: true, PricingMode: resellerdomain.PricingModeFixedPrice, FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(80))},
		},
	}
	resolver := NewResellerPricingResolver(repo)
	products := []productdomain.Product{
		{
			ID:          1,
			PriceAmount: money.FromDecimal(decimal.NewFromInt(100)),
			SKUs: []productdomain.ProductSKU{
				{ID: 11, ProductID: 1, PriceAmount: money.FromDecimal(decimal.NewFromInt(100)), CostPriceAmount: money.FromDecimal(decimal.NewFromInt(50)), IsActive: true},
				{ID: 12, ProductID: 1, PriceAmount: money.FromDecimal(decimal.NewFromInt(100)), CostPriceAmount: money.FromDecimal(decimal.NewFromInt(50)), IsActive: true},
			},
		},
	}
	batch, err := resolver.LoadDisplayPricingBatch(testResellerTenant(), products)
	if err != nil {
		t.Fatalf("LoadDisplayPricingBatch failed: %v", err)
	}
	result, err := resolver.ResolveDisplayPrices(testResellerTenant(), &products[0], batch)
	if err != nil {
		t.Fatalf("ResolveDisplayPrices should degrade gracefully, got error: %v", err)
	}
	if result == nil || !result.Visible {
		t.Fatalf("expected product visible via the valid sku, got %+v", result)
	}
	if !result.HiddenSKUIDs[12] {
		t.Fatalf("expected invalid sku 12 hidden, got %+v", result.HiddenSKUIDs)
	}
	if _, ok := result.SKUPrices[12]; ok {
		t.Fatalf("invalid sku 12 should not carry a price, got %+v", result.SKUPrices)
	}
	if result.DisplaySKUID != 11 || !result.DisplayPrice.Decimal.Equal(decimal.NewFromInt(130)) {
		t.Fatalf("expected display fall back to valid sku 11@130, got %+v", result)
	}
}

// 验证商品配置代理价后，分销加价规则与最大加价比例以主站零售价为基数，代理价仅用于进货成本与利润结算。
func TestResellerPricingResolverAgencyPriceRules(t *testing.T) {
	tests := []struct {
		name           string
		master         string
		agency         string
		defaultMarkup  string
		maxMarkup      string
		setting        *resellerdomain.ProductSetting
		wantUnit       string
		wantBase       string
		wantProfit     string
		wantErr        error
		checkDisplayOK bool
	}{
		{
			name:           "主站100代理70_继承主站价",
			master:         "100.00",
			agency:         "70.00",
			defaultMarkup:  "0.00",
			maxMarkup:      "30.00",
			setting:        &resellerdomain.ProductSetting{ID: 1, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeInherit},
			wantUnit:       "100.00",
			wantBase:       "70.00",
			wantProfit:     "30.00",
			checkDisplayOK: true,
		},
		{
			name:           "主站100代理70_资料默认加价15%",
			master:         "100.00",
			agency:         "70.00",
			defaultMarkup:  "15.00",
			maxMarkup:      "30.00",
			setting:        nil,
			wantUnit:       "115.00",
			wantBase:       "70.00",
			wantProfit:     "45.00",
			checkDisplayOK: true,
		},
		{
			name:           "主站100代理70_百分比加价20%基于主站价不隐藏",
			master:         "100.00",
			agency:         "70.00",
			defaultMarkup:  "0.00",
			maxMarkup:      "30.00",
			setting:        &resellerdomain.ProductSetting{ID: 2, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeMarkupPercent, MarkupPercent: money.FromDecimal(decimal.NewFromInt(20))},
			wantUnit:       "120.00",
			wantBase:       "70.00",
			wantProfit:     "50.00",
			checkDisplayOK: true,
		},
		{
			name:           "主站100代理70_固定加价10基于主站价",
			master:         "100.00",
			agency:         "70.00",
			defaultMarkup:  "0.00",
			maxMarkup:      "30.00",
			setting:        &resellerdomain.ProductSetting{ID: 3, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeFixedMarkup, FixedMarkupAmount: money.FromDecimal(decimal.NewFromInt(10))},
			wantUnit:       "110.00",
			wantBase:       "70.00",
			wantProfit:     "40.00",
			checkDisplayOK: true,
		},
		{
			name:           "主站100代理50_最大加价30%_固定价110通过",
			master:         "100.00",
			agency:         "50.00",
			defaultMarkup:  "0.00",
			maxMarkup:      "30.00",
			setting:        &resellerdomain.ProductSetting{ID: 4, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeFixedPrice, FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(110))},
			wantUnit:       "110.00",
			wantBase:       "50.00",
			wantProfit:     "60.00",
			checkDisplayOK: true,
		},
		{
			name:           "主站100代理50_最大加价30%_固定价130边界通过",
			master:         "100.00",
			agency:         "50.00",
			defaultMarkup:  "0.00",
			maxMarkup:      "30.00",
			setting:        &resellerdomain.ProductSetting{ID: 5, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeFixedPrice, FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(130))},
			wantUnit:       "130.00",
			wantBase:       "50.00",
			wantProfit:     "80.00",
			checkDisplayOK: true,
		},
		{
			name:          "主站100代理50_最大加价30%_固定价131超限被拒",
			master:        "100.00",
			agency:        "50.00",
			defaultMarkup: "0.00",
			maxMarkup:     "30.00",
			setting:       &resellerdomain.ProductSetting{ID: 6, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeFixedPrice, FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(131))},
			wantErr:       resellercontract.ErrMarkupExceeded,
		},
		{
			name:          "主站100代理70_固定价90低于主站价被拒",
			master:        "100.00",
			agency:        "70.00",
			defaultMarkup: "0.00",
			maxMarkup:     "30.00",
			setting:       &resellerdomain.ProductSetting{ID: 7, ResellerID: 10, ProductID: 1, SKUID: 11, IsListed: true, PricingMode: resellerdomain.PricingModeFixedPrice, FixedPriceAmount: money.FromDecimal(decimal.NewFromInt(90))},
			wantErr:       resellercontract.ErrPriceBelowMasterPrice,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profile := &resellerdomain.Profile{
				ID:                   10,
				UserID:               99,
				Status:               resellerdomain.ProfileStatusActive,
				DefaultMarkupPercent: money.FromDecimal(decimal.RequireFromString(tc.defaultMarkup)),
				MaxMarkupPercent:     money.FromDecimal(decimal.RequireFromString(tc.maxMarkup)),
			}
			var settings []resellerdomain.ProductSetting
			if tc.setting != nil {
				settings = []resellerdomain.ProductSetting{*tc.setting}
			}
			repo := &resellerPricingRepoStub{profile: profile, settings: settings}
			resolver := NewResellerPricingResolver(repo)

			masterDec := decimal.RequireFromString(tc.master)
			agencyDec := decimal.RequireFromString(tc.agency)
			result := testOrderBuildResult(struct {
				productID uint
				skuID     uint
				base      decimal.Decimal
				cost      decimal.Decimal
				quantity  int
			}{productID: 1, skuID: 11, base: masterDec, cost: decimal.NewFromInt(40), quantity: 1})
			result.Plans[0].SKU.AgencyPriceAmount = money.FromDecimal(agencyDec)

			ctx, err := resolver.ApplyToOrderBuildResult(testResellerTenant(), 200, result)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyToOrderBuildResult failed: %v", err)
			}
			if ctx.ResellerAmount.StringFixed(2) != tc.wantUnit {
				t.Fatalf("reseller amount = %s, want %s", ctx.ResellerAmount.StringFixed(2), tc.wantUnit)
			}
			if ctx.BaseAmount.StringFixed(2) != tc.wantBase {
				t.Fatalf("base amount = %s, want %s", ctx.BaseAmount.StringFixed(2), tc.wantBase)
			}
			if ctx.ProfitAmount.StringFixed(2) != tc.wantProfit {
				t.Fatalf("profit amount = %s, want %s", ctx.ProfitAmount.StringFixed(2), tc.wantProfit)
			}

			if tc.checkDisplayOK {
				prod := productdomain.Product{
					ID:                1,
					PriceAmount:       money.FromDecimal(masterDec),
					AgencyPriceAmount: money.FromDecimal(agencyDec),
					SKUs: []productdomain.ProductSKU{{
						ID:                11,
						ProductID:         1,
						PriceAmount:       money.FromDecimal(masterDec),
						AgencyPriceAmount: money.FromDecimal(agencyDec),
						CostPriceAmount:   money.FromDecimal(decimal.NewFromInt(40)),
						IsActive:          true,
					}},
				}
				batch, err := resolver.LoadDisplayPricingBatch(testResellerTenant(), []productdomain.Product{prod})
				if err != nil {
					t.Fatalf("LoadDisplayPricingBatch failed: %v", err)
				}
				display, err := resolver.ResolveDisplayPrices(testResellerTenant(), &prod, batch)
				if err != nil {
					t.Fatalf("ResolveDisplayPrices failed: %v", err)
				}
				if !display.Visible || display.HiddenSKUIDs[11] {
					t.Fatalf("expected sku 11 visible, got %+v", display)
				}
				if display.DisplayPrice.Decimal.StringFixed(2) != tc.wantUnit {
					t.Fatalf("display price = %s, want %s", display.DisplayPrice.Decimal.StringFixed(2), tc.wantUnit)
				}
			}
		})
	}
}
