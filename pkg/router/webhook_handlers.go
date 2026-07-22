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
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	webhookdelivery "github.com/agenttier/agenttier/pkg/controller/webhook_delivery"
)

// FR5 Router-side webhook CRUD. The controller (pkg/controller/webhook_delivery,
// task #15) owns delivery, retry/backoff, and auto-disable — this file only
// implements POST/GET/DELETE /webhooks and GET /webhooks/{id}/deliveries,
// per design.md's Component Map ("Router side only").

// webhookStore returns a store bound to the install namespace, mirroring
// s.secretStore()'s pattern in apikey_handlers.go.
func (s *Server) webhookSubscriptionStore() *webhookStore {
	return newWebhookStore(s.k8sClient, s.config.InstallNamespace)
}

// webhookToJSON renders a webhookRecord for the API, optionally including
// the plaintext HMAC secret (only ever done once, at creation).
func webhookToJSON(rec *webhookRecord, secret string) map[string]interface{} {
	out := map[string]interface{}{
		"id":         rec.ID,
		"url":        rec.URL,
		"eventTypes": rec.EventTypes,
		"createdAt":  rec.CreatedAt,
		"disabled":   rec.Disabled,
	}
	if rec.SandboxID != "" {
		out["sandboxId"] = rec.SandboxID
	}
	if rec.Namespace != "" {
		out["namespace"] = rec.Namespace
	}
	if secret != "" {
		out["secret"] = secret
	}
	return out
}

