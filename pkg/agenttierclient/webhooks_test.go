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

func TestCreateListDeleteWebhook(t *testing.T) {
	var gotBody CreateWebhookRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/webhooks":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "wh1", "secret": "shown-once-secret", "url": gotBody.URL, "eventTypes": gotBody.EventTypes,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/webhooks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"webhooks": []map[string]any{{"id": "wh1", "url": "https://example.com/hook"}},
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	created, err := c.CreateWebhook(context.Background(), CreateWebhookRequest{
		URL:        "https://example.com/hook",
		EventTypes: []string{"sandbox.running", "sandbox.stopped"},
	})
	if err != nil {
		t.Fatalf("CreateWebhook() error = %v", err)
	}
	if created.Secret != "shown-once-secret" {
		t.Fatalf("created.Secret = %q, want shown-once-secret", created.Secret)
	}

	list, err := c.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "wh1" {
		t.Fatalf("list = %+v", list)
	}
	// Secret should never appear on the list response.
	if list[0].Secret != "" {
		t.Errorf("list[0].Secret = %q, want empty (list must never return the secret)", list[0].Secret)
	}

	if err := c.DeleteWebhook(context.Background(), "wh1"); err != nil {
		t.Fatalf("DeleteWebhook() error = %v", err)
	}
}

func TestGetWebhookDeliveries(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/webhooks/wh1/deliveries" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deliveries": []map[string]any{
				{"eventType": "sandbox.running", "statusCode": 200, "attempt": 1, "success": true},
			},
		})
	})
	deliveries, err := c.GetWebhookDeliveries(context.Background(), "wh1")
	if err != nil {
		t.Fatalf("GetWebhookDeliveries() error = %v", err)
	}
	if len(deliveries) != 1 || !deliveries[0].Success {
		t.Fatalf("deliveries = %+v", deliveries)
	}
}
