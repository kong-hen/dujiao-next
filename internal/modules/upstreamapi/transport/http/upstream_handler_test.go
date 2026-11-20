package upstreamhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	downstreamcallbackdomain "github.com/dujiao-next/internal/modules/downstreamcallback/domain"
	memberlevelapp "github.com/dujiao-next/internal/modules/memberlevel/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	procurementcontract "github.com/dujiao-next/internal/modules/procurement/contract"
	procurementdomain "github.com/dujiao-next/internal/modules/procurement/domain"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/dujiao-next/internal/shared/money"
	upstreamadapter "github.com/dujiao-next/internal/upstream"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

func TestMapCallbackStatus(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{name: "delivered keep delivered", input: "delivered", expect: "delivered"},
		{name: "completed map delivered", input: "completed", expect: "delivered"},
		{name: "fulfilled map delivered", input: "fulfilled", expect: "delivered"},
		{name: "canceled keep canceled", input: "canceled", expect: "canceled"},
		{name: "cancelled map canceled", input: "cancelled", expect: "canceled"},
		{name: "refunded keep refunded", input: "refunded", expect: "refunded"},
		{name: "partially refunded keep value", input: "partially_refunded", expect: "partially_refunded"},
		{name: "trim and lower", input: "  ReFuNdEd  ", expect: "refunded"},
		{name: "fallback normalized raw", input: "PROCESSING", expect: "processing"},
	}

	for _, tc := range tests {
		got := mapCallbackStatus(tc.input)
		if got != tc.expect {
			t.Fatalf("%s: want %q got %q", tc.name, tc.expect, got)
		}
	}
}

type stubConnections struct {
	conn *siteconnectiondomain.Connection
}

func (s stubConnections) GetByApiKey(string) (*siteconnectiondomain.Connection, error) {
	return s.conn, nil
}

type stubSecrets struct{}

func (stubSecrets) DecryptSecret(encrypted string) (string, error) { return encrypted, nil }

type stubProcurements struct {
	order   *procurementdomain.Order
	handled bool
}

func (s *stubProcurements) GetByLocalOrderNo(string) (*procurementdomain.Order, error) {
	return s.order, nil
}

func (s *stubProcurements) HandleUpstreamCallback(uint, string, *procurementcontract.Fulfillment) error {
	s.handled = true
	return nil
}

// TestHandleCallbackOwnership 保证回调只能由采购单所属连接、且上游订单号一致时才被受理。
func TestHandleCallbackOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name         string
		connectionID uint
		upstreamID   uint
		payloadID    uint
		wantHandled  bool
	}{
		{name: "同连接同上游单号放行", connectionID: 1, upstreamID: 88, payloadID: 88, wantHandled: true},
		{name: "上游单号未落库时只校验连接", connectionID: 1, upstreamID: 0, payloadID: 88, wantHandled: true},
		{name: "其它连接冒用本地订单号", connectionID: 2, upstreamID: 88, payloadID: 88, wantHandled: false},
		{name: "上游订单号不匹配", connectionID: 1, upstreamID: 88, payloadID: 99, wantHandled: false},
	}

	for _, tc := range cases {
		procurements := &stubProcurements{order: &procurementdomain.Order{
			ID:              7,
			ConnectionID:    tc.connectionID,
			LocalOrderNo:    "LOCAL-1",
			UpstreamOrderID: tc.upstreamID,
		}}
		handler := &Handler{Dependencies: Dependencies{
			Connections: stubConnections{conn: &siteconnectiondomain.Connection{
				ID: 1, ApiKey: "key-1", ApiSecret: "secret-1", Status: "active",
			}},
			ConnectionSecrets: stubSecrets{},
			Procurements:      procurements,
		}}

		timestamp := time.Now().Unix()
		body, _ := json.Marshal(callbackPayload{
			Event:             "order.fulfilled",
			OrderID:           tc.payloadID,
			DownstreamOrderNo: "LOCAL-1",
			Status:            "delivered",
			Timestamp:         timestamp,
		})

		request := httptest.NewRequest(http.MethodPost, "/api/v1/upstream/callback", bytes.NewReader(body))
		request.Header.Set(upstreamadapter.HeaderApiKey, "key-1")
		request.Header.Set(upstreamadapter.HeaderTimestamp, fmt.Sprintf("%d", timestamp))
		request.Header.Set(upstreamadapter.HeaderSignature,
			upstreamadapter.Sign("secret-1", http.MethodPost, "/api/v1/upstream/callback", timestamp, body))

		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = request
		handler.HandleCallback(c)

		if procurements.handled != tc.wantHandled {
			t.Fatalf("%s: handled=%v want %v (body %s)", tc.name, procurements.handled, tc.wantHandled, recorder.Body.String())
		}
	}
}

