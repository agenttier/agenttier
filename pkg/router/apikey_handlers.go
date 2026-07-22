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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/agenttier/agenttier/pkg/router/auth"
)

// apiKeyPlaintextPrefix tags generated keys so they're recognizable in logs /
// support tickets and so a leaked key is greppable. The remainder is 32 bytes
// of CSPRNG entropy, base64url-encoded.
const apiKeyPlaintextPrefix = "atk_"

// ValidActionGroups is the sandbox-scoped API-key action-group vocabulary
// (FR6.1/DD3): run-command, files:read, files:write, ports, agent:invoke,
// agent:configure, resume, stop. "delete" is deliberately NOT a member —
// per FR6.1.1/DD3, a scoped key must never be able to destroy the sandbox
// that backs it, so the vocabulary excludes it entirely rather than just
// leaving it out of the default set. handleCreateAPIKey rejects any
// unrecognized value (including "delete") with 400 at mint time; this is
// defense-in-depth beyond requireSandboxScope's route-map enforcement (which
// independently 403s any scoped key on DELETE /sandboxes/{id} because no
// route maps to a delete-shaped action) — a stored record must never carry
// "delete" even if a future code path bypassed the middleware check.
var ValidActionGroups = map[string]bool{
	"run-command":     true,
	"files:read":      true,
	"files:write":     true,
	"ports":           true,
	"agent:invoke":    true,
	"agent:configure": true,
	"resume":          true,
	"stop":            true,
}

// generateAPIKey returns a new random API key string (plaintext, shown once).
func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return apiKeyPlaintextPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// secretStore narrows s.apiKeyValidator's store back to the concrete
// secretAPIKeyStore so the handlers can call its create/list/delete helpers.
// The validator only needs the read interface; the handlers need the writes.
func (s *Server) secretStore() *secretAPIKeyStore {
	return newSecretAPIKeyStore(s.k8sClient, s.config.InstallNamespace)
}

// handleListAPIKeys returns metadata (never the key or its hash) for every
// API key owned by the authenticated user.
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	keys, err := s.secretStore().listAPIKeysForUser(r.Context(), claims.Sub)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list API keys: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"keys": keys})
}

