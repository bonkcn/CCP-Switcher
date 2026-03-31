package web

import (
	"sync"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/app"
)

const (
	shortSessionTTL = 24 * time.Hour
	longSessionTTL  = 30 * 24 * time.Hour
)

type sessionEntry struct {
	expiresAt   time.Time
	fingerprint string
}

type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]sessionEntry
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]sessionEntry),
	}
}

// Create creates a new session. If the fingerprint matches an existing valid session,
// a long TTL (30 days) is used; otherwise a short TTL (24h) is used.
func (m *SessionManager) Create(fingerprint string) (token string, ttl time.Duration, err error) {
	token, err = app.RandomToken(18)
	if err != nil {
		return "", 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ttl = shortSessionTTL
	if fingerprint != "" {
		now := time.Now()
		for _, entry := range m.sessions {
			if entry.fingerprint == fingerprint && now.Before(entry.expiresAt) {
				ttl = longSessionTTL
				break
			}
		}
	}
	m.sessions[token] = sessionEntry{
		expiresAt:   time.Now().Add(ttl),
		fingerprint: fingerprint,
	}
	return token, ttl, nil
}

func (m *SessionManager) Valid(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(entry.expiresAt) {
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
