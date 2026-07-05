package session

import (
	"sync"
	"time"

	"fusiongate/internal/config"
	"fusiongate/internal/logger"
)

// Store 维护网关 response_id 到各供应商的映射。
//
// 场景：Codex 多轮工具调用时，每轮带 previous_response_id
// FusionGate 需要知道该 response_id 对应哪个 group、各 provider 的状态
type Store struct {
	mu    sync.RWMutex
	byID  map[string]*Entry
	log   *logger.Logger
	ttl   time.Duration
	done  chan struct{}
}

type Entry struct {
	ID             string
	ConversationID string // 客户端 conversation_id
	Group          string
	States         map[string]*State // providerName → state
	CreatedAt      time.Time
	LastAccess     time.Time
}

type State struct {
	ResponseID     string
	ConversationID string
}

func New(cfg config.SessionConfig, log *logger.Logger) *Store {
	ttl := cfg.TTLDuration
	if ttl <= 0 { ttl = time.Hour }
	s := &Store{
		byID: make(map[string]*Entry),
		log:  log,
		ttl:  ttl,
		done: make(chan struct{}),
	}
	go s.gcLoop()
	return s
}

func (s *Store) Shutdown() { close(s.done) }

// Register 为新请求注册会话。
func (s *Store) Register(convID, group string) string {
	id := config.NextID()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = &Entry{
		ID: id, ConversationID: convID, Group: group,
		States:    make(map[string]*State),
		CreatedAt: time.Now(), LastAccess: time.Now(),
	}
	s.log.Debug("session register id=%s conv=%s group=%s", id, convID, group)
	return id
}

// UpdateState 更新某 provider 的状态。
func (s *Store) UpdateState(sessionID, providerName, responseID, conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[sessionID]
	if !ok { return }
	e.LastAccess = time.Now()
	e.States[providerName] = &State{ResponseID: responseID, ConversationID: conversationID}
}

// FindByPrevResponse 根据 previous_response_id 查找会话。
func (s *Store) FindByPrevResponse(prevRespID string) *Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.byID[prevRespID]; ok {
		e.LastAccess = time.Now()
		return e
	}
	return nil
}

// FindByConv 根据 conversation_id 查找。
func (s *Store) FindByConv(convID string) *Entry {
	if convID == "" { return nil }
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.byID {
		if e.ConversationID == convID {
			e.LastAccess = time.Now()
			return e
		}
	}
	return nil
}

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
	for id, e := range s.byID {
		if now.Sub(e.LastAccess) > s.ttl { delete(s.byID, id) }
	}
}