// handleCreateAPIKey mints a new API key for the authenticated user. The
// plaintext key is returned EXACTLY ONCE in this response; only its SHA-256
// hash is persisted (in a Secret). There is no way to recover the key later.
//
// Optional sandboxId + scopes fields mint a sandbox-scoped key (FR6.1)
// instead of a user-level one: the caller must own (or admin) the named
// sandbox, and every requested scope must be a recognized action group —
// "delete" is rejected explicitly (400) even though it's also absent from
// ValidActionGroups, so the error message is specific rather than a generic
// "unknown scope". A request with sandboxId but empty/absent scopes mints a
// key with the FR6.1.1 default action-group set (resume/stop included, so
// the sandbox can resume/stop itself without its owner's user-level key).
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req struct {
		Name      string   `json:"name,omitempty"`
		ExpiresIn string   `json:"expiresIn,omitempty"` // optional Go duration, e.g. "720h"
		SandboxID string   `json:"sandboxId,omitempty"`
		Scopes    []string `json:"scopes,omitempty"`
	}
	if r.ContentLength > 0 {
		// DisallowUnknownFields so a misspelled/wrong field name (e.g.
		// "actionGroups" instead of "scopes") 400s instead of silently
		// decoding to the zero value. Scoped to this handler only: a
		// wrong-named scopes field would otherwise leave req.Scopes nil,
		// which falls through to defaultScopedKeyActionGroups() and mints a
		// key with the full default 8-group set instead of the caller's
		// intended narrow scope — a least-privilege violation on a
		// credential-minting endpoint, not just a cosmetic decode error.
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	var expiresAt *time.Time
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			respondError(w, http.StatusBadRequest, "expiresIn must be a positive Go duration (e.g. \"720h\")")
			return
		}
		t := time.Now().Add(d)
		expiresAt = &t
	}

	actionGroups := req.Scopes
	if req.SandboxID != "" {
		// Minting a sandbox-scoped key: verify the caller may act on this
		// sandbox (owner or admin — same gate as any other sandbox
		// mutation) before handing out a credential bound to it.
		if _, err := s.getSandboxWithAuthCheck(r.Context(), req.SandboxID, claims); err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		if len(actionGroups) == 0 {
			actionGroups = defaultScopedKeyActionGroups()
		}
		for _, scope := range actionGroups {
			if scope == "delete" {
				respondError(w, http.StatusBadRequest, `"delete" is not a valid action group — a sandbox-scoped key can never delete the sandbox it is bound to`)
				return
			}
			if !ValidActionGroups[scope] {
				respondError(w, http.StatusBadRequest, "unknown action group: "+scope)
				return
			}
		}
	} else if len(req.Scopes) > 0 {
		// scopes without sandboxId doesn't map to any supported key type
		// (FR6.1: a scoped key carries exactly one sandboxId). Reject
		// rather than silently minting an unscoped user-level key that
		// ignores the caller's requested scopes.
		respondError(w, http.StatusBadRequest, "scopes requires sandboxId — user-level keys do not carry action-group scopes")
		return
	}

	plaintext, err := generateAPIKey()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate key")
		return
	}
	keyHash := auth.HashAPIKey(plaintext)

	rec := &auth.APIKeyRecord{
		KeyHash:      keyHash,
		UserID:       claims.Sub,
		Email:        claims.Email,
		Name:         req.Name,
		SandboxID:    req.SandboxID,
		ActionGroups: actionGroups,
		CreatedAt:    time.Now(),
		ExpiresAt:    expiresAt,
	}
	if err := s.secretStore().createAPIKeySecret(r.Context(), keyHash, rec); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to store API key: "+err.Error())
		return
	}

	// The plaintext key is returned once, here, and never again.
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":           secretNameForHash(keyHash),
		"key":          plaintext,
		"name":         rec.Name,
		"sandboxId":    rec.SandboxID,
		"actionGroups": rec.ActionGroups,
		"createdAt":    rec.CreatedAt,
		"expiresAt":    rec.ExpiresAt,
		"warning":      "Store this key now — it cannot be retrieved again.",
	})
}

// defaultScopedKeyActionGroups is the action-group set auto-granted when a
// sandbox-scoped key is minted without explicit scopes (FR6.1.1): resume and
// stop are included by default so an agent holding its own sandbox's scoped
// key can resume/stop itself without needing its owner's user-level key —
// this resolves the self-lockout edge case (a stopped sandbox's own key can
// still bring it back up). delete is never included (DD3).
func defaultScopedKeyActionGroups() []string {
	return []string{
		"run-command", "files:read", "files:write", "ports",
		"agent:invoke", "agent:configure", "resume", "stop",
	}
}

// handleRevokeAPIKey deletes an API key by its ID. Non-admins may only revoke
// their own keys; admins may revoke any.
func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	keyID := mux.Vars(r)["keyId"]
	if keyID == "" {
		respondError(w, http.StatusBadRequest, "keyId is required")
		return
	}
	keyHash, err := s.secretStore().deleteAPIKey(r.Context(), keyID, claims.Sub, claims.IsAdmin)
	if err != nil {
		// Map ownership failures to 403, everything else to 404/500-ish 400.
		if err.Error() == "access denied" {
			respondError(w, http.StatusForbidden, "you do not own this API key")
			return
		}
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	// Evict the cache so the revoked key stops authenticating immediately
	// rather than lingering until its cache entry ages out (~cacheTTL).
	if s.apiKeyValidator != nil && keyHash != "" {
		s.apiKeyValidator.InvalidateHash(keyHash)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "revoked", "id": keyID})
}