type formTestProducts struct{ product productdomain.Product }

func (s formTestProducts) GetByID(string) (*productdomain.Product, error) {
	p := s.product
	return &p, nil
}

type formTestSKUs struct{}

func (formTestSKUs) GetByID(uint) (*productdomain.ProductSKU, error) {
	return &productdomain.ProductSKU{ID: 19, ProductID: 18, IsActive: true}, nil
}

type formTestOrders struct {
	Orders
	received CreateOrderInput
}

func (s *formTestOrders) CreateOrder(input CreateOrderInput) (*orderdomain.Order, error) {
	s.received = input
	return &orderdomain.Order{ID: 100, OrderNo: "FORM-TEST", Status: constants.OrderStatusPendingPayment}, nil
}

type formTestPayments struct{}

func (formTestPayments) CreatePayment(CreatePaymentInput) (*CreatePaymentResult, error) {
	return &CreatePaymentResult{OrderPaid: true}, nil
}

type formTestRefs struct{}

func (formTestRefs) Create(*downstreamcallbackdomain.OrderRef) error { return nil }
func (formTestRefs) GetByCredentialAndDownstreamNo(uint, string) (*downstreamcallbackdomain.OrderRef, error) {
	return nil, nil
}

type formTestSettings struct{ Settings }

func (formTestSettings) GetSiteCurrency(string) (string, error) { return "CNY", nil }

// 接收端不得按交付类型丢弃 manual_form_data：对接商品（upstream）同样可能带必填表单。
func TestCreateOrderPassesManualFormDataForAllFulfillmentTypes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{constants.FulfillmentTypeManual, constants.FulfillmentTypeAuto, constants.FulfillmentTypeUpstream} {
		t.Run(kind, func(t *testing.T) {
			orders := &formTestOrders{}
			handler := &Handler{Dependencies: Dependencies{
				ProductRepository: formTestProducts{productdomain.Product{ID: 18, IsActive: true, FulfillmentType: kind}},
				SKUs:              formTestSKUs{}, Orders: orders, Payments: formTestPayments{},
				DownstreamRefs: formTestRefs{}, Settings: formTestSettings{},
			}}
			body, _ := json.Marshal(map[string]interface{}{
				"sku_id": 19, "quantity": 1, "manual_form_data": map[string]interface{}{"x_handle": "fixture_user"},
			})
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/upstream/orders", bytes.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set(upstreamUserIDKey, uint(1))
			ctx.Set(upstreamCredentialIDKey, uint(1))
			handler.CreateOrder(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := orders.received.ManualFormData["18"]["x_handle"]; got != "fixture_user" {
				t.Fatalf("manual form data dropped for %s: %#v", kind, orders.received.ManualFormData)
			}
			if !orders.received.IsApiOrder {
				t.Fatalf("expected IsApiOrder to be true for upstream order")
			}
		})
	}
}

type canceledRefOrders struct{ formTestOrders }

func (s *canceledRefOrders) GetOrderByUser(uint, uint) (*orderdomain.Order, error) {
	return &orderdomain.Order{ID: 50, OrderNo: "OLD", Status: constants.OrderStatusCanceled}, nil
}

type existingRefs struct{ formTestRefs }

