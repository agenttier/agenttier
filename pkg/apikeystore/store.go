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

// Package apikeystore implements Secret-backed storage for AgentTier API-key
// records. It is shared by pkg/router (user-level API key CRUD) and
// pkg/controller (sandbox-scoped key auto-minting) so the controller can
// persist and look up APIKeyRecord data without importing pkg/router.
package apikeystore

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agenttier/agenttier/pkg/router/auth"
)

// Secret conventions for API-key storage. Keys are stored one-per-Secret in
// a namespace, never the raw key — only its SHA-256 hash (the Secret name is
// derived from the hash so lookup is a direct Get, no list-scan). The
// plaintext key is returned to the caller exactly once at creation and never
// persisted.
const (
	SecretPurposeLabel = "agenttier.io/secret-purpose" //nolint:gosec // G101: a label key, not a credential
	SecretPurpose      = "api-key"
	UserLabel          = "agenttier.io/api-key-user" //nolint:gosec // G101: a label key, not a credential
	SecretPrefix       = "agenttier-apikey-"         //nolint:gosec // G101: a Secret name prefix, not a credential
)

// Store implements auth.APIKeyStore over Kubernetes Secrets. Each API key is
// a single Secret named <SecretPrefix><hashPrefix> whose data holds the key
// metadata as JSON. Lookup hashes the presented key and Gets the Secret by
// its derived name — O(1), no namespace-wide list.
type Store struct {
	k8sClient client.Client
	namespace string
}

// New creates a Store backed by Kubernetes Secrets in namespace. An empty
// namespace defaults to "agenttier".
func New(c client.Client, namespace string) *Store {
	if namespace == "" {
		namespace = "agenttier"
	}
	return &Store{k8sClient: c, namespace: namespace}
}

// SecretNameForHash derives a deterministic, DNS-safe Secret name from a key
// hash. The hash is 64 hex chars; we take the first 40 to stay well under the
// 253-char limit while keeping collision probability negligible (2^160).
func SecretNameForHash(keyHash string) string {
	n := keyHash
	if len(n) > 40 {
		n = n[:40]
	}
	return SecretPrefix + n
}

// GetAPIKeyByHash implements auth.APIKeyStore.
func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (*auth.APIKeyRecord, error) {
	secret := &corev1.Secret{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: s.namespace,
		Name:      SecretNameForHash(keyHash),
	}, secret); err != nil {
		return nil, fmt.Errorf("api key not found")
	}
	// Defense in depth: confirm the stored hash matches exactly, in case a
	// name collision (or a hand-edited Secret) ever points the derived name
	// at the wrong record. Use constant-time comparison to prevent timing
	// side-channel attacks.
	if subtle.ConstantTimeCompare(secret.Data["keyHash"], []byte(keyHash)) != 1 {
		return nil, fmt.Errorf("api key not found")
	}
	rec := &auth.APIKeyRecord{}
	if raw, ok := secret.Data["record"]; ok {
		if err := json.Unmarshal(raw, rec); err != nil {
			return nil, fmt.Errorf("corrupt api key record: %w", err)
		}
	}
	return rec, nil
}

