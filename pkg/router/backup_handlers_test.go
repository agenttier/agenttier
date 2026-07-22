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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/controller/backup"
	"github.com/agenttier/agenttier/pkg/governance"
)

// backupFixture builds a Server with the snapshot scheme registered, plus a
// standalone mux wired to the FR3 backup handlers. Route wiring on the real
// s.router happens in the Group 2b serialization task (server.go), so tests
// here register their own minimal router the same way clone_test.go does for
// the pre-wiring era of that handler.
func backupFixture(t *testing.T, objs ...client.Object) (*Server, client.Client, http.Handler) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		agenttierv1alpha1.AddToScheme,
		snapshotv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&agenttierv1alpha1.Sandbox{}).
		Build()
	s := NewServer(&Config{ListenAddr: ":0", DevAuth: true}, c, nil)

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/sandboxes/{id}/backups", s.handleListBackups).Methods("GET")
	api.HandleFunc("/sandboxes/{id}/backups", s.handleCreateBackup).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/backups/{snapshotName}/restore", s.handleRestoreBackup).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/backups/{snapshotName}", s.handleDeleteBackup).Methods("DELETE")

	return s, c, r
}

func backupRequest(method, path, body string, admin bool) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	claims := &Claims{
		Sub:     "u-test",
		Email:   "test@agenttier.local",
		Name:    "Test User",
		IsAdmin: admin,
	}
	return req.WithContext(context.WithValue(req.Context(), ClaimsContextKey, claims))
}

func runningSandboxWithPVC(name, namespace, pvcName string) *agenttierv1alpha1.Sandbox {
	return &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agenttierv1alpha1.SandboxSpec{
			Mode:        agenttierv1alpha1.SandboxModeCode,
			TemplateRef: &agenttierv1alpha1.TemplateReference{Name: "general-coding", Kind: "ClusterSandboxTemplate"},
		},
		Status: agenttierv1alpha1.SandboxStatus{
			Phase:   agenttierv1alpha1.SandboxPhaseRunning,
			PVCName: pvcName,
		},
	}
}

func TestListBackups_ReturnsScheduledAndCloneSnapshots(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	scheduled := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-workspace-backup-100",
			Namespace: "default",
			Labels: map[string]string{
				backup.LabelSnapshotKind: backup.SnapshotKindBackup,
				backup.LabelSourcePVC:    "sbx-1-workspace",
			},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: boolPtr(true)},
	}
	clone := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-clonesnap-200",
			Namespace: "default",
			Labels:    map[string]string{backupSnapshotSourceSandboxLabel: "sbx-1"},
		},
	}
	s, _, r := backupFixture(t, sandbox, scheduled, clone)
	_ = s

	req := backupRequest(http.MethodGet, "/api/v1/sandboxes/sbx-1/backups", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Backups []backupInfo `json:"backups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Backups) != 2 {
		t.Fatalf("expected 2 backups, got %d: %+v", len(resp.Backups), resp.Backups)
	}
	kinds := map[string]bool{}
	for _, b := range resp.Backups {
		kinds[b.Kind] = true
	}
	if !kinds[backup.SnapshotKindBackup] || !kinds["clone"] {
		t.Errorf("expected both scheduled-backup and clone kinds, got %+v", resp.Backups)
	}
}

func TestListBackups_EmptyWhenSandboxHasNoPVC(t *testing.T) {
	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseCreating},
	}
	_, _, r := backupFixture(t, sandbox)

	req := backupRequest(http.MethodGet, "/api/v1/sandboxes/sbx-1/backups", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Backups []backupInfo `json:"backups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Backups) != 0 {
		t.Errorf("expected 0 backups, got %d", len(resp.Backups))
	}
}