// handleCreateWebhook implements POST /webhooks. Body:
// {"url","eventTypes":[...],"sandboxId"?,"namespace"?}. The HMAC secret is
// returned exactly once, here, and never again — mirrors the API-key /
// share-link "shown once" pattern.
func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		URL        string   `json:"url"`
		EventTypes []string `json:"eventTypes"`
		SandboxID  string   `json:"sandboxId,omitempty"`
		Namespace  string   `json:"namespace,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.URL == "" {
		respondError(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := validateWebhookEventTypes(req.EventTypes); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateWebhookURL(req.URL); err != nil {
		respondError(w, http.StatusBadRequest, "invalid url: "+err.Error())
		return
	}

	// Authorization gap fix (task #50, CRITICAL): without this check, any
	// authenticated caller could set sandboxId to a sandbox they don't own
	// and receive its lifecycle events, or omit both sandboxId/namespace to
	// receive events for every sandbox across every tenant. Mirrors the
	// exact ownership gate every other sandbox-scoped mutating endpoint uses
	// (PATCH/bulk-action/backups all call getSandboxWithAuthCheck).
	if req.SandboxID != "" {
		// A caller who doesn't own (or isn't admin for) the named sandbox
		// gets 404, not 403 — same "don't confirm existence to a caller
		// with no claim to it" convention used elsewhere (backup restore,
		// FR6's scoped-key middleware).
		if _, err := s.getSandboxWithAuthCheck(r.Context(), req.SandboxID, claims); err != nil {
			respondError(w, http.StatusNotFound, "sandbox not found")
			return
		}
	} else if !claims.IsAdmin {
		// No sandboxId (and no namespace-only scoping means "every sandbox
		// in that namespace", which is still a receive-everything-in-scope
		// case) is the "receive events cluster-wide" case — only admins may
		// register an unscoped subscription.
		respondError(w, http.StatusForbidden, "sandboxId is required for non-admin subscriptions")
		return
	}

	id, secret, err := s.webhookSubscriptionStore().Create(r.Context(), claims.Sub, webhookRecord{
		URL:        req.URL,
		EventTypes: req.EventTypes,
		SandboxID:  req.SandboxID,
		Namespace:  req.Namespace,
		IsAdmin:    claims.IsAdmin,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create webhook subscription: "+err.Error())
		return
	}

	rec := webhookRecord{ID: id, URL: req.URL, EventTypes: req.EventTypes, SandboxID: req.SandboxID, Namespace: req.Namespace, IsAdmin: claims.IsAdmin}
	out := webhookToJSON(&rec, secret)
	out["warning"] = "Store this secret now — it is shown only once."
	respondJSON(w, http.StatusCreated, out)
	s.recordAudit(r.Context(), claims, "webhook.create", "webhook", id)
}

// handleListWebhooks implements GET /webhooks: returns only the caller's
// own subscriptions (admins do not see other users' subscriptions here —
// FR5.5 doesn't provide an admin-list variant, unlike sandboxes/sharing).
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	recs, err := s.webhookSubscriptionStore().ListForUser(r.Context(), claims.Sub)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list webhook subscriptions: "+err.Error())
		return
	}
	results := make([]map[string]interface{}, 0, len(recs))
	for i := range recs {
		results = append(results, webhookToJSON(&recs[i], ""))
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"webhooks": results})
}

// handleDeleteWebhook implements DELETE /webhooks/{id}. Owner-or-admin.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id := mux.Vars(r)["id"]
	if err := s.webhookSubscriptionStore().Delete(r.Context(), id, claims.Sub, claims.IsAdmin); err != nil {
		if err.Error() == "access denied" {
			respondError(w, http.StatusForbidden, "you do not own this webhook subscription")
			return
		}
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
	s.recordAudit(r.Context(), claims, "webhook.delete", "webhook", id)
}

// handleGetWebhookDeliveries implements GET /webhooks/{id}/deliveries:
// recent delivery attempts + status, for debugging (FR5.5). The delivery
// history itself is written by the controller's delivery loop
// (pkg/controller/webhook_delivery) into a per-subscription bounded ring
// inside the well-known agenttier-webhook-cursor ConfigMap in the install
// namespace — read directly here rather than via any Router-controller RPC,
// since the ConfigMap's JSON shape is the stable cross-process contract
// between the two (the controller writes it, the Router only ever reads
// it). If the ConfigMap doesn't exist yet (webhook-delivery isn't enabled
// on this install, or nothing has been delivered yet) or this subscription
// has no history entry, the correct answer is a genuinely-empty array, not
// an error.
func (s *Server) handleGetWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id := mux.Vars(r)["id"]
	if _, err := s.webhookSubscriptionStore().Get(r.Context(), id, claims.Sub, claims.IsAdmin); err != nil {
		if err.Error() == "access denied" {
			respondError(w, http.StatusForbidden, "you do not own this webhook subscription")
			return
		}
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	deliveries, err := s.webhookDeliveryHistory(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read delivery history: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"deliveries": deliveries})
}

// webhookDeliveryHistory reads the controller's delivery-state ConfigMap
// and extracts the bounded history ring for one subscription ID. Returns an
// empty (never nil) slice, not an error, when the ConfigMap doesn't exist or
// the subscription has no recorded attempts — both are legitimate "nothing
// delivered yet" states, not failures.
func (s *Server) webhookDeliveryHistory(ctx context.Context, subscriptionID string) ([]webhookdelivery.DeliveryAttempt, error) {
	cm := &corev1.ConfigMap{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{
		Name:      webhookdelivery.StateConfigMapName,
		Namespace: s.config.InstallNamespace,
	}, cm)
	if apierrors.IsNotFound(err) {
		return []webhookdelivery.DeliveryAttempt{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get webhook delivery state configmap: %w", err)
	}

	raw, ok := cm.Data["state"]
	if !ok || raw == "" {
		return []webhookdelivery.DeliveryAttempt{}, nil
	}
	var state struct {
		Subscriptions map[string]struct {
			History []webhookdelivery.DeliveryAttempt `json:"history"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("parse webhook delivery state configmap: %w", err)
	}
	sub, ok := state.Subscriptions[subscriptionID]
	if !ok || sub.History == nil {
		return []webhookdelivery.DeliveryAttempt{}, nil
	}
	return sub.History, nil
}
