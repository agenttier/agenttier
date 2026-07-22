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
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Webhook subscriptions are stored as Secrets in the install namespace, one
// per subscription — mirroring pkg/apikeystore's pattern (DL1: no CRD, to
// avoid the api/ codegen lockstep). The controller's delivery loop
// (pkg/controller/webhook_delivery, task #15) reads these by label-list; the
// Router owns only the CRUD surface in this file.
const (
	webhookSecretPurposeLabel = "agenttier.io/secret-purpose" //nolint:gosec // G101: a label key, not a credential
	webhookSecretPurpose      = "webhook-subscription"        //nolint:gosec // G101: a label value, not a credential
	webhookOwnerLabel         = "agenttier.io/webhook-owner"  //nolint:gosec // G101: a label key, not a credential
	webhookSecretPrefix       = "agenttier-webhook-"          //nolint:gosec // G101: a Secret name prefix, not a credential
)

// webhookHMACSecretBytes is the CSPRNG entropy size for a subscription's
// HMAC signing secret, per design.md#FR5 ("32B CSPRNG, base64url").
const webhookHMACSecretBytes = 32

// webhookRecord is the persisted (non-secret-header) shape of a webhook
// subscription, stored as JSON under the Secret's "record" key. The HMAC
// signing secret is stored separately under "secret" so list operations can
// omit it trivially.
type webhookRecord struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	URL        string    `json:"url"`
	EventTypes []string  `json:"eventTypes"`
	SandboxID  string    `json:"sandboxId,omitempty"`
	Namespace  string    `json:"namespace,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Disabled   bool      `json:"disabled,omitempty"`
	// IsAdmin captures whether the creating caller was an admin at
	// subscription-creation time (task #50). The controller's delivery
	// loop (pkg/controller/webhook_delivery) reads this to allow an
	// admin's unscoped subscription through its independent
	// defense-in-depth access check, which otherwise has no other way to
	// know the subscriber's admin status (it only ever sees the persisted
	// Secret, never a live Claims object).
	IsAdmin bool `json:"isAdmin,omitempty"`
}

// webhookStore implements Secret-backed CRUD for webhook subscriptions.
type webhookStore struct {
	k8sClient client.Client
	namespace string
}

func newWebhookStore(c client.Client, namespace string) *webhookStore {
	if namespace == "" {
		namespace = "agenttier"
	}
	return &webhookStore{k8sClient: c, namespace: namespace}
}

// webhookAllowedEventTypes is the fixed FR5.2 vocabulary. Kept as a set
// (not an enum) so a future addition is a one-line change, matching the
// Python SDK's webhooks.py convention.
var webhookAllowedEventTypes = map[string]bool{
	"sandbox.creating": true, "sandbox.running": true, "sandbox.stopped": true,
	"sandbox.error": true, "sandbox.deleting": true,
	"backup.created": true, "backup.pruned": true,
	"share.granted": true, "share.revoked": true,
	"agent.invoke.started": true, "agent.invoke.completed": true, "agent.invoke.failed": true,
}

// validateWebhookEventTypes rejects an empty list or any type outside the
// fixed vocabulary. Called at creation time (FR1.10-style local validation
// mirrored server-side since this is the authoritative enforcement point).
func validateWebhookEventTypes(types []string) error {
	if len(types) == 0 {
		return fmt.Errorf("eventTypes must contain at least one event type")
	}
	for _, t := range types {
		if !webhookAllowedEventTypes[t] {
			return fmt.Errorf("unknown event type %q", t)
		}
	}
	return nil
}

// privateOrReservedNetworks are the IP ranges validateWebhookURL rejects —
// loopback, link-local (incl. the cloud-metadata range), and RFC 1918
// private space, plus their IPv6 equivalents. sa-review.md Medium finding
// #5 (SSRF): the controller's delivery loop has broader network reach than
// a sandboxed workload, so a subscription URL resolving into any of these
// ranges must never be dialed.
var privateOrReservedNetworks = mustParseCIDRs(
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, includes 169.254.169.254 cloud metadata
	"10.0.0.0/8",     // RFC 1918
	"172.16.0.0/12",  // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"::1/128",        // IPv6 loopback
	"fe80::/10",      // IPv6 link-local
	"fc00::/7",       // IPv6 unique local (RFC 1918 equivalent)
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("webhook_store: invalid CIDR literal %q: %v", c, err))
		}
		out = append(out, ipnet)
	}
	return out
}

// validateWebhookURL enforces the SSRF guard from sa-review.md Medium
// finding #5: https:// only, and the hostname must not resolve (via real
// DNS lookup, not string-matching — DNS rebinding defeats a naive hostname
// check) into a loopback/link-local/private/cloud-metadata range. Called
// both at subscription-creation time (here) and, per the finding, must be
// re-called before each delivery by the controller's delivery loop (task
// #15) since DNS answers can change between creation and delivery.
func validateWebhookURL(raw string) error {
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

// generateWebhookSecret returns a new random HMAC signing secret. A
// crypto/rand.Read failure is a hard error — sa-review.md Medium finding
// #3 warns against ever falling through with a short/empty secret, so this
// mirrors generateAPIKey's existing error-checked pattern exactly
// (apikey_handlers.go).
func generateWebhookSecret() (string, error) {
	b := make([]byte, webhookHMACSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// secretNameForWebhookID derives a deterministic, DNS-safe Secret name from
// a subscription ID. Safe by construction: generateWebhookID only ever
// produces lowercase hex (`[0-9a-f]`), and webhookSecretPrefix is itself
// lowercase-alphanumeric-and-hyphen, so the concatenation always satisfies
// RFC 1123 (K8s object names: lowercase alphanumeric + `-`, must start/end
// alphanumeric) — see TestGenerateWebhookID_SecretNameAlwaysRFC1123Safe.
func secretNameForWebhookID(id string) string {
	return webhookSecretPrefix + id
}

// generateWebhookID returns a random subscription ID safe to embed in a
// Kubernetes object name (via secretNameForWebhookID). Uses lowercase hex
// (encoding/hex, always `[0-9a-f]`) rather than base64.RawURLEncoding —
// base64url's alphabet includes uppercase letters and `-`/`_`, and a
// "wh_"-style prefix's underscore alone violates RFC 1123, both of which
// previously made the derived Secret name invalid on a real apiserver
// (100% reproducible, not intermittent — see decisions.md DL11). Mirrors
// pkg/apikeystore.SecretNameForHash's existing hex-string idiom for the
// same reason: a hash/ID that's going to be concatenated into a K8s object
// name should be RFC-1123-safe by construction, not by hoping the caller
// remembers to sanitize.
func generateWebhookID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate webhook id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Create persists a new webhook subscription. Returns the generated ID and
// plaintext HMAC secret (shown exactly once — never persisted anywhere but
// this Secret, and never returned again after this call).
func (s *webhookStore) Create(ctx context.Context, userID string, rec webhookRecord) (id, secret string, err error) {
	id, err = generateWebhookID()
	if err != nil {
		return "", "", err
	}
	secret, err = generateWebhookSecret()
	if err != nil {
		return "", "", err
	}
	rec.ID = id
	rec.UserID = userID
	rec.CreatedAt = time.Now()

	recJSON, err := json.Marshal(rec)
	if err != nil {
		return "", "", fmt.Errorf("marshal webhook record: %w", err)
	}
	k8sSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretNameForWebhookID(id),
			Namespace: s.namespace,
			Labels: map[string]string{
				webhookSecretPurposeLabel: webhookSecretPurpose,
				webhookOwnerLabel:         sanitizeLabelValue(userID),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"record": recJSON,
			"secret": []byte(secret),
		},
	}
	if err := s.k8sClient.Create(ctx, k8sSecret); err != nil {
		return "", "", fmt.Errorf("create webhook secret: %w", err)
	}
	return id, secret, nil
}

// ListForUser returns every subscription owned by userID. The HMAC secret
// is never included.
func (s *webhookStore) ListForUser(ctx context.Context, userID string) ([]webhookRecord, error) {
	list := &corev1.SecretList{}
	if err := s.k8sClient.List(ctx, list,
		client.InNamespace(s.namespace),
		client.MatchingLabels{
			webhookSecretPurposeLabel: webhookSecretPurpose,
			webhookOwnerLabel:         sanitizeLabelValue(userID),
		},
	); err != nil {
		return nil, fmt.Errorf("list webhook subscriptions: %w", err)
	}
	out := make([]webhookRecord, 0, len(list.Items))
	for i := range list.Items {
		var rec webhookRecord
		if raw, ok := list.Items[i].Data["record"]; ok {
			if err := json.Unmarshal(raw, &rec); err == nil {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// Get returns one subscription by ID, scoped to userID (unless isAdmin) so
// a caller cannot probe another user's subscription by guessing its ID.
// Returns the record and the HMAC secret is deliberately not part of this
// return value — callers that need it (delivery, deliveries listing) never
// need the secret through this path; the controller (a separate process)
// reads Secrets directly.
func (s *webhookStore) Get(ctx context.Context, id, userID string, isAdmin bool) (*webhookRecord, error) {
	secret := &corev1.Secret{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: secretNameForWebhookID(id)}, secret); err != nil {
		return nil, fmt.Errorf("webhook subscription not found")
	}
	if secret.Labels[webhookSecretPurposeLabel] != webhookSecretPurpose {
		return nil, fmt.Errorf("webhook subscription not found")
	}
	var rec webhookRecord
	if raw, ok := secret.Data["record"]; ok {
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("corrupt webhook record: %w", err)
		}
	}
	if !isAdmin && rec.UserID != userID {
		return nil, fmt.Errorf("access denied")
	}
	return &rec, nil
}

// Delete removes a subscription by ID, scoped to userID unless isAdmin.
func (s *webhookStore) Delete(ctx context.Context, id, userID string, isAdmin bool) error {
	secret := &corev1.Secret{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: secretNameForWebhookID(id)}, secret); err != nil {
		return fmt.Errorf("webhook subscription not found")
	}
	if secret.Labels[webhookSecretPurposeLabel] != webhookSecretPurpose {
		return fmt.Errorf("webhook subscription not found")
	}
	if !isAdmin {
		var rec webhookRecord
		if raw, ok := secret.Data["record"]; ok {
			_ = json.Unmarshal(raw, &rec)
		}
		if rec.UserID != userID {
			return fmt.Errorf("access denied")
		}
	}
	if err := s.k8sClient.Delete(ctx, secret); err != nil {
		return fmt.Errorf("delete webhook subscription: %w", err)
	}
	return nil
}
