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
	"fmt"
	"net/url"
)

// Sandbox lifecycle, patch, bulk, backups, sharing, and template methods.
// Wire shapes mirror pkg/router/handlers.go (sandboxToJSON, templateToJSON,
// sharingToJSON) for endpoints that exist today, and design.md's Interface
// Contracts section for FR2 (PATCH)/FR3 (backups)/FR4 (bulk), which land in
// Group 2 of this build in parallel with this client. Local types are used
// throughout instead of importing api/v1alpha1 or k8s.io/api/core/v1 so this
// package (and anything built on it, like cmd/cli) stays free of the
// controller-runtime dependency tree.

// --- sandbox CRUD ---------------------------------------------------------

// TemplateRef identifies the SandboxTemplate or ClusterSandboxTemplate a
// sandbox is created from.
type TemplateRef struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// StorageSpec configures the sandbox's persistent volume at create time.
type StorageSpec struct {
	Size         string `json:"size,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
}

// CreateSandboxRequest is the body of POST /sandboxes (and, as an element,
// of POST /sandboxes/bulk's "items" array).
type CreateSandboxRequest struct {
	Name        string       `json:"name"`
	Namespace   string       `json:"namespace,omitempty"`
	TemplateRef *TemplateRef `json:"templateRef"`
	Timeout     string       `json:"timeout,omitempty"`
	IdleTimeout string       `json:"idleTimeout,omitempty"`
	Storage     *StorageSpec `json:"storage,omitempty"`
}

// CreatedBy identifies the user who created a sandbox.
type CreatedBy struct {
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// Sandbox is the wire shape returned by create/get/list — see
// sandboxToJSON in pkg/router/handlers.go.
type Sandbox struct {
	SandboxID      string     `json:"sandboxId"`
	Name           string     `json:"name"`
	Namespace      string     `json:"namespace"`
	Status         string     `json:"status"`
	PodName        string     `json:"podName,omitempty"`
	PVCName        string     `json:"pvcName,omitempty"`
	TemplateRef    string     `json:"templateRef,omitempty"`
	Mode           string     `json:"mode,omitempty"`
	CreatedAt      string     `json:"createdAt,omitempty"`
	LastActivityAt string     `json:"lastActivityAt,omitempty"`
	Message        string     `json:"message,omitempty"`
	CreatedByUser  *CreatedBy `json:"createdBy,omitempty"`
}

// ListSandboxesOptions filters GET /sandboxes.
type ListSandboxesOptions struct {
	Namespace string
	Status    string
}

// CreateSandbox issues POST /sandboxes.
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	var out Sandbox
	if err := c.Post(ctx, "/sandboxes", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSandboxes issues GET /sandboxes.
func (c *Client) ListSandboxes(ctx context.Context, opts ListSandboxesOptions) ([]Sandbox, error) {
	q := url.Values{}
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	path := "/sandboxes"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := c.Get(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

// GetSandbox issues GET /sandboxes/{id}.
func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var out Sandbox
	if err := c.Get(ctx, "/sandboxes/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSandbox issues DELETE /sandboxes/{id}.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.Delete(ctx, "/sandboxes/"+url.PathEscape(id), nil)
}

// StopSandbox issues POST /sandboxes/{id}/stop.
func (c *Client) StopSandbox(ctx context.Context, id string) error {
	return c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/stop", nil, nil)
}

// ResumeSandbox issues POST /sandboxes/{id}/resume.
func (c *Client) ResumeSandbox(ctx context.Context, id string) error {
	return c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/resume", nil, nil)
}

// CloneSandboxRequest is the body of POST /sandboxes/{id}/clone.
type CloneSandboxRequest struct {
	Name          string `json:"name,omitempty"`
	SnapshotClass string `json:"snapshotClass,omitempty"`
}

// CloneSandboxResult is the response of POST /sandboxes/{id}/clone (and,
// per design.md, the analogous restore-from-backup response).
type CloneSandboxResult struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Snapshot   string `json:"snapshot"`
	ClonedFrom string `json:"clonedFrom"`
	Phase      string `json:"phase"`
	Message    string `json:"message"`
}

// CloneSandbox issues POST /sandboxes/{id}/clone.
func (c *Client) CloneSandbox(ctx context.Context, id string, req CloneSandboxRequest) (*CloneSandboxResult, error) {
	var out CloneSandboxResult
	if err := c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/clone", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- exec ------------------------------------------------------------------

// ExecRequest is the body of POST /sandboxes/{id}/exec.
type ExecRequest struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// CommandResult is the response of POST /sandboxes/{id}/exec — see
// pkg/router/terminal.CommandResult.
type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// ExecCommand issues POST /sandboxes/{id}/exec.
func (c *Client) ExecCommand(ctx context.Context, id string, req ExecRequest) (*CommandResult, error) {
	var out CommandResult
	if err := c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/exec", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- FR2: PATCH /sandboxes/{id} (design.md#FR2) ---------------------------

// ResourceRequirements mirrors corev1.ResourceRequirements' JSON shape
// (requests/limits maps of resource name -> Kubernetes quantity string,
// e.g. {"cpu":"1","memory":"2Gi"}) without importing k8s.io/api/core/v1.
type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// PatchSandboxRequest is the body of PATCH /sandboxes/{id}. At least one
// field must be set — the caller is expected to validate this locally
// (FR1.10) before the round-trip; the Router also enforces it server-side.
type PatchSandboxRequest struct {
	IdleTimeout string                `json:"idleTimeout,omitempty"`
	Resources   *ResourceRequirements `json:"resources,omitempty"`
	Labels      map[string]string     `json:"labels,omitempty"`
	Annotations map[string]string     `json:"annotations,omitempty"`
}

// PatchSandboxResult is the response of PATCH /sandboxes/{id} — see
// design.md's Interface Contracts / FR2 section. Applied reports, per
// field, whether the change took effect "immediately" or "on-restart".
type PatchSandboxResult struct {
	SandboxID       string            `json:"sandboxId"`
	Applied         map[string]string `json:"applied"`
	RestartRequired bool              `json:"restartRequired"`
	Message         string            `json:"message,omitempty"`
}

// PatchSandbox issues PATCH /sandboxes/{id}.
func (c *Client) PatchSandbox(ctx context.Context, id string, req PatchSandboxRequest) (*PatchSandboxResult, error) {
	var out PatchSandboxResult
	if err := c.Patch(ctx, "/sandboxes/"+url.PathEscape(id), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- FR4: bulk operations (design.md#FR4) ----------------------------------

// BulkCreateRequest is the body of POST /sandboxes/bulk.
type BulkCreateRequest struct {
	Items []CreateSandboxRequest `json:"items"`
}

// BulkCreateItemResult is one element of BulkCreateResult.Results.
type BulkCreateItemResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"` // "created" or "error"
	SandboxID string `json:"sandboxId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// BulkCreateResult is the response of POST /sandboxes/bulk.
type BulkCreateResult struct {
	Results []BulkCreateItemResult `json:"results"`
}

// BulkCreateSandboxes issues POST /sandboxes/bulk. A whole-batch
// governance-cap rejection (DD4) surfaces as a non-nil *APIError with
// StatusCode 409, not a partial BulkCreateResult.
func (c *Client) BulkCreateSandboxes(ctx context.Context, items []CreateSandboxRequest) (*BulkCreateResult, error) {
	var out BulkCreateResult
	if err := c.Post(ctx, "/sandboxes/bulk", BulkCreateRequest{Items: items}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BulkActionRequest is the body of POST /sandboxes/bulk-action.
type BulkActionRequest struct {
	Action string   `json:"action"` // "stop", "resume", or "delete"
	IDs    []string `json:"ids"`
}

// BulkActionItemResult is one element of BulkActionResult.Results.
type BulkActionItemResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// BulkActionResult is the response of POST /sandboxes/bulk-action.
type BulkActionResult struct {
	Results []BulkActionItemResult `json:"results"`
}

// BulkAction issues POST /sandboxes/bulk-action.
func (c *Client) BulkAction(ctx context.Context, action string, ids []string) (*BulkActionResult, error) {
	var out BulkActionResult
	if err := c.Post(ctx, "/sandboxes/bulk-action", BulkActionRequest{Action: action, IDs: ids}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- files -----------------------------------------------------------------

// FileEntry is one entry returned by ListFiles.
type FileEntry struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"isDir"`
	Mode       string `json:"mode,omitempty"`
	ModifiedAt int64  `json:"modifiedAt,omitempty"`
}