func (existingRefs) GetByCredentialAndDownstreamNo(uint, string) (*downstreamcallbackdomain.OrderRef, error) {
	return &downstreamcallbackdomain.OrderRef{OrderID: 50}, nil
}

// 余额不足导致旧单被取消后，下游用同一单号重试必须真正重新下单，而不是拿回已取消的旧单。
func TestCreateOrderRecreatesWhenSameDownstreamNoWasCanceled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	orders := &canceledRefOrders{}
	handler := &Handler{Dependencies: Dependencies{
		ProductRepository: formTestProducts{productdomain.Product{ID: 18, IsActive: true, FulfillmentType: constants.FulfillmentTypeAuto}},
		SKUs:              formTestSKUs{}, Orders: orders, Payments: formTestPayments{},
		DownstreamRefs: existingRefs{}, Settings: formTestSettings{},
	}}
	body, _ := json.Marshal(map[string]interface{}{"sku_id": 19, "quantity": 1, "downstream_order_no": "LOCAL-1"})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/upstream/orders", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(upstreamUserIDKey, uint(1))
	ctx.Set(upstreamCredentialIDKey, uint(1))
	handler.CreateOrder(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if orders.received.Items == nil {
		t.Fatalf("expected a new order to be created, body=%s", recorder.Body.String())
	}
}

type catalogPriceLevels struct {
	MemberLevelService
	userPrice  decimal.Decimal
	levelPrice decimal.Decimal
}

func (l catalogPriceLevels) ResolveMemberPrice(levelID, _, _ uint, base decimal.Decimal, costPrice ...decimal.Decimal) (decimal.Decimal, decimal.Decimal) {
	if levelID == 3 && l.levelPrice.LessThan(base) {
		mp := l.levelPrice
		if len(costPrice) > 0 && costPrice[0].GreaterThan(decimal.Zero) && mp.LessThan(costPrice[0]) {
			if base.GreaterThan(costPrice[0]) {
				mp = costPrice[0]
			} else {
				return base, decimal.Zero
			}
		}
		return mp, base.Sub(mp)
	}
	return base, decimal.Zero
}

func (l catalogPriceLevels) LoadUserPriceBook(userID uint) (memberlevelapp.UserPriceBook, error) {
	book := memberlevelapp.UserPriceBook{}
	if userID == 7 {
		book["18:0"] = l.userPrice
		book["18:19"] = l.userPrice
	}
	return book, nil
}

// ResolveCatalogPrice 复刻生产规则：专属价与等级价取更低者，等级价受成本价托底，专属价必须严格更低。
func (l catalogPriceLevels) ResolveCatalogPrice(book memberlevelapp.UserPriceBook, levelID, productID, skuID uint, base decimal.Decimal, costPrice ...decimal.Decimal) (decimal.Decimal, bool, bool) {
	best, hit, exclusive := base, false, false
	if levelID == 3 {
		if mp, _ := l.ResolveMemberPrice(levelID, productID, skuID, base, costPrice...); mp.LessThan(best) {
			best, hit = mp, true
		}
	}
	if price, ok := book.Resolve(productID, skuID, base); ok && price.LessThan(best) {
		best, hit, exclusive = price, true, true
	}
	return best, hit, exclusive
}