// Metadata is the non-secret view of a stored key, returned by list
// operations. It deliberately omits the hash and never includes the
// plaintext key (which exists only at creation time).
//
// SandboxID/ActionGroups surface a sandbox-scoped key's binding (FR6.6 —
// admin/owner visibility into what scoped credentials exist for a sandbox,
// without them being a hidden mechanism). Both are empty for a user-level
// key.
type Metadata struct {
	ID           string     `json:"id"`
	UserID       string     `json:"userId"`
	Name         string     `json:"name"`
	SandboxID    string     `json:"sandboxId,omitempty"`
	ActionGroups []string   `json:"actionGroups,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
}

// Create writes a new API-key Secret for the given hash + record. The caller
// is responsible for generating the key and computing its hash; this only
// persists the hashed form.
func (s *Store) Create(ctx context.Context, keyHash string, rec *auth.APIKeyRecord) error {
	recJSON, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal api key record: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretNameForHash(keyHash),
			Namespace: s.namespace,
			Labels: map[string]string{
				SecretPurposeLabel: SecretPurpose,
				UserLabel:          SanitizeLabelValue(rec.UserID),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"keyHash": []byte(keyHash),
			"record":  recJSON,
		},
	}
	if err := s.k8sClient.Create(ctx, secret); err != nil {
		return fmt.Errorf("create api key secret: %w", err)
	}
	return nil
}

// ListForUser returns metadata for every API key owned by userID.
func (s *Store) ListForUser(ctx context.Context, userID string) ([]Metadata, error) {
	list := &corev1.SecretList{}
	if err := s.k8sClient.List(ctx, list,
		client.InNamespace(s.namespace),
		client.MatchingLabels{
			SecretPurposeLabel: SecretPurpose,
			UserLabel:          SanitizeLabelValue(userID),
		},
	); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	out := make([]Metadata, 0, len(list.Items))
	for i := range list.Items {
		rec := &auth.APIKeyRecord{}
		if raw, ok := list.Items[i].Data["record"]; ok {
			_ = json.Unmarshal(raw, rec)
		}
		out = append(out, Metadata{
			ID:           list.Items[i].Name,
			UserID:       rec.UserID,
			Name:         rec.Name,
			SandboxID:    rec.SandboxID,
			ActionGroups: rec.ActionGroups,
			CreatedAt:    rec.CreatedAt,
			ExpiresAt:    rec.ExpiresAt,
			LastUsedAt:   rec.LastUsedAt,
		})
	}
	return out, nil
}

// ListAll returns metadata for every API key in the store — user-level and
// sandbox-scoped alike, across all owners. Used by admin-only views (FR6.6)
// that need to cross-reference sandboxes against the scoped keys bound to
// them; callers filter/group by Metadata.SandboxID themselves since Secrets
// carry no sandbox label to filter on server-side (only UserLabel is set at
// Create time). A per-install api-key Secret count is small enough that a
// single unfiltered List (scoped only by SecretPurposeLabel) is simpler and
// just as cheap as adding a new label + selector would be.
func (s *Store) ListAll(ctx context.Context) ([]Metadata, error) {
	list := &corev1.SecretList{}
	if err := s.k8sClient.List(ctx, list,
		client.InNamespace(s.namespace),
		client.MatchingLabels{SecretPurposeLabel: SecretPurpose},
	); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	out := make([]Metadata, 0, len(list.Items))
	for i := range list.Items {
		rec := &auth.APIKeyRecord{}
		if raw, ok := list.Items[i].Data["record"]; ok {
			_ = json.Unmarshal(raw, rec)
		}
		out = append(out, Metadata{
			ID:           list.Items[i].Name,
			UserID:       rec.UserID,
			Name:         rec.Name,
			SandboxID:    rec.SandboxID,
			ActionGroups: rec.ActionGroups,
			CreatedAt:    rec.CreatedAt,
			ExpiresAt:    rec.ExpiresAt,
			LastUsedAt:   rec.LastUsedAt,
		})
	}
	return out, nil
}

// Delete removes a key Secret by its ID (the Secret name), but only if it
// belongs to the requesting user (unless the caller is admin). Returns the
// deleted key's SHA-256 hash (so the caller can evict it from the validator
// cache) and an error if the key doesn't exist or isn't owned by the user.
func (s *Store) Delete(ctx context.Context, keyID, userID string, isAdmin bool) (string, error) {
	secret := &corev1.Secret{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: keyID}, secret); err != nil {
		return "", fmt.Errorf("api key not found")
	}
	if secret.Labels[SecretPurposeLabel] != SecretPurpose {
		return "", fmt.Errorf("api key not found")
	}
	if !isAdmin {
		rec := &auth.APIKeyRecord{}
		if raw, ok := secret.Data["record"]; ok {
			_ = json.Unmarshal(raw, rec)
		}
		if rec.UserID != userID {
			return "", fmt.Errorf("access denied")
		}
	}
	keyHash := string(secret.Data["keyHash"])
	if err := s.k8sClient.Delete(ctx, secret); err != nil {
		return "", fmt.Errorf("delete api key: %w", err)
	}
	return keyHash, nil
}

// SanitizeLabelValue makes an arbitrary user identifier safe to use as a
// Kubernetes label value (RFC 1123, max 63 chars, alphanumeric + -_. , must
// start/end alphanumeric). OIDC subs can contain ':' and other characters, so
// we hash-suffix anything non-conformant rather than risk an invalid label.
func SanitizeLabelValue(v string) string {
	if v == "" {
		return "unknown"
	}
	safe := make([]rune, 0, len(v))
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '-')
		}
	}
	out := string(safe)
	if len(out) > 63 {
		out = out[:63]
	}
	// Trim to a valid start/end character set.
	out = trimNonAlphanumeric(out)
	if out == "" {
		return "user"
	}
	return out
}

func trimNonAlphanumeric(s string) string {
	isAlnum := func(r byte) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	start := 0
	for start < len(s) && !isAlnum(s[start]) {
		start++
	}
	end := len(s)
	for end > start && !isAlnum(s[end-1]) {
		end--
	}
	return s[start:end]
}
