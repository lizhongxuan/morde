package gateway

import (
	"crypto/sha256"
	"sync"
	"time"

	"morde/server/internal/domain/identity"
)

const maxCachedCredentials = 10000

type cachedCredential struct {
	principal identity.Principal
	expiresAt time.Time
}

type credentialCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[[sha256.Size]byte]cachedCredential
}

func newCredentialCache(ttl time.Duration) *credentialCache {
	return &credentialCache{ttl: ttl, data: make(map[[sha256.Size]byte]cachedCredential)}
}

func (c *credentialCache) get(raw string, now time.Time) (identity.Principal, bool) {
	key := sha256.Sum256([]byte(raw))
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.data[key]
	if !ok {
		return identity.Principal{}, false
	}
	if !entry.expiresAt.After(now) {
		delete(c.data, key)
		return identity.Principal{}, false
	}
	return entry.principal, true
}

func (c *credentialCache) put(raw string, principal identity.Principal, lifetime time.Duration, now time.Time) {
	if lifetime <= 0 {
		return
	}
	if lifetime > c.ttl {
		lifetime = c.ttl
	}
	key := sha256.Sum256([]byte(raw))
	c.mu.Lock()
	defer c.mu.Unlock()
	for existingKey, entry := range c.data {
		if !entry.expiresAt.After(now) {
			delete(c.data, existingKey)
		}
	}
	if len(c.data) >= maxCachedCredentials {
		return
	}
	c.data[key] = cachedCredential{principal: principal, expiresAt: now.Add(lifetime)}
}
