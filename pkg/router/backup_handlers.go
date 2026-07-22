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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/controller/backup"
	"github.com/agenttier/agenttier/pkg/governance"
)

// backupSnapshotSourceSandboxLabel mirrors the label handleCloneSandbox
// stamps on a clone's snapshot (handlers.go) so the list endpoint can
// surface both scheduled/on-demand backups (backup.LabelSnapshotKind =
// backup.SnapshotKindBackup) and clone snapshots under one FR3 view.
const backupSnapshotSourceSandboxLabel = "agenttier.io/source-sandbox"

// backupInfo is the wire shape for one entry in GET /sandboxes/{id}/backups
// and the response of POST /sandboxes/{id}/backups, per design.md's FR3
// contract: {name, createdAt, kind, readyToUse, restoreSize}.
type backupInfo struct {
	Name        string  `json:"name"`
	CreatedAt   *string `json:"createdAt,omitempty"`
	Kind        string  `json:"kind"`
	ReadyToUse  bool    `json:"readyToUse"`
	RestoreSize *string `json:"restoreSize,omitempty"`
}

// snapshotBelongsToSandbox reports whether snap is one of sandbox's own
// backup/clone snapshots, using the exact same two label schemes
// handleListBackups scopes its results by: a scheduled/on-demand backup
// (backup.LabelSourcePVC == the sandbox's current PVC) or a clone snapshot
// (backupSnapshotSourceSandboxLabel == the sandbox's name). Delete and
// restore must call this before acting on a caller-supplied snapshotName —
// otherwise any two sandboxes sharing a namespace (the common case: default
// "default") let one owner delete or restore from another owner's backup,
// since ownership is per-Sandbox (CreatedBy.Sub), not per-namespace.
func snapshotBelongsToSandbox(snap *snapshotv1.VolumeSnapshot, sandbox *agenttierv1alpha1.Sandbox) bool {
	if sandbox.Status.PVCName != "" && snap.Labels[backup.LabelSourcePVC] == sandbox.Status.PVCName {
		return true
	}
	return snap.Labels[backupSnapshotSourceSandboxLabel] == sandbox.Name
}

func backupInfoFromSnapshot(snap *snapshotv1.VolumeSnapshot, kind string) backupInfo {
	info := backupInfo{Name: snap.Name, Kind: kind}
	if snap.Status != nil {
		if snap.Status.ReadyToUse != nil {
			info.ReadyToUse = *snap.Status.ReadyToUse
		}
		if snap.Status.CreationTime != nil {
			s := snap.Status.CreationTime.Format(time.RFC3339)
			info.CreatedAt = &s
		}
		if snap.Status.RestoreSize != nil {
			s := snap.Status.RestoreSize.String()
			info.RestoreSize = &s
		}
	}
	if info.CreatedAt == nil && !snap.CreationTimestamp.IsZero() {
		s := snap.CreationTimestamp.Format(time.RFC3339)
		info.CreatedAt = &s
	}
	return info
}

// handleListBackups implements GET /sandboxes/{id}/backups: it returns both
// scheduled/on-demand backup snapshots (labeled by pkg/controller/backup,
// keyed off the sandbox's current PVC) and clone snapshots taken via
// handleCloneSandbox (labeled agenttier.io/source-sandbox=<name>), per
// design.md's FR3 list contract.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sandboxID := mux.Vars(r)["id"]
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	backups := make([]backupInfo, 0)
	if sandbox.Status.PVCName != "" {
		scheduled := &snapshotv1.VolumeSnapshotList{}
		if err := s.k8sClient.List(r.Context(), scheduled,
			client.InNamespace(sandbox.Namespace),
			client.MatchingLabels{
				backup.LabelSnapshotKind: backup.SnapshotKindBackup,
				backup.LabelSourcePVC:    sandbox.Status.PVCName,
			},
		); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to list backup snapshots: "+err.Error())
			return
		}
		for i := range scheduled.Items {
			backups = append(backups, backupInfoFromSnapshot(&scheduled.Items[i], backup.SnapshotKindBackup))
		}
	}

	clones := &snapshotv1.VolumeSnapshotList{}
	if err := s.k8sClient.List(r.Context(), clones,
		client.InNamespace(sandbox.Namespace),
		client.MatchingLabels{backupSnapshotSourceSandboxLabel: sandbox.Name},
	); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list clone snapshots: "+err.Error())
		return
	}
	for i := range clones.Items {
		backups = append(backups, backupInfoFromSnapshot(&clones.Items[i], "clone"))
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"backups": backups})
}

