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

// Webhook subscription CRUD (FR5). Wire shapes mirror design.md's
// Interface Contracts section — the Router-side handlers land in Group 2
// of this build (pkg/router/webhook_handlers.go) in parallel with this
// client; the contract there is fixed by design.md so this file doesn't
// block on that landing.

// CreateWebhookRequest is the body of POST /webhooks.
type CreateWebhookRequest struct {
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	SandboxID  string   `json:"sandboxId,omitempty"`
	Namespace  string   `json:"namespace,omitempty"`
}

// Webhook is a subscription as returned by list/get. Secret is only ever
// populated on the create response (shown once).
type Webhook struct {
	ID         string   `json:"id"`
	Secret     string   `json:"secret,omitempty"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	SandboxID  string   `json:"sandboxId,omitempty"`
	Namespace  string   `json:"namespace,omitempty"`
	CreatedAt  string   `json:"createdAt,omitempty"`
	Disabled   bool     `json:"disabled,omitempty"`
}

// CreateWebhook issues POST /webhooks. The returned Webhook.Secret is the
// HMAC signing secret, shown exactly once — callers must persist it
// immediately (NFR10).
func (c *Client) CreateWebhook(ctx context.Context, req CreateWebhookRequest) (*Webhook, error) {
	var out Webhook
	if err := c.Post(ctx, "/webhooks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListWebhooks issues GET /webhooks (returns the caller's own subscriptions).
func (c *Client) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	var out struct {
		Webhooks []Webhook `json:"webhooks"`
	}
	if err := c.Get(ctx, "/webhooks", &out); err != nil {
		return nil, err
	}
	return out.Webhooks, nil
}

// DeleteWebhook issues DELETE /webhooks/{id}.
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	return c.Delete(ctx, "/webhooks/"+url.PathEscape(id), nil)
}

// WebhookDelivery is one delivery attempt, as returned by
// GetWebhookDeliveries — see design.md's FR5 Interface Contracts.
type WebhookDelivery struct {
	EventType  string `json:"eventType"`
	Timestamp  string `json:"timestamp"`
	StatusCode int    `json:"statusCode"`
	Attempt    int    `json:"attempt"`
	Success    bool   `json:"success"`
}

// GetWebhookDeliveries issues GET /webhooks/{id}/deliveries.
func (c *Client) GetWebhookDeliveries(ctx context.Context, id string) ([]WebhookDelivery, error) {
	var out struct {
		Deliveries []WebhookDelivery `json:"deliveries"`
	}
	if err := c.Get(ctx, "/webhooks/"+url.PathEscape(id)+"/deliveries", &out); err != nil {
		return nil, err
	}
	return out.Deliveries, nil
}