func TestListBackups_UnauthorizedOtherUser(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	sandbox.Spec.CreatedBy = &agenttierv1alpha1.UserIdentity{Sub: "owner"}
	_, _, r := backupFixture(t, sandbox)

	req := backupRequest(http.MethodGet, "/api/v1/sandboxes/sbx-1/backups", "", false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (ownership-masked), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateBackup_HappyPath(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	_, c, r := backupFixture(t, sandbox)

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	var info backupInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if info.Kind != backup.SnapshotKindBackup {
		t.Errorf("kind = %q, want %q", info.Kind, backup.SnapshotKindBackup)
	}

	snapList := &snapshotv1.VolumeSnapshotList{}
	if err := c.List(context.Background(), snapList, client.InNamespace("default")); err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapList.Items) != 1 {
		t.Fatalf("expected 1 snapshot created, got %d", len(snapList.Items))
	}
	snap := snapList.Items[0]
	if snap.Labels[backup.LabelSnapshotKind] != backup.SnapshotKindBackup {
		t.Errorf("snapshot missing scheduled-backup label: %+v", snap.Labels)
	}
	if snap.Labels[backup.LabelSourcePVC] != "sbx-1-workspace" {
		t.Errorf("snapshot source-pvc label = %q, want sbx-1-workspace", snap.Labels[backup.LabelSourcePVC])
	}
	if snap.Labels[backup.LabelManaged] != "true" {
		t.Error("on-demand backup must be labeled managed=true so retention still prunes it (FR3.2)")
	}

	// FR5 event wiring: a successful on-demand backup must emit a
	// corev1.Event with Reason=BackupCreated against the sandbox, so the
	// controller's webhook_delivery loop (which watches Events keyed on
	// InvolvedObject.Kind=Sandbox) can eventually map it to the
	// "backup.created" webhook event type (task #43).
	events := &corev1.EventList{}
	if err := c.List(context.Background(), events, client.InNamespace("default")); err != nil {
		t.Fatalf("list events: %v", err)
	}
	found := false
	for i := range events.Items {
		if events.Items[i].Reason == "BackupCreated" {
			found = true
			if events.Items[i].InvolvedObject.Name != "sbx-1" || events.Items[i].InvolvedObject.Kind != "Sandbox" {
				t.Errorf("BackupCreated event InvolvedObject = %+v, want Sandbox/sbx-1", events.Items[i].InvolvedObject)
			}
			if events.Items[i].Type != corev1.EventTypeNormal {
				t.Errorf("BackupCreated event Type = %q, want Normal", events.Items[i].Type)
			}
		}
	}
	if !found {
		t.Error("expected a corev1.Event with Reason=BackupCreated, found none")
	}
}

func TestCreateBackup_RejectsSandboxWithoutPVC(t *testing.T) {
	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseCreating},
	}
	_, _, r := backupFixture(t, sandbox)

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no PVC), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteBackup_HappyPath(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-1-workspace"},
		},
	}
	_, c, r := backupFixture(t, sandbox, snap)

	req := backupRequest(http.MethodDelete, "/api/v1/sandboxes/sbx-1/backups/sbx-1-workspace-backup-100", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	remaining := &snapshotv1.VolumeSnapshot{}
	err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-1-workspace-backup-100", Namespace: "default"}, remaining)
	if err == nil {
		t.Error("expected snapshot to be deleted")
	}

	// FR5 event wiring: a successful prune must emit a corev1.Event with
	// Reason=BackupPruned (task #43 maps this to the "backup.pruned"
	// webhook event type).
	events := &corev1.EventList{}
	if err := c.List(context.Background(), events, client.InNamespace("default")); err != nil {
		t.Fatalf("list events: %v", err)
	}
	found := false
	for i := range events.Items {
		if events.Items[i].Reason == "BackupPruned" {
			found = true
			if events.Items[i].InvolvedObject.Name != "sbx-1" || events.Items[i].InvolvedObject.Kind != "Sandbox" {
				t.Errorf("BackupPruned event InvolvedObject = %+v, want Sandbox/sbx-1", events.Items[i].InvolvedObject)
			}
		}
	}
	if !found {
		t.Error("expected a corev1.Event with Reason=BackupPruned, found none")
	}
}

func TestDeleteBackup_NonexistentSnapshotIs404(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	_, _, r := backupFixture(t, sandbox)

	req := backupRequest(http.MethodDelete, "/api/v1/sandboxes/sbx-1/backups/does-not-exist", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeleteBackup_CrossTenantSnapshotIs404 is the regression guard for a
// cross-tenant IDOR: sbx-1 and sbx-2 share the "default" namespace (the
// common case — ownership is per-Sandbox CreatedBy.Sub, not per-namespace),
// so sbx-1's owner must NOT be able to delete a snapshot that belongs to
// sbx-2 just by naming it in the path for sbx-1.
func TestDeleteBackup_CrossTenantSnapshotIs404(t *testing.T) {
	sbx1 := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	sbx2snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-2-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-2-workspace"},
		},
	}
	_, c, r := backupFixture(t, sbx1, sbx2snap)

	req := backupRequest(http.MethodDelete, "/api/v1/sandboxes/sbx-1/backups/sbx-2-workspace-backup-100", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (cross-tenant snapshot must not be deletable via another sandbox's path), got %d body=%s", rec.Code, rec.Body.String())
	}

	// Confirm it genuinely was NOT deleted.
	remaining := &snapshotv1.VolumeSnapshot{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-2-workspace-backup-100", Namespace: "default"}, remaining); err != nil {
		t.Errorf("cross-tenant snapshot was deleted (IDOR): %v", err)
	}
}

func TestRestoreBackup_HappyPath(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-1-workspace"},
		},
	}
	_, c, r := backupFixture(t, sandbox, snap)

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups/sbx-1-workspace-backup-100/restore", `{"name":"sbx-1-restored"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["name"] != "sbx-1-restored" {
		t.Errorf("name = %v, want sbx-1-restored", resp["name"])
	}

	restored := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-1-restored", Namespace: "default"}, restored); err != nil {
		t.Fatalf("get restored sandbox: %v", err)
	}
	if restored.Spec.CloneFromSnapshot != "sbx-1-workspace-backup-100" {
		t.Errorf("CloneFromSnapshot = %q, want sbx-1-workspace-backup-100", restored.Spec.CloneFromSnapshot)
	}
	if restored.Spec.TemplateRef == nil || restored.Spec.TemplateRef.Name != "general-coding" {
		t.Errorf("restored template not inherited from source: %+v", restored.Spec.TemplateRef)
	}
}