// 上游目录价口径必须与下单一致：专属价与会员等级价取更低者。
// 专属价比会员价贵时必须让会员价胜出，否则用户会比没有专属价时付得更多。
func TestUpstreamCatalogPriceTakesLowerOfUserPriceAndLevelPrice(t *testing.T) {
	product := productdomain.Product{
		ID:          18,
		PriceAmount: money.FromDecimal(decimal.NewFromInt(100)),
		SKUs: []productdomain.ProductSKU{{
			ID: 19, ProductID: 18, IsActive: true,
			PriceAmount: money.FromDecimal(decimal.NewFromInt(100)),
		}},
	}

	t.Run("user price below level price wins", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice:  decimal.RequireFromString("85"),
			levelPrice: decimal.RequireFromString("90"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(7)
		hit := handler.toUpstreamProductWithMemberPrice(product, book, 3, nil)
		if hit.PriceAmount != "85.00" || hit.MemberPrice != "85.00" || hit.OriginalPrice != "100.00" {
			t.Fatalf("product price = %s/%s/%s, want 85.00/85.00/100.00", hit.PriceAmount, hit.MemberPrice, hit.OriginalPrice)
		}
		if got := hit.SKUs[0]; got.PriceAmount != "85.00" || got.MemberPrice != "85.00" || got.OriginalPrice != "100.00" {
			t.Fatalf("sku price = %s/%s/%s, want 85.00/85.00/100.00", got.PriceAmount, got.MemberPrice, got.OriginalPrice)
		}
	})

	t.Run("user price above level price loses to the level price", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice:  decimal.RequireFromString("95"),
			levelPrice: decimal.RequireFromString("90"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(7)
		got := handler.toUpstreamProductWithMemberPrice(product, book, 3, nil)
		if got.PriceAmount != "90.00" || got.SKUs[0].PriceAmount != "90.00" {
			t.Fatalf("price = %s/%s, want the cheaper level price 90.00", got.PriceAmount, got.SKUs[0].PriceAmount)
		}
	})

	t.Run("user price alone applies when there is no level", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice:  decimal.RequireFromString("95"),
			levelPrice: decimal.RequireFromString("90"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(7)
		got := handler.toUpstreamProductWithMemberPrice(product, book, 0, nil)
		if got.PriceAmount != "95.00" || got.SKUs[0].PriceAmount != "95.00" {
			t.Fatalf("price = %s/%s, want user price 95.00", got.PriceAmount, got.SKUs[0].PriceAmount)
		}
	})

	t.Run("level price applies to a user without a user price", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice:  decimal.RequireFromString("95"),
			levelPrice: decimal.RequireFromString("90"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(8)
		got := handler.toUpstreamProductWithMemberPrice(product, book, 3, nil)
		if got.PriceAmount != "90.00" || got.SKUs[0].PriceAmount != "90.00" {
			t.Fatalf("level fallback price = %s/%s, want 90.00", got.PriceAmount, got.SKUs[0].PriceAmount)
		}
	})

	t.Run("no discount at all", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice:  decimal.RequireFromString("95"),
			levelPrice: decimal.RequireFromString("90"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(8)
		none := handler.toUpstreamProductWithMemberPrice(product, book, 0, nil)
		if none.PriceAmount != "100.00" || none.MemberPrice != "" || none.SKUs[0].PriceAmount != "100.00" {
			t.Fatalf("no discount price = %s member=%q, want 100.00 and empty", none.PriceAmount, none.MemberPrice)
		}
	})
}

// idempotencyOrders / idempotencyRefs / countingPayments 用内存状态模拟订单、引用表和支付，
// 用来验证同一幂等键只会下一单、扣一次款。
type idempotencyOrders struct {
	Orders
	mu       sync.Mutex
	created  int
	canceled int
}

func (s *idempotencyOrders) CreateOrder(CreateOrderInput) (*orderdomain.Order, error) {
	time.Sleep(10 * time.Millisecond) // 放大「先查后建」的窗口
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	return &orderdomain.Order{ID: uint(100 + s.created), OrderNo: "IDEM", Status: constants.OrderStatusPendingPayment}, nil
}

func (s *idempotencyOrders) GetOrderByUser(orderID, _ uint) (*orderdomain.Order, error) {
	return &orderdomain.Order{ID: orderID, OrderNo: "IDEM", Status: constants.OrderStatusPaid}, nil
}

func (s *idempotencyOrders) CancelOrder(orderID, _ uint) (*orderdomain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled++
	return &orderdomain.Order{ID: orderID, Status: constants.OrderStatusCanceled}, nil
}

type idempotencyRefs struct {
	mu        sync.Mutex
	refs      map[string]*downstreamcallbackdomain.OrderRef
	createErr error
}

