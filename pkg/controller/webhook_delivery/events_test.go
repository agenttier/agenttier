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

package webhookdelivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fixedFR5Vocabulary mirrors pkg/router/webhook_store.go's
// webhookAllowedEventTypes — duplicated here (rather than imported) since
// pkg/controller cannot depend on pkg/router, but the two sets must stay in
// lockstep: every entry eventReasonToWebhookType maps TO must be one of
// these 12, and (post task #40/#41/#42/#43) every one of these 12 must be
// reachable FROM some Reason in the map — no gaps, no stray entries.
var fixedFR5Vocabulary = map[string]bool{
	"sandbox.creating": true, "sandbox.running": true, "sandbox.stopped": true,
	"sandbox.error": true, "sandbox.deleting": true,
	"backup.created": true, "backup.pruned": true,
	"share.granted": true, "share.revoked": true,
	"agent.invoke.started": true, "agent.invoke.completed": true, "agent.invoke.failed": true,
}

// TestEventReasonToWebhookType_CoversFullFR5Vocabulary is the completeness
// gate task #43 requires: all 12 FR5.2 event types now have a live K8s
// Event source (resolving DL9), so the map must have exactly 12 entries,
// every value drawn from the fixed vocabulary (no stray entries), and every
// vocabulary entry must be reachable from some Reason (no gaps).
func TestEventReasonToWebhookType_CoversFullFR5Vocabulary(t *testing.T) {
	if len(eventReasonToWebhookType) != 12 {
		t.Fatalf("expected exactly 12 entries (5 sandbox.* + 7 backup/share/agent.invoke), got %d: %+v",
			len(eventReasonToWebhookType), eventReasonToWebhookType)
	}

	seen := make(map[string]bool, len(eventReasonToWebhookType))
	for reason, webhookType := range eventReasonToWebhookType {
		if !fixedFR5Vocabulary[webhookType] {
			t.Errorf("reason %q maps to %q, which is outside the fixed FR5.2 vocabulary", reason, webhookType)
		}
		seen[webhookType] = true
	}
	for wantType := range fixedFR5Vocabulary {
		if !seen[wantType] {
			t.Errorf("FR5.2 vocabulary entry %q has no Reason mapped to it (gap)", wantType)
		}
	}
}

// TestEventReasonToWebhookType_NewReasonsMapCorrectly pins the exact 7 new
// Reason->type pairs task #43 was responsible for adding, matching the
// Reason strings actually landed by tasks #40 (backup), #41 (share), and
// #42 (agent invoke) — verified against pkg/router/backup_handlers.go,
// pkg/router/handlers.go, and pkg/router/agent/invoke.go before this test
// was written.
func TestEventReasonToWebhookType_NewReasonsMapCorrectly(t *testing.T) {
	cases := map[string]string{
		"BackupCreated":        "backup.created",
		"BackupPruned":         "backup.pruned",
		"ShareGranted":         "share.granted",
		"ShareRevoked":         "share.revoked",
		"AgentInvokeStarted":   "agent.invoke.started",
		"AgentInvokeCompleted": "agent.invoke.completed",
		"AgentInvokeFailed":    "agent.invoke.failed",
	}
	for reason, want := range cases {
		got, ok := webhookTypeForReason(reason)
		if !ok {
			t.Errorf("reason %q: expected a webhook mapping, found none", reason)
			continue
		}
		if got != want {
			t.Errorf("reason %q maps to %q, want %q", reason, got, want)
		}
	}
}

// TestDeliverer_DeliversBackupCreatedEndToEnd is the integration-style
// check task #43 asked for: subscribe to "backup.created", create the
// exact K8s Event pkg/router/backup_handlers.go's handleCreateBackup emits
// (Reason=BackupCreated) via the same emitSandboxEvent shape, and confirm a
// real delivery fires through the full RunOnce pipeline (cursor,
// subscription matching, HMAC signing, HTTP POST) with the right event
// type in the payload — not just a unit-level map lookup.
func TestDeliverer_DeliversBackupCreatedEndToEnd(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"backup.created"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx) // bootstrap cursor — no backlog replay

	evt := sandboxEvent("sb1", "default", "BackupCreated", "sb1-workspace-backup-100", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 1 {
		t.Fatalf("expected exactly 1 delivery for a backup.created subscription, got %d", receiver.count())
	}

	var payload struct {
		Event     string `json:"event"`
		SandboxID string `json:"sandboxId"`
	}
	receiver.mu.Lock()
	raw := receiver.receivedRaw[0]
	receiver.mu.Unlock()
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal delivered payload: %v", err)
	}
	if payload.Event != "backup.created" {
		t.Errorf("delivered payload event = %q, want backup.created", payload.Event)
	}
	if payload.SandboxID != "sb1" {
		t.Errorf("delivered payload sandboxId = %q, want sb1", payload.SandboxID)
	}
}

// TestDeliverer_ShareRevokedEndToEnd and
// TestDeliverer_AgentInvokeFailedEndToEnd round out end-to-end coverage for
// the other two new categories (share, agent.invoke) beyond the backup
// case above, so all three of #40/#41/#42's contributions are exercised
// through the real delivery pipeline, not just the map.

func TestDeliverer_ShareRevokedEndToEnd(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"share.revoked"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx)

	evt := sandboxEvent("sb1", "default", "ShareRevoked", "bob@example.com", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 1 {
		t.Fatalf("expected exactly 1 delivery for a share.revoked subscription, got %d", receiver.count())
	}
}

func TestDeliverer_AgentInvokeFailedEndToEnd(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := testScheme(t)
	sub := subscriptionSecret("wh1", srv.URL, []string{"agent.invoke.failed"}, "s3cr3t", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx)

	evt := sandboxEvent("sb1", "default", "AgentInvokeFailed", "invokeId=abc123", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 1 {
		t.Fatalf("expected exactly 1 delivery for an agent.invoke.failed subscription, got %d", receiver.count())
	}
}
