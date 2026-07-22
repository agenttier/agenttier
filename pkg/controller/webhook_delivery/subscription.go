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

// Package webhookdelivery implements the FR5 leader-elected delivery loop
// for webhook subscriptions: it reads Secret-backed subscriptions the
// Router writes (pkg/router/webhook_store.go), watches Sandbox lifecycle
// events, matches them against subscription filters, and delivers signed
// HTTP POSTs with retry/backoff and consecutive-failure auto-disable.
//
// Delivery runs here (not the Router) because the Router is stateless and
// multi-replica — an at-least-once single-cursor deliverer there would race
// across replicas. The controller is leader-elected and already owns the
// reconcile loop that produces the events being delivered (DL4).
package webhookdelivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Subscription mirrors the read side of pkg/router/webhook_store.go's
// webhookRecord JSON shape exactly (same field names/tags) so this package
// can decode the Secrets the Router writes without importing an unexported
// type across packages. The Router owns the CRUD surface and is the source
// of truth for the schema — see webhook_store.go's webhookRecord.
type Subscription struct {
	ID         string   `json:"id"`
	UserID     string   `json:"userId"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	SandboxID  string   `json:"sandboxId,omitempty"`
	Namespace  string   `json:"namespace,omitempty"`
	Disabled   bool     `json:"disabled,omitempty"`
	// IsAdmin mirrors pkg/router/webhook_store.go's webhookRecord.IsAdmin —
	// whether the subscriber was an admin at creation time. Read by
	// canAccessSandbox (access.go) so an admin's unscoped subscription
	// isn't rejected by the delivery loop's own defense-in-depth access
	// check (task #50): the Router already gates unscoped subscriptions to
	// admins only, but this loop has no live Claims object to re-derive
	// that from — only the persisted record.
	IsAdmin bool `json:"isAdmin,omitempty"`
}

// Matches reports whether this subscription should receive an event of the
// given type for a sandbox in the given namespace with the given name, per
// FR5.1's filter model (event types, optionally scoped to a sandboxId or
// namespace).
func (s Subscription) Matches(eventType, sandboxNamespace, sandboxName string) bool {
	if s.Disabled {
		return false
	}
	found := false
	for _, t := range s.EventTypes {
		if t == eventType {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if s.SandboxID != "" && s.SandboxID != sandboxName {
		return false
	}
	if s.Namespace != "" && s.Namespace != sandboxNamespace {
		return false
	}
	return true
}

const (
	subscriptionSecretPurposeLabel = "agenttier.io/secret-purpose" //nolint:gosec // G101: a label key, not a credential
	subscriptionSecretPurpose      = "webhook-subscription"        //nolint:gosec // G101: a label value, not a credential
	// subscriptionSecretPrefix mirrors webhook_store.go's
	// secretNameForWebhookID exactly — the Secret name is deterministic
	// per subscription ID, which is what lets DisableSubscription target
	// the right object without a separate index.
	subscriptionSecretPrefix = "agenttier-webhook-" //nolint:gosec // G101: a Secret name prefix, not a credential
)

func secretNameForSubscriptionID(id string) string {
	return subscriptionSecretPrefix + id
}

// ListSubscriptions reads every webhook-subscription Secret in namespace
// (the install namespace) and returns the decoded records plus their HMAC
// signing secrets, keyed by subscription ID. Corrupt/unparseable records
// are skipped (logged by the caller) rather than aborting the whole list —
// one bad Secret must not block delivery to every other subscription.
func ListSubscriptions(ctx context.Context, c client.Client, namespace string) (map[string]Subscription, map[string]string, error) {
	list := &corev1.SecretList{}
	if err := c.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{subscriptionSecretPurposeLabel: subscriptionSecretPurpose},
	); err != nil {
		return nil, nil, fmt.Errorf("list webhook subscription secrets: %w", err)
	}
	subs := make(map[string]Subscription, len(list.Items))
	secrets := make(map[string]string, len(list.Items))
	for i := range list.Items {
		raw, ok := list.Items[i].Data["record"]
		if !ok {
			continue
		}
		var rec Subscription
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		if rec.ID == "" {
			continue
		}
		subs[rec.ID] = rec
		if secretBytes, ok := list.Items[i].Data["secret"]; ok {
			secrets[rec.ID] = string(secretBytes)
		}
	}
	return subs, secrets, nil
}

// privateOrReservedNetworks mirrors pkg/router/webhook_store.go's SSRF
// allowlist exactly (loopback, link-local incl. cloud metadata, RFC 1918
// private space, and IPv6 equivalents). Duplicated rather than imported
// because the Router's validateWebhookURL is unexported and the two
// packages have no shared home today; keep both lists in sync if either
// changes (see sa-review.md Medium finding #5 / decisions.md DL7).
var privateOrReservedNetworks = mustParseCIDRs(
	"127.0.0.0/8",
	"169.254.0.0/16",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"::1/128",
	"fe80::/10",
	"fc00::/7",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("webhookdelivery: invalid CIDR literal %q: %v", c, err))
		}
		out = append(out, ipnet)
	}
	return out
}

// ValidateDeliveryURL re-validates a subscription's URL immediately before
// each delivery attempt (sa-review.md Medium finding #5 / DL7): the Router
// already validates at subscription-creation time, but DNS answers can
// change between creation and any given delivery (DNS rebinding), so the
// controller — which has broader network reach than a sandboxed workload —
// must re-resolve and re-check on every attempt rather than trusting a
// stored URL forever. Requires https://, and rejects any resolved address
// in a loopback/link-local/private/cloud-metadata range.
func ValidateDeliveryURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("url must use https://")
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("url must have a host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q did not resolve to any address", host)
	}
	for _, ip := range ips {
		for _, network := range privateOrReservedNetworks {
			if network.Contains(ip) {
				return fmt.Errorf("host %q resolves to a disallowed address range (%s)", host, ip)
			}
		}
	}
	return nil
}

// DisableSubscription flips the Disabled flag on a subscription's Secret
// record in place (FR5.4: a subscription that fails repeatedly is
// auto-disabled and surfaced to the owner rather than retried forever).
// This mutates the Router-owned Secret directly rather than going through
// an HTTP call — the controller and Router are separate processes with no
// RPC between them, and the Secret is the shared source of truth both
// sides already read/write (the Router via webhook_store.go, the
// controller here).
func DisableSubscription(ctx context.Context, c client.Client, namespace, id string) error {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretNameForSubscriptionID(id)}, secret); err != nil {
		return fmt.Errorf("get webhook subscription secret: %w", err)
	}
	raw, ok := secret.Data["record"]
	if !ok {
		return fmt.Errorf("webhook subscription secret %s has no record", id)
	}
	var rec Subscription
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("parse webhook subscription record: %w", err)
	}
	if rec.Disabled {
		return nil
	}
	rec.Disabled = true
	updated, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("serialize webhook subscription record: %w", err)
	}
	secret.Data["record"] = updated
	if err := c.Update(ctx, secret); err != nil {
		return fmt.Errorf("update webhook subscription secret: %w", err)
	}
	return nil
}
