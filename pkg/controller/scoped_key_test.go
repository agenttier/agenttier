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

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/apikeystore"
	"github.com/agenttier/agenttier/pkg/router/auth"
)

// scopedKeyReconciler builds a SandboxReconciler + fake client for these
// tests, sharing newTokenScheme (runtime_token_test.go) so the scheme setup
// stays in one place.
func scopedKeyReconciler(t *testing.T, objs ...client.Object) (*SandboxReconciler, client.Client) {
	t.Helper()
	scheme := newTokenScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &SandboxReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
	return r, c
}

func TestScopedKey_EnsureCreatesAndIsIdempotent(t *testing.T) {
	const installNS = "agenttier"
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			UID:       "00000000-0000-0000-0000-000000000001",
		},
	}
	r, c := scopedKeyReconciler(t, sb)

	name1, err := r.ensureSandboxAPIKeySecret(context.Background(), sb, installNS)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if name1 != "sbx-1-scoped-key" {
		t.Errorf("name = %q, want sbx-1-scoped-key", name1)
	}

	got := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name1, Namespace: "default"}, got); err != nil {
		t.Fatalf("get plaintext secret: %v", err)
	}
	key1 := string(got.Data["key"])
	if len(key1) < 32 {
		t.Errorf("key too short (%d chars) — entropy generation failed?", len(key1))
	}
	if got.Labels["agenttier.io/secret-purpose"] != "scoped-api-key" {
		t.Errorf("missing secret-purpose label: %+v", got.Labels)
	}
	// Plaintext Secret is same-namespace as the Sandbox, so owner-ref GC
	// is safe here (unlike the cross-namespace hashed record).
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "Sandbox" {
		t.Errorf("owner reference missing or wrong kind: %+v", got.OwnerReferences)
	}

	// Confirm the hashed record actually landed in the api-key store,
	// in installNamespace (NOT the sandbox's namespace).
	store := apikeystore.New(c, installNS)
	rec, err := store.GetAPIKeyByHash(context.Background(), auth.HashAPIKey(key1))
	if err != nil {
		t.Fatalf("hashed record not found in store: %v", err)
	}
	if rec.SandboxID != "sbx-1" {
		t.Errorf("record SandboxID = %q, want sbx-1", rec.SandboxID)
	}

	// Second call (reconcile re-entry) must reuse the same key — a naive
	// Create-and-fail-on-conflict risks minting a second valid key on
	// every reconcile.
	name2, err := r.ensureSandboxAPIKeySecret(context.Background(), sb, installNS)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if name2 != name1 {
		t.Errorf("second call returned different secret name: %q vs %q", name2, name1)
	}
	got2 := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name2, Namespace: "default"}, got2); err != nil {
		t.Fatalf("get plaintext secret 2nd time: %v", err)
	}
	if string(got2.Data["key"]) != key1 {
		t.Error("scoped key was regenerated on idempotent call — every reconcile would mint a fresh valid key")
	}

	// Exactly one hashed record must exist for this sandbox, not two.
	list := &corev1.SecretList{}
	if err := c.List(context.Background(), list,
		client.InNamespace(installNS),
		client.MatchingLabels{apikeystore.SecretPurposeLabel: apikeystore.SecretPurpose},
	); err != nil {
		t.Fatalf("list hashed records: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("hashed record count = %d, want exactly 1 after two ensure calls", len(list.Items))
	}
}

func TestScopedKey_DefaultActionGroups(t *testing.T) {
	const installNS = "agenttier"
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-2", Namespace: "default"},
	}
	r, c := scopedKeyReconciler(t, sb)

	name, err := r.ensureSandboxAPIKeySecret(context.Background(), sb, installNS)
	if err != nil {
		t.Fatalf("ensureSandboxAPIKeySecret: %v", err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, secret); err != nil {
		t.Fatalf("get plaintext secret: %v", err)
	}
	key := string(secret.Data["key"])

	store := apikeystore.New(c, installNS)
	rec, err := store.GetAPIKeyByHash(context.Background(), auth.HashAPIKey(key))
	if err != nil {
		t.Fatalf("get hashed record: %v", err)
	}

	groups := map[string]bool{}
	for _, g := range rec.ActionGroups {
		groups[g] = true
	}
	for _, want := range []string{"resume", "stop", "run-command", "files:read", "files:write", "ports", "agent:invoke", "agent:configure"} {
		if !groups[want] {
			t.Errorf("default ActionGroups missing %q: %v", want, rec.ActionGroups)
		}
	}
	if groups["delete"] {
		t.Error("default ActionGroups must never contain \"delete\" (FR6.1.1) — a scoped key must not be able to destroy its own sandbox")
	}
}