func TestRestoreBackup_AutoGeneratesNameWhenOmitted(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-1-workspace"},
		},
	}
	_, _, r := backupFixture(t, sandbox, snap)

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups/sbx-1-workspace-backup-100/restore", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	name, _ := resp["name"].(string)
	if !strings.HasPrefix(name, "sbx-1-restore-") {
		t.Errorf("expected auto-generated name with prefix sbx-1-restore-, got %q", name)
	}
}

// TestRestoreBackup_PrunedSnapshotReturns404 covers the list/restore race
// (requirements.md edge case): a snapshot that no longer exists must fail
// with a clear 404, not a silent no-op.
func TestRestoreBackup_PrunedSnapshotReturns404(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	_, _, r := backupFixture(t, sandbox) // no snapshot object — simulates it having been pruned

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups/gone/restore", "", true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (pruned snapshot), got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRestoreBackup_CrossTenantSnapshotIs404 is the regression guard for a
// cross-tenant IDOR with data-exfiltration impact: without the ownership
// check, sbx-1's owner could restore from sbx-2's snapshot and thereby read
// sbx-2's volume contents in the resulting Sandbox. Must 404, and critically
// must NOT create any restore Sandbox.
func TestRestoreBackup_CrossTenantSnapshotIs404(t *testing.T) {
	sbx1 := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	sbx2snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-2-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-2-workspace"},
		},
	}
	_, c, r := backupFixture(t, sbx1, sbx2snap)

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups/sbx-2-workspace-backup-100/restore", `{"name":"sbx-1-restored"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (cross-tenant restore must be rejected), got %d body=%s", rec.Code, rec.Body.String())
	}

	restored := &agenttierv1alpha1.Sandbox{}
	err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-1-restored", Namespace: "default"}, restored)
	if err == nil {
		t.Error("SECURITY: a restore Sandbox was created from another tenant's snapshot (data exfiltration)")
	}
}

func TestRestoreBackup_RejectsInvalidName(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-1-workspace"},
		},
	}
	_, _, r := backupFixture(t, sandbox, snap)

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups/sbx-1-workspace-backup-100/restore", `{"name":"Bad Name"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (invalid name), got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRestoreBackup_GovernanceRejectsOverCap verifies NFR3: governance is
// re-checked at restore time, not only at original create time. A policy
// that would reject the restore due to the namespace quota already being at
// its cap must 403 with the same policy_violation shape create uses.
func TestRestoreBackup_GovernanceRejectsOverCap(t *testing.T) {
	sandbox := runningSandboxWithPVC("sbx-1", "default", "sbx-1-workspace")
	existing := runningSandboxWithPVC("sbx-existing", "default", "sbx-existing-workspace")
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1-workspace-backup-100",
			Namespace: "default",
			Labels:    map[string]string{backup.LabelSourcePVC: "sbx-1-workspace"},
		},
	}
	s, c, r := backupFixture(t, sandbox, existing, snap)

	policies := map[string]governance.Policy{"default": {MaxSandboxesTotal: 2}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(context.Background(), policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}
	_ = s

	req := backupRequest(http.MethodPost, "/api/v1/sandboxes/sbx-1/backups/sbx-1-workspace-backup-100/restore", `{"name":"sbx-1-restored"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (governance cap), got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "policy_violation") {
		t.Errorf("expected policy_violation error shape, got %s", rec.Body.String())
	}
}

func boolPtr(b bool) *bool { return &b }

func mustMarshalPolicies(t *testing.T, p map[string]governance.Policy) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal policies: %v", err)
	}
	return string(b)
}
