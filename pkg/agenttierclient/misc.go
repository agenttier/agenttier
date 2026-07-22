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

package agenttierclient

import (
	"context"
	"net/url"
)

// User identity, preferences, and API-key management. Wire shapes mirror
// pkg/router/handlers.go (handleGetMe, handleGetPreferences,
// handleUpdatePreferences) and pkg/router/apikey_handlers.go.

// --- identity + preferences ---------------------------------------------

// CurrentUser is the response of GET /user/me.
type CurrentUser struct {
	Sub     string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	IsAdmin bool     `json:"isAdmin"`
}

// GetCurrentUser issues GET /user/me.
func (c *Client) GetCurrentUser(ctx context.Context) (*CurrentUser, error) {
	var out CurrentUser
	if err := c.Get(ctx, "/user/me", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPreferences issues GET /user/preferences. The Router stores
// preferences as a free-form JSON object, so this returns the decoded map
// as-is rather than a fixed struct.
func (c *Client) GetPreferences(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.Get(ctx, "/user/preferences", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdatePreferences issues PUT /user/preferences, replacing the caller's
// stored preferences wholesale and returning the value the Router
// persisted.
func (c *Client) UpdatePreferences(ctx context.Context, prefs map[string]any) (map[string]any, error) {
	var out map[string]any
	if err := c.Put(ctx, "/user/preferences", prefs, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- API keys ------------------------------------------------------------

// APIKeyMetadata describes an API key without ever exposing its secret —
// see apikeystore.Metadata / handleListAPIKeys.
type APIKeyMetadata struct {
	ID           string   `json:"id"`
	UserID       string   `json:"userId,omitempty"`
	Name         string   `json:"name,omitempty"`
	CreatedAt    string   `json:"createdAt,omitempty"`
	ExpiresAt    string   `json:"expiresAt,omitempty"`
	LastUsedAt   string   `json:"lastUsedAt,omitempty"`
	SandboxID    string   `json:"sandboxId,omitempty"`
	ActionGroups []string `json:"actionGroups,omitempty"`
}

// ListAPIKeys issues GET /user/api-keys.
func (c *Client) ListAPIKeys(ctx context.Context) ([]APIKeyMetadata, error) {
	var out struct {
		Keys []APIKeyMetadata `json:"keys"`
	}
	if err := c.Get(ctx, "/user/api-keys", &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// CreateAPIKeyRequest is the body of POST /user/api-keys. SandboxID and
// ActionGroups are the FR6 sandbox-scoped-key extension (design.md#FR6);
// leaving both empty mints a regular user-level key, unchanged from
// today's behavior.
type CreateAPIKeyRequest struct {
	Name         string   `json:"name,omitempty"`
	ExpiresIn    string   `json:"expiresIn,omitempty"`
	SandboxID    string   `json:"sandboxId,omitempty"`
	ActionGroups []string `json:"actionGroups,omitempty"`
}

// CreateAPIKeyResult is the response of POST /user/api-keys. Key is the
// plaintext credential, shown exactly once (NFR10) — callers must persist
// it immediately.
type CreateAPIKeyResult struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

// CreateAPIKey issues POST /user/api-keys.
func (c *Client) CreateAPIKey(ctx context.Context, req CreateAPIKeyRequest) (*CreateAPIKeyResult, error) {
	var out CreateAPIKeyResult
	if err := c.Post(ctx, "/user/api-keys", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeAPIKey issues DELETE /user/api-keys/{keyId}.
func (c *Client) RevokeAPIKey(ctx context.Context, keyID string) error {
	return c.Delete(ctx, "/user/api-keys/"+url.PathEscape(keyID), nil)
}
