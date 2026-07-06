package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"fusiongate/internal/config"
	"fusiongate/internal/logger"
)

type rawEntry struct {
	MaxInputTokens     any     `json:"max_input_tokens"`
	MaxOutputTokens    any     `json:"max_output_tokens"`
	InputCostPerToken  float64 `json:"input_cost_per_token"`
	OutputCostPerToken float64 `json:"output_cost_per_token"`
	Mode               string  `json:"mode"`
	MaxTokens          any     `json:"max_tokens"`
}

type Store struct {
	log       *logger.Logger
	cachePath string
	ttl       time.Duration
	data      map[string]*config.PricingEntry
	loadedAt  time.Time
}

const defaultURL = "https://gh.felicity.ac.cn/https://github.com/BerriAI/litellm/raw/litellm_internal_staging/model_prices_and_context_window.json"

func New(cacheTTL time.Duration, log *logger.Logger) *Store {
	if cacheTTL <= 0 { cacheTTL = 72 * time.Hour }
	return &Store{log: log, cachePath: os.TempDir() + "/fusiongate_pricing_cache.json", ttl: cacheTTL}
}

func (s *Store) Load() (int, error) {
	if d, err := os.ReadFile(s.cachePath); err == nil {
		var cached struct {
			Data      map[string]*config.PricingEntry `json:"data"`
			Timestamp int64                           `json:"timestamp"`
		}
		if json.Unmarshal(d, &cached) == nil {
			age := time.Since(time.Unix(cached.Timestamp, 0))
			if age < s.ttl {
				s.data = cached.Data; s.loadedAt = time.Unix(cached.Timestamp, 0)
				s.log.Info("pricing cache: %d entries (age %.1fh)", len(s.data), age.Hours())
				return len(s.data), nil
			}
			s.log.Info("pricing cache expired (%.1fh), re-fetching", age.Hours())
		}
	}

	s.log.Info("fetching pricing DB ...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", defaultURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return 0, fmt.Errorf("fetch pricing: %w", err) }
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))

	s.data = parse(body)
	s.loadedAt = time.Now()

	cached := struct {
		Data      map[string]*config.PricingEntry `json:"data"`
		Timestamp int64                           `json:"timestamp"`
	}{Data: s.data, Timestamp: time.Now().Unix()}
	raw, _ := json.Marshal(cached)
	os.WriteFile(s.cachePath, raw, 0644)
	s.log.Info("pricing DB: %d entries (cached)", len(s.data))
	return len(s.data), nil
}

func (s *Store) Lookup(modelName string) *config.PricingEntry {
	if s.data == nil { return nil }
	key := strings.ToLower(modelName)
	if e, ok := s.data[key]; ok { return e }
	// partial match
	for k, e := range s.data {
		if strings.Contains(k, key) || strings.Contains(key, k) { return e }
	}
	return nil
}

func parse(raw []byte) map[string]*config.PricingEntry {
	var db map[string]json.RawMessage
	if json.Unmarshal(raw, &db) != nil { return nil }
	delete(db, "sample_spec")
	out := make(map[string]*config.PricingEntry, len(db))
	for key, val := range db {
		var re rawEntry
		if json.Unmarshal(val, &re) != nil { continue }
		if re.Mode != "" && re.Mode != "chat" && re.Mode != "embedding" && re.Mode != "completion" {
			continue
		}
		mi := intFromAny(re.MaxInputTokens)
		mo := intFromAny(re.MaxOutputTokens)
		if mi == 0 { mi = intFromAny(re.MaxTokens) }
		if mo == 0 && mi > 0 { mo = mi / 4 }
		out[strings.ToLower(key)] = &config.PricingEntry{
			MaxInputTokens: mi, MaxOutputTokens: mo,
			InputCostPerToken: re.InputCostPerToken, OutputCostPerToken: re.OutputCostPerToken,
		}
	}
	return out
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64: return int(n)
	case string:
		var i int; fmt.Sscanf(n, "%d", &i); return i
	}
	return 0
}
