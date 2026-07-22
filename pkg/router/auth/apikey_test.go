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
	"testing"
	"time"
)

// mapStore is an in-memory APIKeyStore for tests.
type mapStore struct {
	records map[string]*APIKeyRecord
	calls   int
}

func (m *mapStore) GetAPIKeyByHash(_ context.Context, keyHash string) (*APIKeyRecord, error) {
	m.calls++
	rec, ok := m.records[keyHash]
	if !ok {
		return nil, errNotFound
	}
	return rec, nil
}

var errNotFound = &notFoundError{}

type notFoundError struct{}

func (*notFoundError) Error() string { return "not found" }

func TestAPIKeyValidator_ValidKey(t *testing.T) {
	key := "atk_secret123"
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {UserID: "u-1", Email: "u@example.com", Name: "U"},
	}}
	v := NewAPIKeyValidator(store, 16, time.Minute)

	claims, err := v.ValidateKey(context.Background(), key)
	if err != nil {
		t.Fatalf("expected valid key, got %v", err)
	}
	if claims.Sub != "u-1" {
		t.Errorf("sub = %q, want u-1", claims.Sub)
	}
}

func TestAPIKeyValidator_UnknownKey(t *testing.T) {
	store := &mapStore{records: map[string]*APIKeyRecord{}}
	v := NewAPIKeyValidator(store, 16, time.Minute)

	if _, err := v.ValidateKey(context.Background(), "atk_nope"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestAPIKeyValidator_ExpiredKey(t *testing.T) {
	key := "atk_expired"
	past := time.Now().Add(-time.Hour)
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {UserID: "u-1", ExpiresAt: &past},
	}}
	v := NewAPIKeyValidator(store, 16, time.Minute)

	if _, err := v.ValidateKey(context.Background(), key); err == nil {
		t.Fatal("expected error for expired key")
	}
}

func TestAPIKeyValidator_CachesHits(t *testing.T) {
	key := "atk_cacheme"
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {UserID: "u-1"},
	}}
	v := NewAPIKeyValidator(store, 16, time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := v.ValidateKey(context.Background(), key); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if store.calls != 1 {
		t.Errorf("expected 1 store lookup (rest cached), got %d", store.calls)
	}
}

func TestHashAPIKey_StableAndDistinct(t *testing.T) {
	a := HashAPIKey("atk_one")
	b := HashAPIKey("atk_one")
	c := HashAPIKey("atk_two")
	if a != b {
		t.Error("hash not stable for same input")
	}
	if a == c {
		t.Error("hash collision for distinct inputs")
	}
	if len(a) != 64 {
		t.Errorf("expected 64-hex-char sha256, got len %d", len(a))
	}
}

func TestAPIKeyValidator_CacheHitRechecksExpiry(t *testing.T) {
	key := "atk_willexpire"
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {UserID: "u-1"},
	}}
	v := NewAPIKeyValidator(store, 16, time.Hour)

	// Seed the cache directly with an entry whose key has already expired,
	// simulating a key that expired partway through a long cache TTL. The
	// pre-fix code returned the cached claims because it only compared
	// cachedAt against the TTL and never re-checked the key's own expiry.
	past := time.Now().Add(-time.Minute)
	v.cache.Put(HashAPIKey(key), &cacheEntry{
		claims:    &Claims{Sub: "u-1"},
		cachedAt:  time.Now(),
		expiresAt: &past,
	})

	if _, err := v.ValidateKey(context.Background(), key); err == nil {
		t.Fatal("expected an expired key to be rejected on a cache hit")
	}
	if store.calls != 0 {
		t.Errorf("expiry should be caught on the cache path without a store lookup; got %d store calls", store.calls)
	}
}

func TestAPIKeyValidator_ScopedKeyCarriesSandboxIDAndActionGroups(t *testing.T) {
	key := "atk_scoped"
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {
			UserID:       "u-1",
			SandboxID:    "sbx-1",
			ActionGroups: []string{"run-command", "files:read"},
		},
	}}
	v := NewAPIKeyValidator(store, 16, time.Minute)

	claims, err := v.ValidateKey(context.Background(), key)
	if err != nil {
		t.Fatalf("expected valid key, got %v", err)
	}
	if claims.SandboxID != "sbx-1" {
		t.Errorf("SandboxID = %q, want sbx-1", claims.SandboxID)
	}
	if len(claims.ActionGroups) != 2 || claims.ActionGroups[0] != "run-command" || claims.ActionGroups[1] != "files:read" {
		t.Errorf("ActionGroups = %v, want [run-command files:read]", claims.ActionGroups)
	}
}

func TestAPIKeyValidator_UserLevelKeyLeavesScopeFieldsEmpty(t *testing.T) {
	key := "atk_userlevel"
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {UserID: "u-1"},
	}}
	v := NewAPIKeyValidator(store, 16, time.Minute)

	claims, err := v.ValidateKey(context.Background(), key)
	if err != nil {
		t.Fatalf("expected valid key, got %v", err)
	}
	if claims.SandboxID != "" {
		t.Errorf("expected empty SandboxID for a user-level key, got %q", claims.SandboxID)
	}
	if len(claims.ActionGroups) != 0 {
		t.Errorf("expected no ActionGroups for a user-level key, got %v", claims.ActionGroups)
	}
}

func TestAPIKeyValidator_InvalidateEvictsCache(t *testing.T) {
	key := "atk_revokeme"
	store := &mapStore{records: map[string]*APIKeyRecord{
		HashAPIKey(key): {UserID: "u-1"},
	}}
	v := NewAPIKeyValidator(store, 16, time.Hour)

	// Prime the cache.
	if _, err := v.ValidateKey(context.Background(), key); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Simulate revocation: the backing record is deleted AND the cache evicted.
	delete(store.records, HashAPIKey(key))
	v.Invalidate(key)

	if _, err := v.ValidateKey(context.Background(), key); err == nil {
		t.Fatal("expected a revoked key to be rejected immediately after Invalidate")
	}
	if store.calls != 2 {
		t.Errorf("eviction should force a re-lookup against the store; got %d store calls", store.calls)
	}
}
