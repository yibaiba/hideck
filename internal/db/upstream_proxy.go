package db

import (
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/upstreamproxy"
	"gorm.io/gorm"
)

// UpstreamProxy 前置代理实例（用于代理 VoWiFi 的 ePDG 连接）
// 通过 Socks5 UDP Associate 将 IKE/ESP 流量转发到 ePDG
type UpstreamProxy struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	Name      string    `json:"name"`
	Addr      string    `json:"addr"`               // Socks5 服务器地址 (host:port)
	Username  string    `json:"username"`           // 可选鉴权用户名
	Password  string    `json:"password,omitempty"` // 可选鉴权密码
	Enabled   bool      `json:"enabled"`            // 是否启用
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpstreamProxyCountryRule 将 SIM home country 路由到一条前置代理。
// 同一国家可以绑多条，开 VoWiFi 时从启用节点里选；多条时优先 UDP 正常的再随机。
type UpstreamProxyCountryRule struct {
	CountryCode     string    `gorm:"primaryKey;size:8" json:"country_code"`
	UpstreamProxyID string    `gorm:"primaryKey;size:64" json:"upstream_proxy_id"`
	Enabled         bool      `json:"enabled"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (UpstreamProxyCountryRule) TableName() string {
	return "upstream_proxy_country_rules"
}

// ── UpstreamProxy CRUD ──

// ListUpstreamProxies 列出所有前置代理实例
func ListUpstreamProxies() ([]UpstreamProxy, error) {
	var out []UpstreamProxy
	if err := DB.Order("id asc").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// GetUpstreamProxyByID 根据 ID 获取前置代理
func GetUpstreamProxyByID(id string) (*UpstreamProxy, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("empty id")
	}
	var out UpstreamProxy
	err := DB.First(&out, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

// UpsertUpstreamProxy 创建或更新前置代理
func UpsertUpstreamProxy(p UpstreamProxy) error {
	if strings.TrimSpace(p.ID) == "" {
		return errors.New("empty id")
	}
	if strings.TrimSpace(p.Addr) == "" {
		return errors.New("empty addr")
	}
	return DB.Save(&p).Error
}

// DeleteUpstreamProxy 删除前置代理（同时清理关联的国家规则）
func DeleteUpstreamProxy(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("empty id")
	}
	if err := DB.Delete(&UpstreamProxyCountryRule{}, "upstream_proxy_id = ?", id).Error; err != nil {
		return err
	}
	return DB.Delete(&UpstreamProxy{}, "id = ?", id).Error
}

// ── UpstreamProxyCountryRule 国家规则管理 ──

func ListUpstreamProxyCountryRules() ([]UpstreamProxyCountryRule, error) {
	var out []UpstreamProxyCountryRule
	if err := DB.Order("country_code asc").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func UpsertUpstreamProxyCountryRule(rule UpstreamProxyCountryRule) error {
	rule.CountryCode = upstreamproxy.NormalizeCountryCode(rule.CountryCode)
	rule.UpstreamProxyID = strings.TrimSpace(rule.UpstreamProxyID)
	if rule.CountryCode == "" {
		return errors.New("empty country_code")
	}
	if rule.UpstreamProxyID == "" {
		return errors.New("empty upstream_proxy_id")
	}
	rule.UpdatedAt = time.Now()
	return DB.Save(&rule).Error
}

func DeleteUpstreamProxyCountryRule(countryCode, proxyID string) error {
	countryCode = upstreamproxy.NormalizeCountryCode(countryCode)
	if countryCode == "" {
		return errors.New("empty country_code")
	}
	proxyID = strings.TrimSpace(proxyID)
	q := DB.Where("country_code = ?", countryCode)
	if proxyID != "" {
		q = q.Where("upstream_proxy_id = ?", proxyID)
	}
	return q.Delete(&UpstreamProxyCountryRule{}).Error
}

func GetCountryUpstreamProxies(countryCode string) ([]UpstreamProxy, error) {
	countryCode = upstreamproxy.NormalizeCountryCode(countryCode)
	if countryCode == "" || DB == nil {
		return nil, nil
	}
	var rules []UpstreamProxyCountryRule
	if err := DB.Where("country_code = ? AND enabled = ?", countryCode, true).Find(&rules).Error; err != nil {
		return nil, err
	}
	out := make([]UpstreamProxy, 0, len(rules))
	seen := map[string]struct{}{}
	for _, rule := range rules {
		id := strings.TrimSpace(rule.UpstreamProxyID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		proxy, err := GetUpstreamProxyByID(id)
		if err != nil {
			return nil, err
		}
		if proxy == nil || !proxy.Enabled {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, *proxy)
	}
	return out, nil
}

func PickUpstreamProxy(proxies []UpstreamProxy) *UpstreamProxy {
	if len(proxies) == 0 {
		return nil
	}
	if len(proxies) == 1 {
		p := proxies[0]
		return &p
	}
	p := proxies[rand.IntN(len(proxies))]
	return &p
}

func GetCountryUpstreamProxy(countryCode string) (*UpstreamProxy, error) {
	proxies, err := GetCountryUpstreamProxies(countryCode)
	if err != nil {
		return nil, err
	}
	return PickUpstreamProxy(proxies), nil
}

func GetHomeMCCUpstreamProxies(homeMCC string) ([]UpstreamProxy, string, error) {
	countryCode, ok := upstreamproxy.CountryCodeFromHomeMCC(homeMCC)
	if !ok {
		return nil, "", nil
	}
	proxies, err := GetCountryUpstreamProxies(countryCode)
	return proxies, countryCode, err
}

func GetHomeMCCUpstreamProxy(homeMCC string) (*UpstreamProxy, string, error) {
	proxies, countryCode, err := GetHomeMCCUpstreamProxies(homeMCC)
	if err != nil {
		return nil, countryCode, err
	}
	return PickUpstreamProxy(proxies), countryCode, nil
}
