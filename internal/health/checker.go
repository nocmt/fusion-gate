package health

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"fusiongate/internal/config"
	"fusiongate/internal/logger"
)

type Result struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Model   string `json:"model"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Latency int64  `json:"latency_ms"`
}

type cacheData struct {
	Checked map[string]cacheEntry `json:"checked"`
}

type cacheEntry struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type Checker struct {
	log       *logger.Logger
	cachePath string
	key       []byte
	mu        sync.Mutex
}

func New(log *logger.Logger) *Checker {
	return &Checker{
		log:       log,
		cachePath: os.TempDir() + "/fusiongate_health_check",
		key:       deriveKey(),
	}
}

// CheckAll 顺序检查所有 provider，优先使用缓存。
func (c *Checker) CheckAll(cfg *config.Config) []Result {
	c.mu.Lock()
	defer c.mu.Unlock()

	cache := c.loadCache()
	var results []Result
	timeout := 10 * time.Second

	for _, p := range cfg.Providers {
		hash := config.ProviderHash(p.BaseURL, p.ModelName, p.APIKey)
		if cached, ok := cache.Checked[hash]; ok {
			results = append(results, Result{
				Name: p.Name, URL: p.BaseURL, Model: p.ModelName,
				Status: "cached_" + cached.Status, Error: cached.Error,
			})
			continue
		}
		// 顺序检查（避免 goroutine 并发写 map 的复杂问题）
		r := checkOne(p, timeout)
		cache.Checked[hash] = cacheEntry{Status: r.Status, Error: r.Error}
		results = append(results, r)
	}

	c.saveCache(cache)
	return results
}

func checkOne(p config.Provider, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	body := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, p.ModelName)
	req, _ := http.NewRequestWithContext(ctx, "POST", p.ResolveEndpoint(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		return Result{Name: p.Name, URL: p.BaseURL, Model: p.ModelName,
			Status: "failed", Error: err.Error(), Latency: latency}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return Result{Name: p.Name, URL: p.BaseURL, Model: p.ModelName,
			Status: "failed",
			Error:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(data)),
			Latency: latency}
	}
	return Result{Name: p.Name, URL: p.BaseURL, Model: p.ModelName,
		Status: "ok", Latency: latency}
}

// ---- 缓存 ----

func (c *Checker) loadCache() cacheData {
	data, err := os.ReadFile(c.cachePath)
	if err != nil { return cacheData{Checked: make(map[string]cacheEntry)} }
	plain, err := decrypt(data, c.key)
	if err != nil { return cacheData{Checked: make(map[string]cacheEntry)} }
	var cd cacheData
	if json.Unmarshal(plain, &cd) != nil { return cacheData{Checked: make(map[string]cacheEntry)} }
	if cd.Checked == nil { cd.Checked = make(map[string]cacheEntry) }
	return cd
}

func (c *Checker) saveCache(cd cacheData) {
	raw, _ := json.Marshal(cd)
	enc, err := encrypt(raw, c.key)
	if err != nil { return }
	os.WriteFile(c.cachePath, enc, 0600)
}

// ---- AES-256-GCM ----

func deriveKey() []byte {
	h := sha256.Sum256([]byte("fusiongate_health_v1"))
	return h[:]
}

func encrypt(plain []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil { return nil, err }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return nil, err }
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil { return nil, err }
	ciphertext := gcm.Seal(nonce, nonce, plain, nil)
	return []byte(hex.EncodeToString(ciphertext)), nil
}

func decrypt(encHex []byte, key []byte) ([]byte, error) {
	ciphertext, err := hex.DecodeString(string(encHex))
	if err != nil { return nil, err }
	block, err := aes.NewCipher(key)
	if err != nil { return nil, err }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return nil, err }
	ns := gcm.NonceSize()
	if len(ciphertext) < ns { return nil, fmt.Errorf("ciphertext too short") }
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}
