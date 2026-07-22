/*
Copyright 2024 AgentTier Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// APIKeyValidator validates API keys. API key storage is planned for a future
// SQL datastore. For now, it validates against a static in-memory map.
type APIKeyValidator struct {
	store    APIKeyStore
	cache    *LRUCache
	cacheTTL time.Duration
}

// APIKeyStore is the interface for looking up API keys in the datastore.
type APIKeyStore interface {
	// GetAPIKeyByHash looks up an API key record by its SHA-256 hash.
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRecord, error)
}

// APIKeyRecord represents a stored API key.
//
// SandboxID and ActionGroups implement the sandbox-scoped key type (FR6): a
// non-empty SandboxID marks this as a scoped key bound to exactly one
// sandbox, restricted to the action groups listed in ActionGroups (e.g.
// "run-command", "files:read", "resume"). User-level keys leave both fields
// empty/nil and are unaffected. This finishes wiring the previously-dead
// Scopes field into an enforced model — Scopes is retained for backward
// compatibility with any existing records but is no longer the enforcement
// mechanism.
type APIKeyRecord struct {
	KeyHash           string     `json:"keyHash"`
	UserID            string     `json:"userId"`
	Email             string     `json:"email"`
	Name              string     `json:"name"`
	Scopes            []string   `json:"scopes"`
	AllowedNamespaces []string   `json:"allowedNamespaces"`
	SandboxID         string     `json:"sandboxId,omitempty"`
	ActionGroups      []string   `json:"actionGroups,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	ExpiresAt         *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt        *time.Time `json:"lastUsedAt,omitempty"`
}

// NewAPIKeyValidator creates a new API key validator with LRU caching.
func NewAPIKeyValidator(store APIKeyStore, cacheSize int, cacheTTL time.Duration) *APIKeyValidator {
	return &APIKeyValidator{
		store:    store,
		cache:    NewLRUCache(cacheSize),
		cacheTTL: cacheTTL,
	}
}

// ValidateKey validates an API key and returns the associated claims.
func (v *APIKeyValidator) ValidateKey(ctx context.Context, key string) (*Claims, error) {
	keyHash := hashAPIKey(key)

	// Check cache first
	if cached, ok := v.cache.Get(keyHash); ok {
		entry := cached.(*cacheEntry)
		// Re-check the key's own expiry on every hit. Otherwise a key that
		// expires partway through the cache TTL keeps authenticating until the
		// entry ages out — auth staleness bounded only by cacheTTL.
		if entry.expiresAt != nil && time.Now().After(*entry.expiresAt) {
			v.cache.Remove(keyHash)
			return nil, fmt.Errorf("API key expired")
		}
		if time.Since(entry.cachedAt) < v.cacheTTL {
			return entry.claims, nil
		}
		// Cache TTL elapsed — remove and re-validate against the store.
		v.cache.Remove(keyHash)
	}

	// Lookup in datastore
	record, err := v.store.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		return nil, fmt.Errorf("API key not found")
	}

	// Check expiration
	if record.ExpiresAt != nil && time.Now().After(*record.ExpiresAt) {
		return nil, fmt.Errorf("API key expired")
	}

	// Build claims. SandboxID/ActionGroups are carried straight from the
	// record so a scoped key's Claims reach the router with enough
	// information for scopedkey middleware to enforce bound-sandbox +
	// action-group checks; a user-level record leaves both empty.
	claims := &Claims{
		Sub:          record.UserID,
		Email:        record.Email,
		Name:         record.Name,
		SandboxID:    record.SandboxID,
		ActionGroups: record.ActionGroups,
	}

	// Cache the result
	v.cache.Put(keyHash, &cacheEntry{
		claims:    claims,
		cachedAt:  time.Now(),
		expiresAt: record.ExpiresAt,
	})

	return claims, nil
}

// Invalidate evicts a key from the cache by its plaintext value.
func (v *APIKeyValidator) Invalidate(key string) {
	v.InvalidateHash(hashAPIKey(key))
}

// InvalidateHash evicts a key from the cache by its SHA-256 hash. The revoke
// path calls this after deleting the backing Secret so a revoked key stops
// authenticating immediately instead of lingering for up to cacheTTL.
func (v *APIKeyValidator) InvalidateHash(keyHash string) {
	if v == nil || v.cache == nil {
		return
	}
	v.cache.Remove(keyHash)
}

// hashAPIKey computes the SHA-256 hash of an API key.
func hashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// HashAPIKey is the exported version for use by key generation code.
func HashAPIKey(key string) string {
	return hashAPIKey(key)
}

// --- Simple LRU Cache ---

type cacheEntry struct {
	claims    *Claims
	cachedAt  time.Time
	expiresAt *time.Time
}

// LRUCache is a simple thread-safe LRU cache.
type LRUCache struct {
	maxSize int
	items   map[string]*lruItem
	order   []string
	mu      sync.RWMutex
}

type lruItem struct {
	value interface{}
}

// NewLRUCache creates a new LRU cache with the given maximum size.
func NewLRUCache(maxSize int) *LRUCache {
	return &LRUCache{
		maxSize: maxSize,
		items:   make(map[string]*lruItem, maxSize),
		order:   make([]string, 0, maxSize),
	}
}

// Get retrieves a value from the cache.
func (c *LRUCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, ok := c.items[key]
	if !ok {
		return nil, false
	}
	return item.value, true
}

// Put adds a value to the cache, evicting the oldest entry if at capacity.
func (c *LRUCache) Put(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If key exists, update it
	if _, ok := c.items[key]; ok {
		c.items[key] = &lruItem{value: value}
		return
	}

	// Evict oldest if at capacity
	if len(c.items) >= c.maxSize && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}

	c.items[key] = &lruItem{value: value}
	c.order = append(c.order, key)
}

// Remove deletes a key from the cache.
func (c *LRUCache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