// handleCreateBackup implements POST /sandboxes/{id}/backups: it triggers an
// on-demand VolumeSnapshot outside the scheduled interval, labeled the same
// way the scheduler labels its own backups (backup.LabelSnapshotKind +
// backup.LabelSourcePVC + backup.LabelManaged) so the retention sweep
// (scheduler.go's pruneExpired) still prunes it — on-demand backups don't
// bypass retention (FR3.2).
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sandboxID := mux.Vars(r)["id"]
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	if sandbox.Status.PVCName == "" {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("sandbox %s has no PVC to back up (phase=%s)", sandbox.Name, sandbox.Status.Phase))
		return
	}

	var req struct {
		SnapshotClass string `json:"snapshotClass,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	pvcName := sandbox.Status.PVCName
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-backup-%d", pvcName, time.Now().Unix()),
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				backup.LabelSnapshotKind: backup.SnapshotKindBackup,
				backup.LabelSourcePVC:    pvcName,
				backup.LabelManaged:      "true",
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvcName},
		},
	}
	if req.SnapshotClass != "" {
		snap.Spec.VolumeSnapshotClassName = &req.SnapshotClass
	}
	if err := s.k8sClient.Create(r.Context(), snap); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create backup snapshot: "+err.Error())
		return
	}

	respondJSON(w, http.StatusCreated, backupInfoFromSnapshot(snap, backup.SnapshotKindBackup))
	s.recordAudit(r.Context(), claims, "backup.create", "sandbox", sandbox.Name)
	s.emitSandboxEvent(r.Context(), sandbox, corev1.EventTypeNormal, "BackupCreated", snap.Name)
}

// handleDeleteBackup implements DELETE /sandboxes/{id}/backups/{snapshotName}:
// it prunes a specific snapshot on demand (owner-or-admin, same RBAC as other
// sandbox mutations).
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	vars := mux.Vars(r)
	sandboxID := vars["id"]
	snapshotName := vars["snapshotName"]
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	// Look up the snapshot first (rather than a blind Delete-by-name) so we
	// can verify it actually belongs to this sandbox before touching it —
	// closes a cross-tenant IDOR where any sandbox owner could delete
	// another sandbox's backup (or an unrelated VolumeSnapshot) just by
	// guessing/enumerating its name in the shared namespace.
	snap := &snapshotv1.VolumeSnapshot{}
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Name: snapshotName, Namespace: sandbox.Namespace}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to look up snapshot: "+err.Error())
		return
	}
	if !snapshotBelongsToSandbox(snap, sandbox) {
		// 404, not 403: the caller already proved ownership of `sandbox`,
		// but this snapshot isn't one of its backups — don't confirm the
		// snapshot's existence to a caller who has no claim to it.
		respondError(w, http.StatusNotFound, "snapshot not found")
		return
	}

	if err := s.k8sClient.Delete(r.Context(), snap); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete snapshot: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "deleted", "name": snapshotName})
	s.recordAudit(r.Context(), claims, "backup.delete", "sandbox", sandbox.Name)
	s.emitSandboxEvent(r.Context(), sandbox, corev1.EventTypeNormal, "BackupPruned", snapshotName)
}

// handleRestoreBackup implements POST /sandboxes/{id}/backups/{snapshotName}/restore:
// it creates a new Sandbox with Spec.CloneFromSnapshot set to the chosen
// snapshot, reusing the same construction handleCloneSandbox uses so restore
// and clone stay behaviorally identical. Governance is re-checked exactly as
// it is on create (NFR3) — a policy tightened after the backup was taken
// still gates the restore.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	vars := mux.Vars(r)
	sandboxID := vars["id"]
	snapshotName := vars["snapshotName"]
	source, err := s.getSandboxWithAuthCheck(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	// Verify the snapshot still exists before building the restore Sandbox —
	// closes the list/restore race where a snapshot is pruned between a
	// caller's GET /backups and this call (edge case in requirements.md):
	// a 404 here is explicit, never a silent no-op.
	snap := &snapshotv1.VolumeSnapshot{}
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Name: snapshotName, Namespace: source.Namespace}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "snapshot not found (it may have been pruned)")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to look up snapshot: "+err.Error())
		return
	}
	// A snapshot mid-deletion (finalizer teardown) is a 409, not a 404 — the
	// caller should retry, not assume it never existed.
	if !snap.DeletionTimestamp.IsZero() {
		respondError(w, http.StatusConflict, "snapshot is being deleted")
		return
	}
	// Cross-tenant IDOR guard: verify the snapshot actually belongs to the
	// path sandbox before hydrating a new Sandbox's PVC from it — without
	// this, any sandbox owner could restore (and thereby read the volume
	// contents of) another owner's backup by naming its snapshot directly.
	// 404, not 403, to avoid confirming the snapshot's existence.
	if !snapshotBelongsToSandbox(snap, source) {
		respondError(w, http.StatusNotFound, "snapshot not found (it may have been pruned)")
		return
	}

	var req struct {
		Name string `json:"name,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}
	restoreName := req.Name
	if restoreName == "" {
		restoreName = fmt.Sprintf("%s-restore-%d", source.Name, time.Now().Unix())
	}
	if !validSandboxName.MatchString(restoreName) {
		respondError(w, http.StatusBadRequest, "name must match RFC 1123 label rules: lowercase alphanumeric or '-', start/end alphanumeric, max 63 chars")
		return
	}

	restoreSpec := *source.Spec.DeepCopy()
	restoreSpec.CloneFromSnapshot = snapshotName
	restoreSpec.CreatedBy = &agenttierv1alpha1.UserIdentity{
		Sub:         claims.Sub,
		Email:       claims.Email,
		DisplayName: claims.Name,
	}

	restore := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restoreName,
			Namespace: source.Namespace,
			Labels: map[string]string{
				"agenttier.io/restored-from": source.Name,
			},
		},
		Spec: restoreSpec,
	}

	// Governance re-check (NFR3), mirroring the inline check in
	// handleCreateSandbox: a policy that tightened after the backup was
	// taken still gates the restore.
	if s.governanceStore != nil {
		policy, err := governance.Resolve(r.Context(), s.governanceStore, source.Namespace)
		if err != nil {
			s.logger.Warn("failed to resolve governance policy; proceeding without enforcement", "namespace", source.Namespace, "error", err)
		} else if !policy.IsEmpty() {
			existing := &agenttierv1alpha1.SandboxList{}
			if err := s.k8sClient.List(r.Context(), existing, client.InNamespace(source.Namespace)); err != nil {
				respondError(w, http.StatusInternalServerError, "failed to check namespace usage: "+err.Error())
				return
			}
			usage := governance.CountUsage(existing, claims.Sub)
			if v := governance.Check(policy, usage, restore); v.Violated() {
				respondJSON(w, http.StatusForbidden, map[string]interface{}{
					"error":      "policy_violation",
					"violations": v,
				})
				return
			}
		}
	}

	if err := s.k8sClient.Create(r.Context(), restore); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create restored sandbox: "+err.Error())
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"name":         restoreName,
		"namespace":    restore.Namespace,
		"restoredFrom": source.Name,
		"snapshot":     snapshotName,
		"phase":        "Pending",
		"message":      "Restore in progress. Poll GET /api/v1/sandboxes/" + restoreName + " for status.",
	})
	s.recordAudit(r.Context(), claims, "backup.restore", "sandbox", restoreName)
}