func TestScopedKey_ReconcileCreatingInjectsEnv(t *testing.T) {
	// End-to-end through PodBuilder: MergedPodConfig.SandboxAPIKeySecret,
	// once set from ensureSandboxAPIKeySecret's return value, must produce
	// an AGENTTIER_SANDBOX_API_KEY env var on the sandbox container —
	// mirrors how AGENTTIER_RUNTIME_TOKEN is injected.
	const installNS = "agenttier"
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-3", Namespace: "default"},
	}
	r, _ := scopedKeyReconciler(t, sb)

	secretName, err := r.ensureSandboxAPIKeySecret(context.Background(), sb, installNS)
	if err != nil {
		t.Fatalf("ensureSandboxAPIKeySecret: %v", err)
	}

	builder := &PodBuilder{DefaultImage: "default:latest"}
	config := &MergedPodConfig{
		Image:               "test:v1",
		MountPath:           "/workspace",
		PVCName:             "sbx-3-workspace",
		UseHTTPExec:         true,
		SandboxAPIKeySecret: secretName,
	}
	pod := builder.Build(sb, config)

	var found *corev1.EnvVar
	for i := range pod.Spec.Containers[0].Env {
		if pod.Spec.Containers[0].Env[i].Name == "AGENTTIER_SANDBOX_API_KEY" {
			found = &pod.Spec.Containers[0].Env[i]
			break
		}
	}
	if found == nil {
		t.Fatal("AGENTTIER_SANDBOX_API_KEY env var not injected into sandbox container")
	}
	if found.ValueFrom == nil || found.ValueFrom.SecretKeyRef == nil {
		t.Fatal("AGENTTIER_SANDBOX_API_KEY must be sourced via SecretKeyRef, not an inline literal (NFR10)")
	}
	if found.ValueFrom.SecretKeyRef.Name != secretName {
		t.Errorf("SecretKeyRef.Name = %q, want %q", found.ValueFrom.SecretKeyRef.Name, secretName)
	}
	if found.ValueFrom.SecretKeyRef.Key != "key" {
		t.Errorf("SecretKeyRef.Key = %q, want %q", found.ValueFrom.SecretKeyRef.Key, "key")
	}
}

func TestScopedKey_NotInjectedWhenHTTPExecDisabled(t *testing.T) {
	sb := &agenttierv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-4", Namespace: "default"}}
	builder := &PodBuilder{DefaultImage: "default:latest"}
	config := &MergedPodConfig{
		Image:               "test:v1",
		MountPath:           "/workspace",
		PVCName:             "sbx-4-workspace",
		UseHTTPExec:         false, // opt-in gate off
		SandboxAPIKeySecret: "sbx-4-scoped-key",
	}
	pod := builder.Build(sb, config)
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "AGENTTIER_SANDBOX_API_KEY" {
			t.Fatal("AGENTTIER_SANDBOX_API_KEY must not be injected when UseHTTPExec is false")
		}
	}
}

