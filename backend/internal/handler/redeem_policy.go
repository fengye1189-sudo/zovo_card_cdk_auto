package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const siteRedeemPolicyKey = "site_redeem_policy"

// SiteRedeemPolicy 本站可控兑换策略（不依赖 ACC；经卡台协议下发 no_auto_card_switch / 发码偏好）。
// 一卡几付的硬限制仍由卡台账户侧容量策略执行；此处把偏好与「是否自动换卡」交给本站。
type SiteRedeemPolicy struct {
	Enabled bool `json:"enabled"`
	// NoAutoCardSwitch=true → 兑换时向卡台传 no_auto_card_switch（失败不自动换卡）
	NoAutoCardSwitch bool `json:"no_auto_card_switch"`
	// StrictCardPreference=true → 兑换时向卡台传 strict_card_preference：CDK 严格按本站选卡配置
	// (发码偏好)选卡，卡台默认级联(537872/星链)只给卡台直充用户、不再盖过 CDK。默认开。
	StrictCardPreference bool `json:"strict_card_preference"`
	// AutoOpenWhenNoCard：展示/说明用；卡台 CDK 兑换默认 auto_open，本字段预留与文档对齐
	AutoOpenWhenNoCard bool `json:"auto_open_when_no_card"`
	// 每卡新账号上限（展示 + 写入审计；硬限以卡台为准）
	MaxNewAccountsPerCard int `json:"max_new_accounts_per_card"`
	// 单任务最多卡数（预留）
	MaxCardsPerTask int `json:"max_cards_per_task"`
	// 失败冷却小时（预留展示）
	FailCooldownHours int `json:"fail_cooldown_hours"`
	// 限定发卡地区文案
	IssuingArea string `json:"issuing_area"`
	// 持卡人
	HolderFirst string `json:"holder_first"`
	HolderLast  string `json:"holder_last"`
	// 指定产品码：空则用「选卡配置」第一条启用规则的 plan_key
	ProductCode string `json:"product_code"`
	// 渠道：one/three/four；空则从产品缓存推断
	Issuer string `json:"issuer"`
}

func defaultSiteRedeemPolicy() SiteRedeemPolicy {
	return SiteRedeemPolicy{
		Enabled:               false,
		NoAutoCardSwitch:      true, // 启用本站策略时默认不让卡台自动换卡
		StrictCardPreference:  true, // CDK 严格按本站选卡配置，不被卡台默认 537872 盖过
		AutoOpenWhenNoCard:    true,
		MaxNewAccountsPerCard: 4,
		MaxCardsPerTask:       3,
		FailCooldownHours:     24,
		IssuingArea:           "United States",
		HolderFirst:           "GPT",
		HolderLast:            "Direct",
	}
}

func normalizeSiteRedeemPolicy(p SiteRedeemPolicy) SiteRedeemPolicy {
	if p.MaxNewAccountsPerCard <= 0 {
		p.MaxNewAccountsPerCard = 4
	}
	if p.MaxCardsPerTask <= 0 {
		p.MaxCardsPerTask = 3
	}
	if p.FailCooldownHours <= 0 {
		p.FailCooldownHours = 24
	}
	return p
}

// loadSiteRedeemPolicyStrict is used on the public payment path. A malformed
// or temporarily unreadable policy must stop before the one-way submit latch
// is consumed; silently falling back here could bypass the configured card
// protections for a real payment.
func loadSiteRedeemPolicyStrict() (SiteRedeemPolicy, error) {
	p := defaultSiteRedeemPolicy()
	if db.DB == nil {
		return p, fmt.Errorf("db not ready")
	}
	raw, err := db.GetSetting(siteRedeemPolicyKey)
	if err != nil {
		return p, err
	}
	if strings.TrimSpace(raw) == "" {
		return normalizeSiteRedeemPolicy(p), nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("decode site redeem policy: %w", err)
	}
	return normalizeSiteRedeemPolicy(p), nil
}

func loadSiteRedeemPolicy() SiteRedeemPolicy {
	p, err := loadSiteRedeemPolicyStrict()
	if err != nil {
		return defaultSiteRedeemPolicy()
	}
	return p
}

func saveSiteRedeemPolicy(p SiteRedeemPolicy) error {
	if p.MaxNewAccountsPerCard <= 0 {
		p.MaxNewAccountsPerCard = 4
	}
	if p.MaxCardsPerTask <= 0 {
		p.MaxCardsPerTask = 3
	}
	if p.FailCooldownHours < 0 {
		p.FailCooldownHours = 0
	}
	p.ProductCode = strings.TrimSpace(p.ProductCode)
	p.Issuer = strings.ToLower(strings.TrimSpace(p.Issuer))
	p.IssuingArea = strings.TrimSpace(p.IssuingArea)
	p.HolderFirst = strings.TrimSpace(p.HolderFirst)
	p.HolderLast = strings.TrimSpace(p.HolderLast)
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return db.SetSetting(siteRedeemPolicyKey, string(b))
}