func (s *idempotencyRefs) Create(ref *downstreamcallbackdomain.OrderRef) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[ref.DownstreamOrderNo] = ref
	return nil
}

func (s *idempotencyRefs) GetByCredentialAndDownstreamNo(_ uint, no string) (*downstreamcallbackdomain.OrderRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[no], nil
}

type countingPayments struct{ calls atomic.Int32 }

func (s *countingPayments) CreatePayment(CreatePaymentInput) (*CreatePaymentResult, error) {
	s.calls.Add(1)
	return &CreatePaymentResult{OrderPaid: true}, nil
}

func postDownstreamOrder(handler *Handler, downstreamOrderNo string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]interface{}{"sku_id": 19, "quantity": 1, "downstream_order_no": downstreamOrderNo})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/upstream/orders", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(upstreamUserIDKey, uint(1))
	ctx.Set(upstreamCredentialIDKey, uint(1))
	handler.CreateOrder(ctx)
	return recorder
}

// 同一凭证、同一下游单号的并发请求只能创建一笔订单、执行一次支付。
func TestCreateOrderConcurrentSameDownstreamNoCreatesOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	orders := &idempotencyOrders{}
	payments := &countingPayments{}
	handler := &Handler{Dependencies: Dependencies{
		ProductRepository: formTestProducts{productdomain.Product{ID: 18, IsActive: true, FulfillmentType: constants.FulfillmentTypeAuto}},
		SKUs:              formTestSKUs{}, Orders: orders, Payments: payments,
		DownstreamRefs: &idempotencyRefs{refs: map[string]*downstreamcallbackdomain.OrderRef{}}, Settings: formTestSettings{},
	}}

	// 测试环境没有 Redis：用内存互斥锁顶替，验证的是「拿到锁之后再做幂等检查」这条流程。
	var mu sync.Mutex
	original := acquireDownstreamOrderLock
	acquireDownstreamOrderLock = func(context.Context, string, time.Duration) (func(), error) {
		mu.Lock()
		return mu.Unlock, nil
	}
	t.Cleanup(func() { acquireDownstreamOrderLock = original })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if recorder := postDownstreamOrder(handler, "DUP-1"); recorder.Code != http.StatusOK {
				t.Errorf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		}()
	}
	wg.Wait()

	if orders.created != 1 || payments.calls.Load() != 1 {
		t.Fatalf("want 1 order and 1 payment, got %d orders and %d payments", orders.created, payments.calls.Load())
	}
}

// 引用写入失败时必须停在扣款之前，并取消刚创建的订单。
func TestCreateOrderStopsBeforePaymentWhenRefCreateFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	orders := &idempotencyOrders{}
	payments := &countingPayments{}
	handler := &Handler{Dependencies: Dependencies{
		ProductRepository: formTestProducts{productdomain.Product{ID: 18, IsActive: true, FulfillmentType: constants.FulfillmentTypeAuto}},
		SKUs:              formTestSKUs{}, Orders: orders, Payments: payments,
		DownstreamRefs: &idempotencyRefs{createErr: errors.New("db down")}, Settings: formTestSettings{},
	}}

	recorder := postDownstreamOrder(handler, "REF-FAIL-1")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if payments.calls.Load() != 0 || orders.canceled != 1 {
		t.Fatalf("want 0 payments and 1 canceled order, got %d payments and %d canceled", payments.calls.Load(), orders.canceled)
	}
}

