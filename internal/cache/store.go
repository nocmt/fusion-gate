package cache

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"fusiongate/internal/logger"
	"fusiongate/internal/types"
)

// Store 是请求级语义缓存。
//
// 设计理念（参考 OpenClacky 90.6% 缓存命中率实践）：
//   1. 前缀分层：稳定的 prompt 结构前置，动态上下文放尾部
//   2. TTL 博弈：在过期前主动检查，避免 cold start
//   3. 请求级去重：相同（消息+工具+分组）的请求只算一次
type Store struct {
	mu       sync.RWMutex
	entries  map[string]*entry
	log      *logger.Logger
	ttl      time.Duration
	done     chan struct{}
}

type entry struct {
	response  *types.ChatCompletionResponse
	createdAt time.Time
	lastHit   time.Time
	hits      int64
}

// New 创建缓存，ttl 默认为 10 分钟（适合 prompt cache 窗口）。
func New(ttl time.Duration, log *logger.Logger) *Store {
	if ttl <= 0 { ttl = 10 * time.Minute }
	s := &Store{
		entries: make(map[string]*entry),
		log:     log,
		ttl:     ttl,
		done:    make(chan struct{}),
	}
	go s.gcLoop()
	return s
}

// Shutdown 停止后台清理。
func (s *Store) Shutdown() { close(s.done) }

// Key 生成缓存键：消息内容 + 工具定义 + 分组名 + 模型名 → SHA256
func Key(messages []types.Message, tools []types.Tool, group, model string) string {
	raw, _ := json.Marshal(struct {
		Msgs  []types.Message `json:"msgs"`
		Tools []types.Tool    `json:"tools"`
		Group string          `json:"group"`
		Model string          `json:"model"`
	}{messages, tools, group, model})
	h := sha256.Sum256(raw)
	return fmt.Sprintf("%x", h[:16])
}

// Get 读取缓存，命中返回 (response, true)。
func (s *Store) Get(key string) (*types.ChatCompletionResponse, bool) {
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok { return nil, false }
	if time.Since(e.createdAt) > s.ttl { return nil, false }
	s.mu.Lock()
	e.lastHit = time.Now()
	e.hits++
	s.mu.Unlock()
	return e.response, true
}

// Set 写入缓存。
func (s *Store) Set(key string, resp *types.ChatCompletionResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = &entry{
		response:  resp,
		createdAt: time.Now(),
		lastHit:   time.Now(),
		hits:      0,
	}
}

// Stats 返回缓存统计。
func (s *Store) Stats() (size int, hitRate float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	size = len(s.entries)
	var totalHits int64
	for _, e := range s.entries { totalHits += e.hits }
	return size, 0 // hit rate 需外部跟踪
}

// Clear 清空缓存（如配置变更时）。
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]*entry)
}

// ---- 后台过期清理 ----

func (s *Store) gcLoop() {
	tk := time.NewTicker(time.Minute)
	defer tk.Stop()
	for {
		select {
		case <-s.done: return
		case <-tk.C: s.gc()
		}
	}
}

func (s *Store) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, e := range s.entries {
		if now.Sub(e.createdAt) > s.ttl { delete(s.entries, k) }
	}
}
