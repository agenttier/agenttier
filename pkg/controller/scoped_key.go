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

// FR6 sandbox-scoped API keys: the controller auto-mints a scoped key at
// sandbox create time and injects it into the Pod, mirroring the runtime-
// token precedent (runtime_token.go). Two Secrets are involved:
//
//   - The HASHED APIKeyRecord, written into the Router's api-key store
//     (pkg/apikeystore), which lives in InstallNamespace so the Router's
//     O(1) hash-based lookup keeps working. This Secret is NOT owned by
//     the Sandbox via owner reference — Sandboxes live in SandboxNamespace,
//     and a Kubernetes owner reference cannot cross namespaces (the GC only
//     looks for the owner in the *same* namespace as the owned object).
//     Revocation on delete is therefore handled explicitly from the
//     sandbox-delete finalizer path (reconcileDelete in
//     sandbox_controller.go), not owner-ref GC. See DL7 (decisions.md),
//     sa-review.md Critical finding #1.
//   - The PLAINTEXT injection Secret, co-located with the Pod in
//     SandboxNamespace, exactly like runtime_token.go's Secret. This one
//     safely uses owner-ref GC since owner and owned object share a
//     namespace.
//
// Both writes are idempotent under reconcile-loop re-entry: the hashed
// record's Secret name is deterministic (apikeystore.SecretNameForHash,
// keyed off the key's own hash — the key itself is generated once and
// persisted in the plaintext Secret, so re-reading that Secret on a second
// reconcile reproduces the same hash and therefore the same store lookup
// key rather than minting a second valid key).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/apikeystore"
	"github.com/agenttier/agenttier/pkg/router/auth"
)

// scopedKeyBytes is the entropy size of the auto-minted sandbox-scoped
// key, matching runtimeTokenBytes / generateAPIKey's 32-byte convention.
const scopedKeyBytes = 32

// scopedKeyPlaintextPrefix mirrors apiKeyPlaintextPrefix's "atk_" tag (kept
// as a local copy — the controller does not import pkg/router, per the
// package boundary pkg/apikeystore exists to preserve) so a leaked scoped
// key is recognizable and greppable the same way a user-level key is.
const scopedKeyPlaintextPrefix = "atk_"

// DefaultScopedKeyActionGroups is the action-group set granted to every
// auto-minted sandbox-scoped key (FR6.1.1/DD3). "resume" and "stop" are
// included so an agent holding its own sandbox's key can recover from a
// stop without its owner's intervention (the self-lockout edge case).
// "delete" is deliberately absent — not just from this default set but
// from the vocabulary entirely (see router.ValidActionGroups) — a scoped
// key must never be able to destroy the sandbox that backs it.
var DefaultScopedKeyActionGroups = []string{
	"run-command",
	"files:read",
	"files:write",
	"ports",
	"agent:invoke",
	"agent:configure",
	"resume",
	"stop",
}

// scopedKeyPlaintextSecretName returns the deterministic Secret name for a
// sandbox's scoped-key plaintext injection Secret, colocated with the
// Sandbox/Pod in SandboxNamespace. Deterministic per-sandbox naming (same
// idiom as runtimeTokenSecretName) means a controller restart or reconcile
// re-entry finds the existing Secret instead of minting a new key.
func scopedKeyPlaintextSecretName(sandboxName string) string {
	return sandboxName + "-scoped-key"
}

// ensureSandboxAPIKeySecret mints (or, on reconcile re-entry, reuses) the
// sandbox's scoped API key. Returns the plaintext-injection Secret's name
// on success — the caller plumbs it into MergedPodConfig so the Pod spec
// mounts AGENTTIER_SANDBOX_API_KEY from it (mirrors RuntimeTokenSecret).
//
// installNamespace is where the hashed APIKeyRecord is written (the
// Router's api-key store namespace — see apikeystore.New). It is passed
// explicitly rather than read from a Sandbox field because it is a
// controller-wide setting (SandboxReconciler.InstallNamespace), not
// per-sandbox data.
//
// Idempotent: if the plaintext Secret already exists, its key is reused
// (the hash is re-derived from it) rather than minting a second key — a
// naive Create-and-fail-on-conflict would either error-loop or, worse,
// silently mint a second valid key for the same sandbox on every
// reconcile pass.
func (r *SandboxReconciler) ensureSandboxAPIKeySecret(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, installNamespace string) (string, error) {
	plaintextName := scopedKeyPlaintextSecretName(sandbox.Name)

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: plaintextName, Namespace: sandbox.Namespace}, existing)
	if err == nil {
		// Already minted for this sandbox — reuse it. We still make sure
		// the hashed record exists (defense against the hashed record
		// having been deleted out-of-band while the plaintext Secret
		// survived, e.g. a partial failure on a previous reconcile).
		key := string(existing.Data["key"])
		if key == "" {
			return "", fmt.Errorf("scoped-key secret %s exists but has no key data", plaintextName)
		}
		if err := r.ensureHashedScopedKeyRecord(ctx, sandbox, key, installNamespace); err != nil {
			return "", err
		}
		return plaintextName, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("get scoped-key secret: %w", err)
	}

	keyBytes := make([]byte, scopedKeyBytes)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", fmt.Errorf("read random for scoped key: %w", err)
	}
	key := scopedKeyPlaintextPrefix + base64.RawURLEncoding.EncodeToString(keyBytes)

	// Write the hashed record FIRST. If this fails, we haven't yet created
	// the plaintext Secret, so a retried reconcile lands on this same
	// not-found branch again rather than leaving a plaintext Secret whose
	// key was never made valid on the Router side.
	if err := r.ensureHashedScopedKeyRecord(ctx, sandbox, key, installNamespace); err != nil {
		return "", err
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      plaintextName,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{ //nolint:gosec // G101: label values below, not credentials
				"agenttier.io/sandbox":        sandbox.Name,
				"agenttier.io/managed":        "true",
				"agenttier.io/secret-purpose": "scoped-api-key",
			},
		},
		Type: corev1.SecretTypeOpaque,
		// Data (not StringData) so the fake client used in tests behaves
		// identically to the real apiserver — same rationale as
		// runtime_token.go.
		Data: map[string][]byte{
			"key": []byte(key),
		},
	}
	if err := controllerutil.SetControllerReference(sandbox, desired, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner reference on scoped-key secret: %w", err)
	}
	if err := r.Create(ctx, desired); err != nil {
		if errors.IsAlreadyExists(err) {
			// Lost a create race to another reconcile; the hashed record
			// write above is idempotent (same deterministic Secret name
			// derived from this exact key's hash) so no double-mint risk.
			return plaintextName, nil
		}
		return "", fmt.Errorf("create scoped-key secret: %w", err)
	}
	return plaintextName, nil
}

