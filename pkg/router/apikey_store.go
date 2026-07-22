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

package router

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agenttier/agenttier/pkg/apikeystore"
	"github.com/agenttier/agenttier/pkg/router/auth"
)

// secretAPIKeyStore implements auth.APIKeyStore over Kubernetes Secrets. It
// is a thin wrapper around the shared pkg/apikeystore.Store so the storage
// logic (hashing, Secret CRUD, record I/O) lives in one place importable by
// both the Router and the controller (which mints sandbox-scoped keys).
type secretAPIKeyStore struct {
	store *apikeystore.Store
}

func newSecretAPIKeyStore(c client.Client, namespace string) *secretAPIKeyStore {
	return &secretAPIKeyStore{store: apikeystore.New(c, namespace)}
}

// apiKeyMetadata is the non-secret view of a stored key, returned by the
// list endpoint. It deliberately omits the hash and never includes the
// plaintext key (which exists only at creation time).
type apiKeyMetadata = apikeystore.Metadata

// secretNameForHash derives the deterministic Secret name for a key hash.
func secretNameForHash(keyHash string) string {
	return apikeystore.SecretNameForHash(keyHash)
}

// sanitizeLabelValue makes an arbitrary user identifier safe to use as a
// Kubernetes label value.
func sanitizeLabelValue(v string) string {
	return apikeystore.SanitizeLabelValue(v)
}

// GetAPIKeyByHash implements auth.APIKeyStore.
func (s *secretAPIKeyStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*auth.APIKeyRecord, error) {
	return s.store.GetAPIKeyByHash(ctx, keyHash)
}

// createAPIKeySecret writes a new API-key Secret for the given hash + record.
func (s *secretAPIKeyStore) createAPIKeySecret(ctx context.Context, keyHash string, rec *auth.APIKeyRecord) error {
	return s.store.Create(ctx, keyHash, rec)
}

// listAPIKeysForUser returns metadata for every API key owned by userID.
func (s *secretAPIKeyStore) listAPIKeysForUser(ctx context.Context, userID string) ([]apiKeyMetadata, error) {
	return s.store.ListForUser(ctx, userID)
}

// listAllAPIKeys returns metadata for every API key in the store, regardless
// of owner. Admin-only use (FR6.6's admin sandbox-list cross-reference).
func (s *secretAPIKeyStore) listAllAPIKeys(ctx context.Context) ([]apiKeyMetadata, error) {
	return s.store.ListAll(ctx)
}

// deleteAPIKey removes a key Secret by its ID, scoped to the requesting user
// unless isAdmin. Returns the deleted key's SHA-256 hash for cache eviction.
func (s *secretAPIKeyStore) deleteAPIKey(ctx context.Context, keyID, userID string, isAdmin bool) (string, error) {
	return s.store.Delete(ctx, keyID, userID, isAdmin)
}
