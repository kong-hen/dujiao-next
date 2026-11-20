package application

import (
	"strings"

	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"

	resellerdomain "github.com/dujiao-next/internal/modules/reseller/domain"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	"github.com/shopspring/decimal"
)

const (
	RuleSourceSKU     = "sku"
	RuleSourceProduct = "product"
	RuleSourceProfile = "profile"
	RuleSourceInherit = "inherit"
)

// PricingRule 描述分销改价规则解析结果。
type PricingRule struct {
	Mode              string
	Source            string
	SettingID         *uint
	MarkupPercent     decimal.Decimal
	FixedMarkupAmount decimal.Decimal
	FixedPriceAmount  decimal.Decimal
}

// ResolveUnitAmount 按 SKU → 商品 → 资料默认加价 → 继承主站售价的优先级解析成交单价。
// masterUnit 为主站零售价，所有加价与继承规则均以主站零售价为基数。
func ResolveUnitAmount(profile *resellerdomain.Profile, productSetting *resellerdomain.ProductSetting, skuSetting *resellerdomain.ProductSetting, masterUnit decimal.Decimal) (decimal.Decimal, PricingRule, error) {
	masterUnit = masterUnit.Round(2)
	if skuSetting != nil && strings.TrimSpace(skuSetting.PricingMode) != resellerdomain.PricingModeInherit {
		return ApplyPricingRule(*skuSetting, RuleSourceSKU, masterUnit)
	}
	if productSetting != nil && strings.TrimSpace(productSetting.PricingMode) != resellerdomain.PricingModeInherit {
		return ApplyPricingRule(*productSetting, RuleSourceProduct, masterUnit)
	}
	if profile != nil && profile.DefaultMarkupPercent.Decimal.GreaterThan(decimal.Zero) {
		rule := PricingRule{
			Mode:          resellerdomain.PricingModeMarkupPercent,
			Source:        RuleSourceProfile,
			MarkupPercent: profile.DefaultMarkupPercent.Decimal.Round(2),
		}
		return ApplyMarkupPercent(masterUnit, rule.MarkupPercent), rule, nil
	}
	return masterUnit, PricingRule{Mode: resellerdomain.PricingModeInherit, Source: RuleSourceInherit}, nil
}

// ApplyPricingRule 按单条分销配置基于主站零售价计算单价。
func ApplyPricingRule(setting resellerdomain.ProductSetting, source string, masterUnit decimal.Decimal) (decimal.Decimal, PricingRule, error) {
	masterUnit = masterUnit.Round(2)
	settingID := setting.ID
	rule := PricingRule{
		Mode:              strings.TrimSpace(setting.PricingMode),
		Source:            source,
		SettingID:         &settingID,
		MarkupPercent:     setting.MarkupPercent.Decimal.Round(2),
		FixedMarkupAmount: setting.FixedMarkupAmount.Decimal.Round(2),
		FixedPriceAmount:  setting.FixedPriceAmount.Decimal.Round(2),
	}
	switch rule.Mode {
	case resellerdomain.PricingModeMarkupPercent:
		return ApplyMarkupPercent(masterUnit, rule.MarkupPercent), rule, nil
	case resellerdomain.PricingModeFixedMarkup:
		return masterUnit.Add(rule.FixedMarkupAmount).Round(2), rule, nil
	case resellerdomain.PricingModeFixedPrice:
		return rule.FixedPriceAmount.Round(2), rule, nil
	case resellerdomain.PricingModeInherit:
		return masterUnit, rule, nil
	default:
		return decimal.Zero, rule, resellercontract.ErrPricingModeInvalid
	}
}

// ApplyMarkupPercent 按百分比加价。
func ApplyMarkupPercent(baseUnit decimal.Decimal, percent decimal.Decimal) decimal.Decimal {
	return baseUnit.Mul(decimal.NewFromInt(100).Add(percent)).Div(decimal.NewFromInt(100)).Round(2)
}

// ValidateUnitAmount 校验分销单价不低于进货底价/主站售价/成本价，且相对主站售价不超过最大加价比例。
func ValidateUnitAmount(profile *resellerdomain.Profile, sku *productdomain.ProductSKU, baseUnit decimal.Decimal, masterUnit decimal.Decimal, resellerUnit decimal.Decimal) error {
	baseUnit = baseUnit.Round(2)
	masterUnit = masterUnit.Round(2)
	resellerUnit = resellerUnit.Round(2)
	if resellerUnit.LessThanOrEqual(decimal.Zero) || resellerUnit.LessThan(baseUnit) {
		return resellercontract.ErrPriceBelowBase
	}
	if masterUnit.GreaterThan(decimal.Zero) && resellerUnit.LessThan(masterUnit) {
		return resellercontract.ErrPriceBelowMasterPrice
	}
	if sku != nil && sku.CostPriceAmount.Decimal.GreaterThan(decimal.Zero) && resellerUnit.LessThan(sku.CostPriceAmount.Decimal.Round(2)) {
		return resellercontract.ErrPriceBelowBase
	}
	if profile != nil && profile.MaxMarkupPercent.Decimal.GreaterThan(decimal.Zero) && masterUnit.GreaterThan(decimal.Zero) {
		implicit := resellerUnit.Sub(masterUnit).Div(masterUnit).Mul(decimal.NewFromInt(100)).Round(4)
		if implicit.GreaterThan(profile.MaxMarkupPercent.Decimal.Round(4)) {
			return resellercontract.ErrMarkupExceeded
		}
	}
	return nil
}