// ensureHashedScopedKeyRecord writes the hashed APIKeyRecord for key into
// the api-key store in installNamespace, if it isn't already there. The
// Secret name apikeystore derives from the key's hash is deterministic, so
// calling this with the same key on every reconcile is a no-op after the
// first successful write — it never mints a second valid record for the
// same plaintext key.
func (r *SandboxReconciler) ensureHashedScopedKeyRecord(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, key, installNamespace string) error {
	store := apikeystore.New(r.Client, installNamespace)
	keyHash := auth.HashAPIKey(key)

	if _, err := store.GetAPIKeyByHash(ctx, keyHash); err == nil {
		// Record already persisted for this exact key — nothing to do.
		return nil
	}

	rec := &auth.APIKeyRecord{
		KeyHash:      keyHash,
		UserID:       ownerSubOf(sandbox),
		Name:         "sandbox-scoped: " + sandbox.Name,
		SandboxID:    sandbox.Name,
		ActionGroups: append([]string(nil), DefaultScopedKeyActionGroups...),
		CreatedAt:    metav1.Now().Time,
	}
	if err := store.Create(ctx, keyHash, rec); err != nil {
		if errors.IsAlreadyExists(err) {
			// Raced with another reconcile that just created the same
			// deterministic-named record; that's fine, not an error.
			return nil
		}
		return fmt.Errorf("create hashed scoped-key record: %w", err)
	}
	return nil
}

// ownerSubOf returns the OIDC sub of the sandbox's creator, or empty when
// unknown (e.g. a sandbox created before CreatedBy was populated). Used
// only as informational metadata on the scoped key's record — the scoped
// key's authorization is driven entirely by SandboxID + ActionGroups, not
// UserID, so an empty value here doesn't weaken enforcement.
func ownerSubOf(sandbox *agenttierv1alpha1.Sandbox) string {
	if sandbox.Spec.CreatedBy == nil {
		return ""
	}
	return sandbox.Spec.CreatedBy.Sub
}

// revokeSandboxAPIKey deletes the sandbox's hashed scoped-key record from
// the api-key store in installNamespace. Called from the sandbox-delete
// finalizer cleanup path (reconcileDelete), NOT relied upon via owner-
// reference GC — see the package doc comment above and DL7 for why: the
// hashed record lives in installNamespace while the Sandbox lives in
// sandbox.Namespace, and owner references cannot cross namespaces.
//
// This only removes the Secret-backed record; it does NOT evict the
// Router's in-memory LRU validation cache (the controller and Router are
// separate processes/Deployments with no shared memory or RPC channel for
// this). A revoked scoped key may continue to authenticate on a Router
// replica that already cached it, for up to that replica's configured
// cache TTL — a bounded, accepted staleness window (sa-review.md Medium
// finding, checklist item #9/#12), not a silent gap. The NEXT cache-miss
// lookup for this key (on any replica) will 404 against the now-deleted
// Secret and correctly fail closed.
//
// A no-op (nil error) when no scoped key was ever minted for this sandbox
// — deleting a sandbox that never opted into HTTP-exec/agent mode should
// not be treated as a cleanup failure.
func (r *SandboxReconciler) revokeSandboxAPIKey(ctx context.Context, sandbox *agenttierv1alpha1.Sandbox, installNamespace string) error {
	plaintextName := scopedKeyPlaintextSecretName(sandbox.Name)
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: plaintextName, Namespace: sandbox.Namespace}, secret)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get scoped-key secret for revoke: %w", err)
	}
	key := string(secret.Data["key"])
	if key == "" {
		return nil
	}

	keyHash := auth.HashAPIKey(key)
	hashedName := apikeystore.SecretNameForHash(keyHash)

	hashedSecret := &corev1.Secret{}
	getErr := r.Get(ctx, client.ObjectKey{Name: hashedName, Namespace: installNamespace}, hashedSecret)
	if errors.IsNotFound(getErr) {
		// Already gone (e.g. a retried reconcile after a partial prior
		// delete) — nothing left to revoke.
		return nil
	}
	if getErr != nil {
		return fmt.Errorf("get hashed scoped-key record for revoke: %w", getErr)
	}
	if err := r.Delete(ctx, hashedSecret); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete hashed scoped-key record: %w", err)
	}
	return nil
}
