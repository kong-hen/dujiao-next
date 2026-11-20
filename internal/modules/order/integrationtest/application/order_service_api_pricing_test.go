package application_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	memberlevelapp "github.com/dujiao-next/internal/modules/memberlevel/application"
	memberleveldomain "github.com/dujiao-next/internal/modules/memberlevel/domain"
	memberlevelgormstore "github.com/dujiao-next/internal/modules/memberlevel/infrastructure/gormstore"
	. "github.com/dujiao-next/internal/modules/order/application"
	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"
	promotiongormstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func openAPIPricingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:api_order_pricing_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&categorydomain.Category{},
		&productdomain.Product{},
		&productdomain.ProductSKU{},
		&userdomain.User{},
		&memberleveldomain.MemberLevel{},
		&memberleveldomain.MemberLevelPrice{},
		&memberleveldomain.UserProductPrice{},
		&promotiondomain.Promotion{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}
	return db
}

// 验证 API 订单按 SKU 粒度生效代理价：
// 1. 设了代理价的 SKU 跳过促销/批发/会员等级价，仅与更低的用户专属价比较；
// 2. 未设代理价的 SKU 仍完整享受原项目的会员等级价/批发价/促销价与用户专属价。
func TestPreviewOrderAPIPricingBySKUAgencyAndUserPrice(t *testing.T) {
	db := openAPIPricingTestDB(t)
	now := time.Now()

	category := categorydomain.Category{Slug: "cat-api", NameJSON: jsonmap.JSON{"zh-CN": "测试分类"}, CreatedAt: now}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	// 8 折会员等级
	level := memberleveldomain.MemberLevel{
		NameJSON:     jsonmap.JSON{"zh-CN": "八折会员"},
		Slug:         "vip-80",
		DiscountRate: money.FromDecimal(decimal.NewFromInt(80)),
		IsActive:     true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := db.Create(&level).Error; err != nil {
		t.Fatalf("create level failed: %v", err)
	}

	userStandard := userdomain.User{
		Email:         "api-standard@example.com",
		PasswordHash:  "hash",
		Status:        constants.UserStatusActive,
		MemberLevelID: level.ID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	userWithCheaperExclusive := userdomain.User{
		Email:         "api-cheaper@example.com",
		PasswordHash:  "hash",
		Status:        constants.UserStatusActive,
		MemberLevelID: level.ID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	userWithHigherExclusive := userdomain.User{
		Email:         "api-higher@example.com",
		PasswordHash:  "hash",
		Status:        constants.UserStatusActive,
		MemberLevelID: level.ID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, u := range []*userdomain.User{&userStandard, &userWithCheaperExclusive, &userWithHigherExclusive} {
		if err := db.Create(u).Error; err != nil {
			t.Fatalf("create user failed: %v", err)
		}
	}

	// 商品 A：SKU1 设了代理价 70，SKU2 未设代理价（0）；带批发价（满 2 件 85）与促销价（立减 10 -> 90）
	product := productdomain.Product{
		CategoryID:        category.ID,
		Slug:              "prod-api-mixed",
		TitleJSON:         jsonmap.JSON{"zh-CN": "API混合测试商品"},
		PriceAmount:       money.FromDecimal(decimal.NewFromInt(100)),
		AgencyPriceAmount: money.FromDecimal(decimal.Zero),
		WholesalePrices: productdomain.WholesalePriceTiers{
			{MinQuantity: 2, UnitPrice: money.FromDecimal(decimal.NewFromInt(85))},
		},
		PurchaseType:     constants.ProductPurchaseMember,
		StockDisplayMode: constants.ProductStockDisplayStatus,
		FulfillmentType:  constants.FulfillmentTypeManual,
		IsActive:         true,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Create(&product).Error; err != nil {
		t.Fatalf("create product failed: %v", err)
	}
	skuWithAgency := productdomain.ProductSKU{
		ProductID:         product.ID,
		SKUCode:           "WITH-AGENCY",
		PriceAmount:       money.FromDecimal(decimal.NewFromInt(100)),
		AgencyPriceAmount: money.FromDecimal(decimal.NewFromInt(70)),
		ManualStockTotal:  20,
		IsActive:          true,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	skuWithoutAgency := productdomain.ProductSKU{
		ProductID:         product.ID,
		SKUCode:           "NO-AGENCY",
		PriceAmount:       money.FromDecimal(decimal.NewFromInt(100)),
		AgencyPriceAmount: money.FromDecimal(decimal.Zero),
		ManualStockTotal:  20,
		IsActive:          true,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := db.Create(&skuWithAgency).Error; err != nil {
		t.Fatalf("create skuWithAgency failed: %v", err)
	}
	if err := db.Create(&skuWithoutAgency).Error; err != nil {
		t.Fatalf("create skuWithoutAgency failed: %v", err)
	}

	// 给商品配置促销（立减 10 元，原价 100 -> 促销价 90）
	promo := promotiondomain.Promotion{
		Name:       "立减10",
		ScopeType:  constants.ScopeTypeProduct,
		ScopeRefID: product.ID,
		Type:       constants.PromotionTypeFixed,
		Value:      money.FromDecimal(decimal.NewFromInt(10)),
		IsActive:   true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.Create(&promo).Error; err != nil {
		t.Fatalf("create promotion failed: %v", err)
	}

	// 用户专属价：cheaper 为 60（低于代理价 70），higher 为 75（高于代理价 70）
	for _, up := range []memberleveldomain.UserProductPrice{
		{UserID: userWithCheaperExclusive.ID, ProductID: product.ID, SKUID: skuWithAgency.ID, PriceAmount: money.FromDecimal(decimal.NewFromInt(60))},
		{UserID: userWithHigherExclusive.ID, ProductID: product.ID, SKUID: skuWithAgency.ID, PriceAmount: money.FromDecimal(decimal.NewFromInt(75))},
	} {
		if err := db.Create(&up).Error; err != nil {
			t.Fatalf("create user product price failed: %v", err)
		}
	}

	memberSvc := memberlevelapp.NewService(
		memberlevelgormstore.NewLevelStore(db),
		memberlevelgormstore.NewPriceStore(db),
		memberlevelgormstore.NewUserStore(db),
		memberlevelgormstore.NewUserPriceStore(db),
	)
	svc := NewOrderService(OrderServiceOptions{
		UserStore:          userstore.New(db),
		ProductStore:       productgormstore.NewProductStore(db),
		ProductSKUStore:    productgormstore.NewSKUStore(db),
		PromotionRepo:      promotiongormstore.New(db),
		MemberLevelService: memberSvc,
		ExpireMinutes:      15,
	})

	// 1. 有代理价的 SKU：API 订单买 2 件应直接按代理价 70×2=140 结算（不叠加促销/批发/会员 8 折）
	previewAgency, err := svc.PreviewOrder(CreateOrderInput{
		UserID:     userStandard.ID,
		IsApiOrder: true,
		Items:      []CreateOrderItem{{ProductID: product.ID, SKUID: skuWithAgency.ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("preview api order with agency failed: %v", err)
	}
	if !previewAgency.TotalAmount.Decimal.Equal(decimal.NewFromInt(140)) {
		t.Fatalf("expected api agency total 140, got %s", previewAgency.TotalAmount)
	}

	// 2. 有代理价(70) + 更低专属价(60)：API 订单取更低者 60×2=120
	previewCheaper, err := svc.PreviewOrder(CreateOrderInput{
		UserID:     userWithCheaperExclusive.ID,
		IsApiOrder: true,
		Items:      []CreateOrderItem{{ProductID: product.ID, SKUID: skuWithAgency.ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("preview api order with cheaper exclusive failed: %v", err)
	}
	if !previewCheaper.TotalAmount.Decimal.Equal(decimal.NewFromInt(120)) {
		t.Fatalf("expected api cheaper exclusive total 120, got %s", previewCheaper.TotalAmount)
	}

	// 3. 有代理价(70) + 更高专属价(75)：API 订单仍取更低的代理价 70×2=140
	previewHigher, err := svc.PreviewOrder(CreateOrderInput{
		UserID:     userWithHigherExclusive.ID,
		IsApiOrder: true,
		Items:      []CreateOrderItem{{ProductID: product.ID, SKUID: skuWithAgency.ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("preview api order with higher exclusive failed: %v", err)
	}
	if !previewHigher.TotalAmount.Decimal.Equal(decimal.NewFromInt(140)) {
		t.Fatalf("expected api agency total 140 when exclusive is higher, got %s", previewHigher.TotalAmount)
	}

	// 4. 未设代理价的 SKU：API 订单买 1 件应走促销价 90 + 会员 8 折 = 72，与原项目行为一致
	previewNoAgencyOne, err := svc.PreviewOrder(CreateOrderInput{
		UserID:     userStandard.ID,
		IsApiOrder: true,
		Items:      []CreateOrderItem{{ProductID: product.ID, SKUID: skuWithoutAgency.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("preview api order without agency (qty=1) failed: %v", err)
	}
	if !previewNoAgencyOne.TotalAmount.Decimal.Equal(decimal.NewFromInt(72)) {
		t.Fatalf("expected api order without agency (qty=1) total 72 (promo 90 * 0.8), got %s", previewNoAgencyOne.TotalAmount)
	}

	// 5. 未设代理价的 SKU：API 订单买 2 件应命中更优的批发价 85 + 会员 8 折 = 68×2=136
	previewNoAgencyTwo, err := svc.PreviewOrder(CreateOrderInput{
		UserID:     userStandard.ID,
		IsApiOrder: true,
		Items:      []CreateOrderItem{{ProductID: product.ID, SKUID: skuWithoutAgency.ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("preview api order without agency (qty=2) failed: %v", err)
	}
	if !previewNoAgencyTwo.TotalAmount.Decimal.Equal(decimal.NewFromInt(136)) {
		t.Fatalf("expected api order without agency (qty=2) total 136 (wholesale 85 * 0.8 * 2), got %s", previewNoAgencyTwo.TotalAmount)
	}
}
