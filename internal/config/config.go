package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Provider 描述一个上游模型供应商。
type Provider struct {
	Name             string  `json:"name"`
	BaseURL          string  `json:"base_url"`
	ModelName        string  `json:"model_name"`
	APIKey           string  `json:"api_key"`
	ContextLength    int     `json:"context_length"`
	OutputLength     int     `json:"output_length"`
	InputTokenPrice  float64 `json:"input_token_price"`
	CachedTokenPrice float64 `json:"cached_token_price"`
	OutputTokenPrice float64 `json:"output_token_price"`
}

// Group 绑定审查模型和一组子模型。
type Group struct {
	Name      string   `json:"name"`
	Reviewer  string   `json:"reviewer"`  // 审查模型（组长），负责审核和最终工具调用
	Providers []string `json:"providers"` // 子模型（组员），只提供分析解法
}

// CLI 服务配置。
type CLI struct {
	Language string `json:"language"`
	Port     int    `json:"port"`
	Host     string `json:"host"`
}

// Config 顶层配置。
type Config struct {
	Providers []Provider `json:"providers"`
	Groups    []Group    `json:"groups"`
	LogLevel  string     `json:"log_level"`
	CLI       CLI        `json:"cli"`

	providerMap map[string]Provider
	groupMap    map[string]Group
}

// ---- 加载 ----

func Load() (*Config, error) {
	candidates := []string{}
	if env := os.Getenv("FUSIONGATE_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	cwd, _ := os.Getwd()
	candidates = append(candidates, filepath.Join(cwd, "config.json"))
	if filepath.Base(cwd) == "fusiongate" {
		candidates = append(candidates, filepath.Join(filepath.Dir(cwd), "config.json"))
	}

	var lastErr error
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil { lastErr = err; continue }
		cfg, err := parse(data)
		if err != nil { return nil, fmt.Errorf("parse %s: %w", p, err) }
		cfg.providerMap["__config_path__"] = Provider{Name: "__config_path__", BaseURL: p}
		return cfg, nil
	}
	return nil, fmt.Errorf("config.json not found (tried %v): %w", candidates, lastErr)
}

func parse(data []byte) (*Config, error) {
	var raw struct {
		Providers []Provider `json:"providers"`
		Groups    []Group    `json:"groups"`
		LogLevel  string     `json:"log_level"`
		CLI       CLI        `json:"cli"`
	}
	if err := json.Unmarshal(data, &raw); err != nil { return nil, err }
	cfg := &Config{
		Providers: raw.Providers, Groups: raw.Groups,
		LogLevel: raw.LogLevel, CLI: raw.CLI,
	}
	cfg.index()
	cfg.fillDefaults()
	return cfg, nil
}

func (c *Config) index() {
	c.providerMap = make(map[string]Provider, len(c.Providers))
	for _, p := range c.Providers { c.providerMap[p.Name] = p }
	c.groupMap = make(map[string]Group, len(c.Groups))
	for _, g := range c.Groups { c.groupMap[g.Name] = g }
}

func (c *Config) fillDefaults() {
	if c.LogLevel == "" { c.LogLevel = "info" }
	if c.CLI.Port == 0 { c.CLI.Port = 8080 }
	if c.CLI.Host == "" { c.CLI.Host = "0.0.0.0" }
	if c.CLI.Language == "" { c.CLI.Language = "zh-CN" }
}

func (c *Config) Provider(name string) (Provider, bool) {
	p, ok := c.providerMap[name]; return p, ok
}

func (c *Config) Group(name string) (Group, bool) {
	g, ok := c.groupMap[name]; return g, ok
}

func (c *Config) ConfigPath() string {
	if p, ok := c.providerMap["__config_path__"]; ok { return p.BaseURL }
	return ""
}

func (c *Config) ResolveModelName(model string) string {
	if g, ok := c.Group(model); ok {
		if p, ok2 := c.Provider(g.Reviewer); ok2 { return p.ModelName }
	}
	return model
}

func (c *Config) Validate() []error {
	var errs []error
	if len(c.Providers) == 0 { errs = append(errs, fmt.Errorf("未配置任何 provider")) }
	for _, g := range c.Groups {
		if g.Reviewer == "" {
			errs = append(errs, fmt.Errorf("分组 %q 未指定 reviewer", g.Name))
		} else if _, ok := c.Provider(g.Reviewer); !ok {
			errs = append(errs, fmt.Errorf("分组 %q 的 reviewer %q 不存在", g.Name, g.Reviewer))
		}
		for _, pn := range g.Providers {
			if _, ok := c.Provider(pn); !ok {
				errs = append(errs, fmt.Errorf("分组 %q 的 provider %q 不存在", g.Name, pn))
			}
		}
	}
	return errs
}

// ---- ID 生成与缓存 key ----

var idCounter int64

func init() { idCounter = time.Now().UnixMicro() }
func NextID() string {
	idCounter++
	return fmt.Sprintf("fg_%x", idCounter)
}

func ProviderHash(url, model, key string) string {
	h := sha256.Sum256([]byte(url + "|" + model + "|" + key))
	return fmt.Sprintf("%x", h[:16])
}