// ListFiles issues GET /sandboxes/{id}/files/?path=<dir>.
func (c *Client) ListFiles(ctx context.Context, id, dir string) ([]FileEntry, error) {
	path := "/sandboxes/" + url.PathEscape(id) + "/files/"
	if dir != "" {
		path += "?" + url.Values{"path": {dir}}.Encode()
	}
	var out struct {
		Path    string      `json:"path"`
		Entries []FileEntry `json:"entries"`
	}
	if err := c.Get(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// GetFile issues GET /sandboxes/{id}/files/{path} and returns the raw file
// bytes (the Router responds with application/octet-stream, not JSON).
func (c *Client) GetFile(ctx context.Context, id, filePath string) ([]byte, error) {
	path := "/sandboxes/" + url.PathEscape(id) + "/files/" + encodeFilePath(filePath)
	return c.DoRawDownload(ctx, path)
}

// PutFileResult is the response of PUT /sandboxes/{id}/files/{path}.
type PutFileResult struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

// PutFile issues PUT /sandboxes/{id}/files/{path} with the raw file
// contents as the request body.
func (c *Client) PutFile(ctx context.Context, id, filePath string, data []byte) (*PutFileResult, error) {
	path := "/sandboxes/" + url.PathEscape(id) + "/files/" + encodeFilePath(filePath)
	var out PutFileResult
	if err := c.DoRawUpload(ctx, "PUT", path, "application/octet-stream", data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// encodeFilePath escapes each path segment individually so a multi-
// segment sandbox path (e.g. "workspace/notes.txt") round-trips through
// the Router's {path:.*} mux variable without "/" itself being escaped.
func encodeFilePath(filePath string) string {
	segments := splitPath(filePath)
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return joinPath(segments)
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	out = append(out, p[start:])
	return out
}

func joinPath(segments []string) string {
	out := ""
	for i, seg := range segments {
		if i > 0 {
			out += "/"
		}
		out += seg
	}
	return out
}

// --- ports -------------------------------------------------------------

// ForwardedPort is a port exposed from the sandbox via the Router.
type ForwardedPort struct {
	Port        int32  `json:"port"`
	Protocol    string `json:"protocol"`
	PreviewURL  string `json:"previewUrl,omitempty"`
	InternalURL string `json:"internalUrl,omitempty"`
}

// ListPorts issues GET /sandboxes/{id}/ports.
func (c *Client) ListPorts(ctx context.Context, id string) ([]ForwardedPort, error) {
	var out struct {
		Ports []ForwardedPort `json:"ports"`
	}
	if err := c.Get(ctx, "/sandboxes/"+url.PathEscape(id)+"/ports", &out); err != nil {
		return nil, err
	}
	return out.Ports, nil
}

// ForwardPort issues POST /sandboxes/{id}/ports.
func (c *Client) ForwardPort(ctx context.Context, id string, port int32, protocol string) (*ForwardedPort, error) {
	body := struct {
		Port     int32  `json:"port"`
		Protocol string `json:"protocol"`
	}{Port: port, Protocol: protocol}
	var out ForwardedPort
	if err := c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/ports", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemovePort issues DELETE /sandboxes/{id}/ports/{port}.
func (c *Client) RemovePort(ctx context.Context, id string, port int32) error {
	return c.Delete(ctx, fmt.Sprintf("/sandboxes/%s/ports/%d", url.PathEscape(id), port), nil)
}

// --- sharing -----------------------------------------------------------

// SharePermission is one identity's access level on a sandbox.
type SharePermission struct {
	Identity string `json:"identity"`
	Level    string `json:"level"`
}

// ShareLinkInfo is share-link metadata (never the raw token, except at
// creation — see CreateShareLinkResult).
type ShareLinkInfo struct {
	ID        string `json:"id"`
	Level     string `json:"level"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	MaxUses   int    `json:"maxUses,omitempty"`
	UsedCount int    `json:"usedCount,omitempty"`
}

// SharingInfo is the response of GET/POST /sandboxes/{id}/share — see
// sharingToJSON in pkg/router/handlers.go.
type SharingInfo struct {
	Users      []SharePermission `json:"users"`
	Groups     []SharePermission `json:"groups"`
	ShareLinks []ShareLinkInfo   `json:"shareLinks"`
}

// GetSharing issues GET /sandboxes/{id}/share.
func (c *Client) GetSharing(ctx context.Context, id string) (*SharingInfo, error) {
	var out SharingInfo
	if err := c.Get(ctx, "/sandboxes/"+url.PathEscape(id)+"/share", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ShareSandboxRequest is the body of POST /sandboxes/{id}/share.
type ShareSandboxRequest struct {
	Identity string `json:"identity"`
	Level    string `json:"level"`          // "viewer" or "collaborator"
	Kind     string `json:"kind,omitempty"` // "user" (default) or "group"
}

// ShareSandbox issues POST /sandboxes/{id}/share.
func (c *Client) ShareSandbox(ctx context.Context, id string, req ShareSandboxRequest) (*SharingInfo, error) {
	var out SharingInfo
	if err := c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/share", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeShare issues DELETE /sandboxes/{id}/share/{userId}.
func (c *Client) RevokeShare(ctx context.Context, id, userID string) error {
	return c.Delete(ctx, "/sandboxes/"+url.PathEscape(id)+"/share/"+url.PathEscape(userID), nil)
}

// CreateShareLinkRequest is the body of POST /sandboxes/{id}/share-links.
type CreateShareLinkRequest struct {
	Level     string `json:"level,omitempty"`
	ExpiresIn string `json:"expiresIn,omitempty"`
	MaxUses   int    `json:"maxUses,omitempty"`
}

// CreateShareLinkResult is the response of POST /sandboxes/{id}/share-links.
// Token is populated exactly once, at creation.
type CreateShareLinkResult struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Level     string `json:"level"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	MaxUses   int    `json:"maxUses,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

// CreateShareLink issues POST /sandboxes/{id}/share-links.
func (c *Client) CreateShareLink(ctx context.Context, id string, req CreateShareLinkRequest) (*CreateShareLinkResult, error) {
	var out CreateShareLinkResult
	if err := c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/share-links", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- FR3: backups (design.md#FR3) -------------------------------------

// BackupSnapshot describes one VolumeSnapshot associated with a sandbox.
type BackupSnapshot struct {
	Name        string `json:"name"`
	CreatedAt   string `json:"createdAt,omitempty"`
	Kind        string `json:"kind,omitempty"`
	ReadyToUse  bool   `json:"readyToUse"`
	RestoreSize string `json:"restoreSize,omitempty"`
}

// ListBackups issues GET /sandboxes/{id}/backups.
func (c *Client) ListBackups(ctx context.Context, id string) ([]BackupSnapshot, error) {
	var out struct {
		Backups []BackupSnapshot `json:"backups"`
	}
	if err := c.Get(ctx, "/sandboxes/"+url.PathEscape(id)+"/backups", &out); err != nil {
		return nil, err
	}
	return out.Backups, nil
}

// CreateBackupRequest is the body of POST /sandboxes/{id}/backups.
type CreateBackupRequest struct {
	Name          string `json:"name,omitempty"`
	SnapshotClass string `json:"snapshotClass,omitempty"`
}

// CreateBackup issues POST /sandboxes/{id}/backups.
func (c *Client) CreateBackup(ctx context.Context, id string, req CreateBackupRequest) (*BackupSnapshot, error) {
	var out BackupSnapshot
	if err := c.Post(ctx, "/sandboxes/"+url.PathEscape(id)+"/backups", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestoreBackupRequest is the body of POST
// /sandboxes/{id}/backups/{snapshotName}/restore.
type RestoreBackupRequest struct {
	Name string `json:"name,omitempty"`
}

// RestoreBackup issues POST /sandboxes/{id}/backups/{snapshotName}/restore.
// A snapshot pruned between list and restore surfaces as *APIError with
// StatusCode 404 or 409 (design.md's Edge Cases section).
func (c *Client) RestoreBackup(ctx context.Context, id, snapshotName string, req RestoreBackupRequest) (*CloneSandboxResult, error) {
	path := "/sandboxes/" + url.PathEscape(id) + "/backups/" + url.PathEscape(snapshotName) + "/restore"
	var out CloneSandboxResult
	if err := c.Post(ctx, path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteBackup issues DELETE /sandboxes/{id}/backups/{snapshotName}.
func (c *Client) DeleteBackup(ctx context.Context, id, snapshotName string) error {
	return c.Delete(ctx, "/sandboxes/"+url.PathEscape(id)+"/backups/"+url.PathEscape(snapshotName), nil)
}

// --- templates -----------------------------------------------------------

// Template is a ClusterSandboxTemplate as exposed by the Router.
type Template struct {
	Name            string         `json:"name"`
	Description     string         `json:"description,omitempty"`
	Image           string         `json:"image,omitempty"`
	ResourceVersion string         `json:"resourceVersion,omitempty"`
	Spec            map[string]any `json:"spec,omitempty"`
}

// ListTemplates issues GET /templates.
func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var out struct {
		Templates []Template `json:"templates"`
	}
	if err := c.Get(ctx, "/templates", &out); err != nil {
		return nil, err
	}
	return out.Templates, nil
}

// GetTemplate issues GET /templates/{name}.
func (c *Client) GetTemplate(ctx context.Context, name string) (*Template, error) {
	var out Template
	if err := c.Get(ctx, "/templates/"+url.PathEscape(name), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTemplateRequest is the body of POST /templates (admin-only).
type CreateTemplateRequest struct {
	Name string         `json:"name"`
	Spec map[string]any `json:"spec"`
}

// CreateTemplate issues POST /templates (admin-only).
func (c *Client) CreateTemplate(ctx context.Context, req CreateTemplateRequest) (*Template, error) {
	var out Template
	if err := c.Post(ctx, "/templates", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateTemplateRequest is the body of PUT /templates/{name} (admin-only).
type UpdateTemplateRequest struct {
	Spec map[string]any `json:"spec"`
}

// UpdateTemplate issues PUT /templates/{name} (admin-only).
func (c *Client) UpdateTemplate(ctx context.Context, name string, req UpdateTemplateRequest) (*Template, error) {
	var out Template
	if err := c.Put(ctx, "/templates/"+url.PathEscape(name), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTemplate issues DELETE /templates/{name} (admin-only).
func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	return c.Delete(ctx, "/templates/"+url.PathEscape(name), nil)
}