// TestScopedKey_RevokeCrossNamespace_RemovesHashedRecordAndStopsAuthenticating
// is the REQUIRED negative test from sa-review.md Critical finding #1 /
// checklist item #9: InstallNamespace != SandboxNamespace (the documented,
// expected multi-tenant topology), confirming (a) the hashed record is
// actually removed from the store (not orphaned) and (b) the key stops
// authenticating (a fresh lookup by the same hash 404s).
func TestScopedKey_RevokeCrossNamespace_RemovesHashedRecordAndStopsAuthenticating(t *testing.T) {
	const installNamespace = "agenttier" // where the hashed record lives
	const sandboxNamespace = "default"   // where the Sandbox/Pod live — DISTINCT from installNamespace
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-cross-ns",
			Namespace: sandboxNamespace,
			UID:       "00000000-0000-0000-0000-0000000000c1",
		},
	}
	r, c := scopedKeyReconciler(t, sb)

	// Mint, exactly as reconcileCreating would with InstallNamespace set
	// to a namespace distinct from the Sandbox's own namespace.
	plaintextName, err := r.ensureSandboxAPIKeySecret(context.Background(), sb, installNamespace)
	if err != nil {
		t.Fatalf("ensureSandboxAPIKeySecret: %v", err)
	}
	plaintextSecret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: plaintextName, Namespace: sandboxNamespace}, plaintextSecret); err != nil {
		t.Fatalf("get plaintext secret: %v", err)
	}
	key := string(plaintextSecret.Data["key"])
	keyHash := auth.HashAPIKey(key)

	// Sanity: the hashed record must be in installNamespace, not
	// sandboxNamespace — confirming the cross-namespace setup this test
	// exists to exercise is actually in effect.
	store := apikeystore.New(c, installNamespace)
	if _, err := store.GetAPIKeyByHash(context.Background(), keyHash); err != nil {
		t.Fatalf("precondition failed: hashed record not found in installNamespace before revoke: %v", err)
	}
	crossNSStore := apikeystore.New(c, sandboxNamespace)
	if _, err := crossNSStore.GetAPIKeyByHash(context.Background(), keyHash); err == nil {
		t.Fatal("precondition failed: hashed record unexpectedly found in sandboxNamespace — test setup isn't actually cross-namespace")
	}

	// This is what reconcileDelete's finalizer cleanup calls — NOT
	// owner-reference GC, which cannot fire here since installNamespace
	// != sandboxNamespace.
	if err := r.revokeSandboxAPIKey(context.Background(), sb, installNamespace); err != nil {
		t.Fatalf("revokeSandboxAPIKey: %v", err)
	}

	// (a) The hashed record must be actually removed from the store, not
	// merely orphaned.
	if _, err := store.GetAPIKeyByHash(context.Background(), keyHash); err == nil {
		t.Error("hashed record still present in the store after revoke — FR6.5 did not fire")
	}

	// (b) The key must stop authenticating: a fresh lookup by hash 404s.
	// (This IS the "stops authenticating" check for a Secret-backed
	// store with no separate cache in this test — the Router's live
	// APIKeyValidator additionally has an in-memory LRU cache, whose
	// bounded staleness window is a documented, accepted exposure
	// per sa-review.md, not exercised by this store-level test.)
	hashedSecretName := apikeystore.SecretNameForHash(keyHash)
	hashedSecret := &corev1.Secret{}
	err = c.Get(context.Background(), client.ObjectKey{Name: hashedSecretName, Namespace: installNamespace}, hashedSecret)
	if err == nil {
		t.Error("hashed-record Secret still exists in installNamespace after revoke")
	}
}

func TestScopedKey_RevokeNoScopedKeyEverMintedIsNoop(t *testing.T) {
	// A sandbox that never opted into HTTP-exec/agent mode never had a
	// scoped key minted. Deleting it must not surface a cleanup error.
	sb := &agenttierv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-no-key", Namespace: "default"}}
	r, _ := scopedKeyReconciler(t, sb)

	if err := r.revokeSandboxAPIKey(context.Background(), sb, "agenttier"); err != nil {
		t.Fatalf("revokeSandboxAPIKey on a sandbox with no scoped key: %v", err)
	}
}

func TestScopedKey_ReconcileDeleteRevokesAcrossNamespaces(t *testing.T) {
	// Integration-level version of the cross-namespace revoke test above,
	// through the actual reconcileDelete finalizer path (not calling
	// revokeSandboxAPIKey directly), confirming the wiring in
	// sandbox_controller.go actually invokes it with r.InstallNamespace.
	const installNamespace = "agenttier"
	const sandboxNamespace = "default"
	now := metav1.Now()
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sbx-delete-scoped",
			Namespace:         sandboxNamespace,
			DeletionTimestamp: &now,
			Finalizers:        []string{FinalizerName},
		},
	}
	scheme := newTokenScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sb).Build()
	r := &SandboxReconciler{
		Client:           c,
		Scheme:           scheme,
		Recorder:         record.NewFakeRecorder(10),
		InstallNamespace: installNamespace,
	}

	plaintextName, err := r.ensureSandboxAPIKeySecret(context.Background(), sb, r.InstallNamespace)
	if err != nil {
		t.Fatalf("ensureSandboxAPIKeySecret: %v", err)
	}
	plaintextSecret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: plaintextName, Namespace: sandboxNamespace}, plaintextSecret); err != nil {
		t.Fatalf("get plaintext secret: %v", err)
	}
	keyHash := auth.HashAPIKey(string(plaintextSecret.Data["key"]))

	if _, err := r.reconcileDelete(context.Background(), sb); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	store := apikeystore.New(c, installNamespace)
	if _, err := store.GetAPIKeyByHash(context.Background(), keyHash); err == nil {
		t.Error("hashed scoped-key record still present after reconcileDelete — revokeSandboxAPIKey was not wired into the finalizer path correctly")
	}
}
