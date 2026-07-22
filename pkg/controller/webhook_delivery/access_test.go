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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

// Task #50 (CRITICAL cross-tenant webhook disclosure): tests for the
// controller-side defense-in-depth access check. The Router's
// handleCreateWebhook (pkg/router/webhook_handlers_test.go) already
// enforces ownership at subscription-creation time — these tests instead
// prove the delivery loop independently re-derives access on every single
// delivery from the live Sandbox object, so a subscription that somehow
// bypassed (or predates) the Router-side fix still cannot leak another
// tenant's events.

func schemeWithSandboxes(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := agenttierv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func testSandbox(name, namespace, ownerSub string) *agenttierv1alpha1.Sandbox {
	return &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agenttierv1alpha1.SandboxSpec{
			CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: ownerSub},
		},
	}
}

// --- canAccessSandbox unit tests ---

func TestCanAccessSandbox_AdminSubscriptionAlwaysAllowed(t *testing.T) {
	scheme := schemeWithSandboxes(t)
	// No Sandbox object exists at all — an admin subscription must still
	// be allowed, since IsAdmin short-circuits before any Get call.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	sub := Subscription{ID: "wh1", UserID: "admin", IsAdmin: true}
	if !canAccessSandbox(context.Background(), c, sub, "default", "nonexistent-sandbox") {
		t.Fatal("expected an admin subscription to access any sandbox, including a nonexistent one")
	}
}

func TestCanAccessSandbox_OwnerAllowed(t *testing.T) {
	scheme := schemeWithSandboxes(t)
	sandbox := testSandbox("sb1", "default", "u1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	sub := Subscription{ID: "wh1", UserID: "u1"}
	if !canAccessSandbox(context.Background(), c, sub, "default", "sb1") {
		t.Fatal("expected the sandbox's owner to have access")
	}
}

func TestCanAccessSandbox_NonOwnerDenied(t *testing.T) {
	scheme := schemeWithSandboxes(t)
	sandbox := testSandbox("sb1", "default", "victim")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	sub := Subscription{ID: "wh1", UserID: "attacker"}
	if canAccessSandbox(context.Background(), c, sub, "default", "sb1") {
		t.Fatal("expected a non-owner, non-admin subscriber to be denied access")
	}
}

func TestCanAccessSandbox_SharedUserAllowed(t *testing.T) {
	scheme := schemeWithSandboxes(t)
	sandbox := testSandbox("sb1", "default", "owner")
	sandbox.Spec.Sharing = &agenttierv1alpha1.SharingSpec{
		Users: []agenttierv1alpha1.SharePermission{{Identity: "collaborator", Level: "viewer"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	sub := Subscription{ID: "wh1", UserID: "collaborator"}
	if !canAccessSandbox(context.Background(), c, sub, "default", "sb1") {
		t.Fatal("expected a user the sandbox is explicitly shared with to have access")
	}
}

func TestCanAccessSandbox_NonexistentSandboxDenied(t *testing.T) {
	scheme := schemeWithSandboxes(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	sub := Subscription{ID: "wh1", UserID: "u1"}
	if canAccessSandbox(context.Background(), c, sub, "default", "sb1") {
		t.Fatal("expected a nonexistent sandbox to deny access (nothing to check ownership against)")
	}
}

func TestCanAccessSandbox_EmptyUserIDDenied(t *testing.T) {
	scheme := schemeWithSandboxes(t)
	// A sandbox with no CreatedBy at all — an empty subscriber UserID must
	// never accidentally match an empty/unset owner sub.
	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb1", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	sub := Subscription{ID: "wh1", UserID: ""}
	if canAccessSandbox(context.Background(), c, sub, "default", "sb1") {
		t.Fatal("expected an empty subscriber UserID to never match, even against a sandbox with no CreatedBy")
	}
}

// --- end-to-end: dispatchEvent skips delivery for out-of-scope subscribers ---

func TestDeliverer_SkipsDeliveryWhenSubscriberCannotAccessSandbox(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := schemeWithSandboxes(t)
	// The subscriber ("attacker") is NOT the sandbox's owner ("victim") and
	// has no share grant — the persisted subscription record itself
	// (hypothetically reached via a bypassed or pre-fix Router check)
	// still must not result in a delivered event.
	victimSandbox := testSandbox("victims-sandbox", "default", "victim")
	sub := subscriptionSecretOwnedBy("wh1", srv.URL, []string{"sandbox.running"}, "", "attacker", "s3cr3t")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub, victimSandbox).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx) // bootstrap

	evt := sandboxEvent("victims-sandbox", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 0 {
		t.Fatalf("expected NO delivery to a subscriber who cannot access the sandbox, got %d HTTP calls", receiver.count())
	}
}

// TestDeliverer_DeliversWhenSubscriberOwnsSandbox is the positive-case
// regression guard alongside the skip test above — the access check must
// not become an accidental default-deny-everything: a subscriber who
// genuinely owns the sandbox must still receive its events end-to-end.
func TestDeliverer_DeliversWhenSubscriberOwnsSandbox(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := schemeWithSandboxes(t)
	ownedSandboxObj := testSandbox("my-sandbox", "default", "owner")
	sub := subscriptionSecretOwnedBy("wh1", srv.URL, []string{"sandbox.running"}, "", "owner", "s3cr3t")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub, ownedSandboxObj).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx) // bootstrap

	evt := sandboxEvent("my-sandbox", "default", "Running", "ready", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 1 {
		t.Fatalf("expected exactly 1 delivery to the sandbox's own owner, got %d", receiver.count())
	}
}

// TestDeliverer_SkipsDeliveryForNonexistentSandbox covers the edge case
// where the Sandbox object has since been deleted (e.g. the delivery loop
// is catching up on a backlog of events for a sandbox that no longer
// exists) — access must be denied, not silently granted.
func TestDeliverer_SkipsDeliveryForNonexistentSandbox(t *testing.T) {
	receiver := newFakeReceiver()
	srv := receiver.server()
	defer srv.Close()

	scheme := schemeWithSandboxes(t)
	// No Sandbox object seeded at all.
	sub := subscriptionSecretOwnedBy("wh1", srv.URL, []string{"sandbox.deleting"}, "", "u1", "s3cr3t")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub).Build()

	ctx := context.Background()
	d := NewDeliverer(c, discard(), "agenttier", time.Hour)
	d.validateURL = permissiveValidator
	d.sleep = func(time.Duration) {}
	d.RunOnce(ctx) // bootstrap

	evt := sandboxEvent("gone-sandbox", "default", "Deleted", "Sandbox deleted", time.Now().Add(time.Second))
	if err := c.Create(ctx, evt); err != nil {
		t.Fatalf("create event: %v", err)
	}
	d.RunOnce(ctx)

	if receiver.count() != 0 {
		t.Fatalf("expected no delivery for a sandbox that no longer exists, got %d", receiver.count())
	}
}
