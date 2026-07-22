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
	"encoding/json"
	"net/http"
	"testing"
)

func TestGetCurrentUser(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "user-1", "email": "user@example.com", "isAdmin": false,
		})
	})
	out, err := c.GetCurrentUser(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentUser() error = %v", err)
	}
	if out.Sub != "user-1" || out.IsAdmin {
		t.Fatalf("out = %+v", out)
	}
}

func TestGetSetPreferences(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"theme": "dark"})
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(gotBody)
		}
	})
	prefs, err := c.GetPreferences(context.Background())
	if err != nil {
		t.Fatalf("GetPreferences() error = %v", err)
	}
	if prefs["theme"] != "dark" {
		t.Fatalf("prefs = %+v", prefs)
	}
	updated, err := c.UpdatePreferences(context.Background(), map[string]any{"theme": "light"})
	if err != nil {
		t.Fatalf("UpdatePreferences() error = %v", err)
	}
	if updated["theme"] != "light" {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestAPIKeysLifecycle(t *testing.T) {
	var gotBody CreateAPIKeyRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]any{{"id": "key1", "name": "ci"}},
			})
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "key2", "key": "atk_shown-once", "name": gotBody.Name,
			})
		case r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "revoked", "id": "key1"})
		}
	})
	keys, err := c.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys() error = %v", err)
	}
	if len(keys) != 1 || keys[0].ID != "key1" {
		t.Fatalf("keys = %+v", keys)
	}

	created, err := c.CreateAPIKey(context.Background(), CreateAPIKeyRequest{Name: "ci-2"})
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if created.Key != "atk_shown-once" {
		t.Fatalf("created = %+v", created)
	}

	if err := c.RevokeAPIKey(context.Background(), "key1"); err != nil {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}
}

func TestCreateAPIKey_ScopedFields(t *testing.T) {
	var gotBody CreateAPIKeyRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "key3", "key": "atk_scoped"})
	})
	_, err := c.CreateAPIKey(context.Background(), CreateAPIKeyRequest{
		SandboxID:    "sbx-1",
		ActionGroups: []string{"run-command", "files:read"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if gotBody.SandboxID != "sbx-1" || len(gotBody.ActionGroups) != 2 {
		t.Fatalf("gotBody = %+v", gotBody)
	}
}
