package web

import (
	"sync"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/app"
)

type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
}

func NewSessionManager(ttl time.Duration) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
	}
}

func (m *SessionManager) Create() (string, error) {
	token, err := app.RandomToken(18)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = time.Now().Add(m.ttl)
	return token, nil
}

func (m *SessionManager) Valid(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	expiresAt, ok := m.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiresAt) {
		delete(m.sessions, token)
		return false
	}
	return true
}

func (m *SessionManager) Delete(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}