// 验证上游目录对代理价、用户专属价及批发阶梯的处理口径与下单一致：
// - 有代理价的 SKU 忽略会员等级折扣、与用户专属价取更低者，并剔除不生效的批发阶梯；
// - 无代理价的 SKU 保持原有会员价/专属价与批发阶梯。
func TestToUpstreamProductWithAgencyPrice(t *testing.T) {
	baseProduct := productdomain.Product{
		ID:                18,
		Slug:              "agency-product",
		PriceAmount:       money.FromDecimal(decimal.RequireFromString("100.00")),
		AgencyPriceAmount: money.FromDecimal(decimal.RequireFromString("70.00")),
		WholesalePrices: productdomain.WholesalePriceTiers{
			{MinQuantity: 5, UnitPrice: money.FromDecimal(decimal.RequireFromString("80.00"))},
		},
		FulfillmentType: constants.FulfillmentTypeManual,
		SKUs: []productdomain.ProductSKU{
			{
				ID:                19,
				SKUCode:           "SKU-1",
				PriceAmount:       money.FromDecimal(decimal.RequireFromString("100.00")),
				AgencyPriceAmount: money.FromDecimal(decimal.RequireFromString("70.00")),
				IsActive:          true,
			},
		},
	}

	t.Run("agency price overrides level discount and hides wholesale tiers", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			levelPrice: decimal.RequireFromString("60.00"),
		}}}
		res := handler.toUpstreamProductWithMemberPrice(baseProduct, memberlevelapp.UserPriceBook{}, 3, nil)
		if res.PriceAmount != "70.00" || res.OriginalPrice != "100.00" || res.AgencyPrice != "70.00" || res.MemberPrice != "" {
			t.Fatalf("unexpected product response: %+v", res)
		}
		if len(res.WholesalePrices) != 0 {
			t.Fatalf("expected wholesale_prices cleared when all skus use agency price, got %+v", res.WholesalePrices)
		}
		if len(res.SKUs) != 1 || res.SKUs[0].PriceAmount != "70.00" || res.SKUs[0].AgencyPrice != "70.00" || res.SKUs[0].OriginalPrice != "100.00" || res.SKUs[0].MemberPrice != "" {
			t.Fatalf("unexpected sku response: %+v", res.SKUs)
		}
	})

	t.Run("lower user exclusive price beats agency price", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice: decimal.RequireFromString("60.00"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(7)
		res := handler.toUpstreamProductWithMemberPrice(baseProduct, book, 3, nil)
		if res.PriceAmount != "60.00" || res.MemberPrice != "60.00" || res.AgencyPrice != "70.00" {
			t.Fatalf("expected cheaper user price 60.00 to win, got %+v", res)
		}
		if res.SKUs[0].PriceAmount != "60.00" || res.SKUs[0].MemberPrice != "60.00" || res.SKUs[0].AgencyPrice != "70.00" {
			t.Fatalf("expected sku cheaper user price 60.00 to win, got %+v", res.SKUs[0])
		}
	})

	t.Run("higher user exclusive price loses to agency price", func(t *testing.T) {
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			userPrice: decimal.RequireFromString("80.00"),
		}}}
		book, _ := handler.MemberLevels.LoadUserPriceBook(7)
		res := handler.toUpstreamProductWithMemberPrice(baseProduct, book, 3, nil)
		if res.PriceAmount != "70.00" || res.MemberPrice != "" || res.AgencyPrice != "70.00" {
			t.Fatalf("expected agency price 70.00 to win over higher exclusive 80.00, got %+v", res)
		}
		if res.SKUs[0].PriceAmount != "70.00" || res.SKUs[0].MemberPrice != "" {
			t.Fatalf("expected sku agency price 70.00 to win, got %+v", res.SKUs[0])
		}
	})

	t.Run("mixed skus filter agency sku wholesale tiers and preserve non-agency sku pricing", func(t *testing.T) {
		mixed := productdomain.Product{
			ID:                18,
			Slug:              "mixed-agency",
			PriceAmount:       money.FromDecimal(decimal.RequireFromString("100.00")),
			AgencyPriceAmount: money.FromDecimal(decimal.Zero),
			WholesalePrices: productdomain.WholesalePriceTiers{
				{SKUID: 19, SKUCode: "SKU-A", MinQuantity: 5, UnitPrice: money.FromDecimal(decimal.RequireFromString("80.00"))},
				{SKUID: 20, SKUCode: "SKU-B", MinQuantity: 5, UnitPrice: money.FromDecimal(decimal.RequireFromString("85.00"))},
			},
			FulfillmentType: constants.FulfillmentTypeManual,
			SKUs: []productdomain.ProductSKU{
				{ID: 19, SKUCode: "SKU-A", PriceAmount: money.FromDecimal(decimal.RequireFromString("100.00")), AgencyPriceAmount: money.FromDecimal(decimal.RequireFromString("70.00")), IsActive: true},
				{ID: 20, SKUCode: "SKU-B", PriceAmount: money.FromDecimal(decimal.RequireFromString("100.00")), AgencyPriceAmount: money.FromDecimal(decimal.Zero), IsActive: true},
			},
		}
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			levelPrice: decimal.RequireFromString("90.00"),
		}}}
		res := handler.toUpstreamProductWithMemberPrice(mixed, memberlevelapp.UserPriceBook{}, 3, nil)
		if len(res.WholesalePrices) != 1 || res.WholesalePrices[0].SKUID != 20 {
			t.Fatalf("expected only non-agency sku 20 wholesale tier kept, got %+v", res.WholesalePrices)
		}
		if res.SKUs[0].PriceAmount != "70.00" || res.SKUs[0].AgencyPrice != "70.00" {
			t.Fatalf("sku 19 want agency 70.00, got %+v", res.SKUs[0])
		}
		if res.SKUs[1].PriceAmount != "90.00" || res.SKUs[1].MemberPrice != "90.00" || res.SKUs[1].AgencyPrice != "" {
			t.Fatalf("sku 20 want level price 90.00, got %+v", res.SKUs[1])
		}
	})

	t.Run("non-agency sku member price respects cost floor while user exclusive price does not", func(t *testing.T) {
		noAgency := productdomain.Product{
			ID:              18,
			Slug:            "cost-floor-product",
			PriceAmount:     money.FromDecimal(decimal.RequireFromString("300.00")),
			CostPriceAmount: money.FromDecimal(decimal.RequireFromString("200.00")),
			FulfillmentType: constants.FulfillmentTypeManual,
			SKUs: []productdomain.ProductSKU{
				{
					ID:              19,
					SKUCode:         "SKU-FLOOR",
					PriceAmount:     money.FromDecimal(decimal.RequireFromString("300.00")),
					CostPriceAmount: money.FromDecimal(decimal.RequireFromString("200.00")),
					IsActive:        true,
				},
			},
		}
		handler := &Handler{Dependencies: Dependencies{MemberLevels: catalogPriceLevels{
			levelPrice: decimal.RequireFromString("150.00"),
			userPrice:  decimal.RequireFromString("180.00"),
		}}}
		// 无专属价：等级价 150 被成本 200 托底至 200.00
		resFloor := handler.toUpstreamProductWithMemberPrice(noAgency, memberlevelapp.UserPriceBook{}, 3, nil)
		if resFloor.PriceAmount != "200.00" || resFloor.MemberPrice != "200.00" {
			t.Fatalf("expected product member price floored to 200.00, got %+v", resFloor)
		}
		if resFloor.SKUs[0].PriceAmount != "200.00" || resFloor.SKUs[0].MemberPrice != "200.00" {
			t.Fatalf("expected sku member price floored to 200.00, got %+v", resFloor.SKUs[0])
		}
		// 有用户专属一口价 180：不受成本价托底限制，比托底后的 200 更低，生效 180.00
		book, _ := handler.MemberLevels.LoadUserPriceBook(7)
		resExclusive := handler.toUpstreamProductWithMemberPrice(noAgency, book, 3, nil)
		if resExclusive.PriceAmount != "180.00" || resExclusive.MemberPrice != "180.00" {
			t.Fatalf("expected product user exclusive price 180.00 not floored by cost, got %+v", resExclusive)
		}
		if resExclusive.SKUs[0].PriceAmount != "180.00" || resExclusive.SKUs[0].MemberPrice != "180.00" {
			t.Fatalf("expected sku user exclusive price 180.00 not floored by cost, got %+v", resExclusive.SKUs[0])
		}
	})
}
