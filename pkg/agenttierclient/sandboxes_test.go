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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c, srv
}

func TestCreateSandbox(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sandboxId": "demo", "name": "demo", "namespace": "default", "status": "Creating",
		})
	})

	out, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{
		Name:        "demo",
		TemplateRef: &TemplateRef{Name: "general-coding"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/sandboxes" {
		t.Fatalf("method/path = %s %s, want POST /api/v1/sandboxes", gotMethod, gotPath)
	}
	if gotBody["name"] != "demo" {
		t.Errorf("body name = %v, want demo", gotBody["name"])
	}
	if out.SandboxID != "demo" || out.Status != "Creating" {
		t.Fatalf("out = %+v", out)
	}
}

func TestListSandboxes_QueryParams(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxes": []map[string]string{{"sandboxId": "a", "status": "Running"}},
		})
	})

	out, err := c.ListSandboxes(context.Background(), ListSandboxesOptions{Namespace: "ns1", Status: "Running"})
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if gotQuery != "namespace=ns1&status=Running" {
		t.Errorf("query = %q, want namespace=ns1&status=Running", gotQuery)
	}
	if len(out) != 1 || out[0].SandboxID != "a" {
		t.Fatalf("out = %+v", out)
	}
}

func TestGetSandbox_NotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "sandbox not found"})
	})

	_, err := c.GetSandbox(context.Background(), "missing")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", apiErr.StatusCode)
	}
}

func TestDeleteSandbox_NoContent(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteSandbox(context.Background(), "demo"); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
}

func TestStopResumeSandbox(t *testing.T) {
	var methods []string
	var paths []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	if err := c.StopSandbox(context.Background(), "demo"); err != nil {
		t.Fatalf("StopSandbox() error = %v", err)
	}
	if err := c.ResumeSandbox(context.Background(), "demo"); err != nil {
		t.Fatalf("ResumeSandbox() error = %v", err)
	}
	wantPaths := []string{"/api/v1/sandboxes/demo/stop", "/api/v1/sandboxes/demo/resume"}
	for i, p := range wantPaths {
		if paths[i] != p {
			t.Errorf("paths[%d] = %s, want %s", i, paths[i], p)
		}
		if methods[i] != http.MethodPost {
			t.Errorf("methods[%d] = %s, want POST", i, methods[i])
		}
	}
}

func TestCloneSandbox(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": "demo-clone-1", "namespace": "default", "snapshot": "demo-snap-1",
			"clonedFrom": "demo", "phase": "Pending",
		})
	})
	out, err := c.CloneSandbox(context.Background(), "demo", CloneSandboxRequest{Name: "demo-clone-1"})
	if err != nil {
		t.Fatalf("CloneSandbox() error = %v", err)
	}
	if out.Name != "demo-clone-1" || out.ClonedFrom != "demo" {
		t.Fatalf("out = %+v", out)
	}
}

func TestExecCommand(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Command != "echo hi" {
			t.Errorf("command = %q, want 'echo hi'", body.Command)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stdout": "hi\n", "stderr": "", "exitCode": 0})
	})
	out, err := c.ExecCommand(context.Background(), "demo", ExecRequest{Command: "echo hi"})
	if err != nil {
		t.Fatalf("ExecCommand() error = %v", err)
	}
	if out.Stdout != "hi\n" || out.ExitCode != 0 {
		t.Fatalf("out = %+v", out)
	}
}

func TestPatchSandbox(t *testing.T) {
	var gotMethod string
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxId":       "demo",
			"applied":         map[string]string{"idleTimeout": "immediately", "resources": "on-restart"},
			"restartRequired": true,
			"message":         "resource changes take effect after the sandbox is stopped and resumed",
		})
	})
	out, err := c.PatchSandbox(context.Background(), "demo", PatchSandboxRequest{
		IdleTimeout: "30m",
		Resources:   &ResourceRequirements{Requests: map[string]string{"cpu": "1"}},
	})
	if err != nil {
		t.Fatalf("PatchSandbox() error = %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	if gotBody["idleTimeout"] != "30m" {
		t.Errorf("body idleTimeout = %v, want 30m", gotBody["idleTimeout"])
	}
	if !out.RestartRequired {
		t.Errorf("RestartRequired = false, want true")
	}
	if out.Applied["resources"] != "on-restart" {
		t.Errorf("Applied[resources] = %q, want on-restart", out.Applied["resources"])
	}
}

func TestBulkCreateSandboxes(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "status": "created", "sandboxId": "a"},
				{"index": 1, "status": "error", "error": "template not found"},
			},
		})
	})
	items := []CreateSandboxRequest{
		{Name: "a", TemplateRef: &TemplateRef{Name: "t1"}},
		{Name: "b", TemplateRef: &TemplateRef{Name: "missing"}},
	}
	out, err := c.BulkCreateSandboxes(context.Background(), items)
	if err != nil {
		t.Fatalf("BulkCreateSandboxes() error = %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("Results = %+v, want 2 entries", out.Results)
	}
	if out.Results[0].Status != "created" || out.Results[1].Status != "error" {
		t.Fatalf("Results = %+v", out.Results)
	}
	rawItems, ok := gotBody["items"].([]any)
	if !ok || len(rawItems) != 2 {
		t.Fatalf("gotBody[items] = %v, want 2-element array", gotBody["items"])
	}
}

func TestBulkCreateSandboxes_CapExceeded(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "quota_would_exceed"})
	})
	_, err := c.BulkCreateSandboxes(context.Background(), []CreateSandboxRequest{{Name: "a", TemplateRef: &TemplateRef{Name: "t1"}}})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want 409", apiErr.StatusCode)
	}
}

func TestBulkAction(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "a", "status": "stopped"},
				{"id": "unknown", "status": "error", "error": "not found"},
			},
		})
	})
	out, err := c.BulkAction(context.Background(), "stop", []string{"a", "unknown"})
	if err != nil {
		t.Fatalf("BulkAction() error = %v", err)
	}
	if gotBody["action"] != "stop" {
		t.Errorf("body action = %v, want stop", gotBody["action"])
	}
	if len(out.Results) != 2 {
		t.Fatalf("Results = %+v", out.Results)
	}
}

func TestListFiles(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": "/workspace",
			"entries": []map[string]any{
				{"name": "main.go", "size": 42, "isDir": false},
			},
		})
	})
	out, err := c.ListFiles(context.Background(), "demo", "/workspace")
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if gotQuery != "path=%2Fworkspace" {
		t.Errorf("query = %q, want path=%%2Fworkspace", gotQuery)
	}
	if len(out) != 1 || out[0].Name != "main.go" {
		t.Fatalf("out = %+v", out)
	}
}

func TestGetFile_RawBytes(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes/demo/files/workspace/notes.txt" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("hello world"))
	})
	data, err := c.GetFile(context.Background(), "demo", "workspace/notes.txt")
	if err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("data = %q, want 'hello world'", data)
	}
}

func TestPutFile_RawBytes(t *testing.T) {
	var gotContentType string
	var gotBody []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"path": "/workspace/notes.txt", "bytes": len(gotBody)})
	})
	out, err := c.PutFile(context.Background(), "demo", "workspace/notes.txt", []byte("payload"))
	if err != nil {
		t.Fatalf("PutFile() error = %v", err)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", gotContentType)
	}
	if string(gotBody) != "payload" {
		t.Errorf("body = %q, want payload", gotBody)
	}
	if out.Bytes != len("payload") {
		t.Errorf("Bytes = %d, want %d", out.Bytes, len("payload"))
	}
}

func TestListForwardCreateRemovePorts(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{{"port": 8080, "protocol": "http"}}})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"port": 9090, "protocol": "http"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ports, err := c.ListPorts(context.Background(), "demo")
	if err != nil {
		t.Fatalf("ListPorts() error = %v", err)
	}
	if len(ports) != 1 || ports[0].Port != 8080 {
		t.Fatalf("ports = %+v", ports)
	}
	fp, err := c.ForwardPort(context.Background(), "demo", 9090, "http")
	if err != nil {
		t.Fatalf("ForwardPort() error = %v", err)
	}
	if fp.Port != 9090 {
		t.Fatalf("fp = %+v", fp)
	}
	if err := c.RemovePort(context.Background(), "demo", 9090); err != nil {
		t.Fatalf("RemovePort() error = %v", err)
	}
}

func TestSharingLifecycle(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/demo/share":
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{}, "groups": []any{}, "shareLinks": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/demo/share":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"users": []map[string]string{{"identity": "alice@example.com", "level": "viewer"}},
			})
		case r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "revoked", "identity": "alice@example.com"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/demo/share-links":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "link1", "token": "raw-token", "level": "viewer"})
		}
	})
	if _, err := c.GetSharing(context.Background(), "demo"); err != nil {
		t.Fatalf("GetSharing() error = %v", err)
	}
	info, err := c.ShareSandbox(context.Background(), "demo", ShareSandboxRequest{Identity: "alice@example.com", Level: "viewer"})
	if err != nil {
		t.Fatalf("ShareSandbox() error = %v", err)
	}
	if len(info.Users) != 1 || info.Users[0].Identity != "alice@example.com" {
		t.Fatalf("info = %+v", info)
	}
	if err := c.RevokeShare(context.Background(), "demo", "alice@example.com"); err != nil {
		t.Fatalf("RevokeShare() error = %v", err)
	}
	link, err := c.CreateShareLink(context.Background(), "demo", CreateShareLinkRequest{Level: "viewer"})
	if err != nil {
		t.Fatalf("CreateShareLink() error = %v", err)
	}
	if link.Token != "raw-token" {
		t.Fatalf("link = %+v", link)
	}
}

func TestBackupsLifecycle(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/demo/backups":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"backups": []map[string]any{{"name": "snap-1", "readyToUse": true}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/demo/backups":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "snap-2", "readyToUse": false})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/demo/backups/snap-1/restore":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "demo-restored", "clonedFrom": "demo"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	backups, err := c.ListBackups(context.Background(), "demo")
	if err != nil {
		t.Fatalf("ListBackups() error = %v", err)
	}
	if len(backups) != 1 || backups[0].Name != "snap-1" {
		t.Fatalf("backups = %+v", backups)
	}
	created, err := c.CreateBackup(context.Background(), "demo", CreateBackupRequest{})
	if err != nil {
		t.Fatalf("CreateBackup() error = %v", err)
	}
	if created.Name != "snap-2" {
		t.Fatalf("created = %+v", created)
	}
	restored, err := c.RestoreBackup(context.Background(), "demo", "snap-1", RestoreBackupRequest{})
	if err != nil {
		t.Fatalf("RestoreBackup() error = %v", err)
	}
	if restored.Name != "demo-restored" {
		t.Fatalf("restored = %+v", restored)
	}
	if err := c.DeleteBackup(context.Background(), "demo", "snap-1"); err != nil {
		t.Fatalf("DeleteBackup() error = %v", err)
	}
}

func TestBackupRestore_PrunedSnapshot(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "snapshot not found"})
	})
	_, err := c.RestoreBackup(context.Background(), "demo", "gone", RestoreBackupRequest{})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", apiErr.StatusCode)
	}
}

func TestTemplatesLifecycle(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates":
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": []map[string]any{{"name": "t1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates/t1":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "t1", "description": "desc"})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "t2"})
		case r.Method == http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "t1", "description": "updated"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	templates, err := c.ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(templates) != 1 || templates[0].Name != "t1" {
		t.Fatalf("templates = %+v", templates)
	}
	tmpl, err := c.GetTemplate(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if tmpl.Description != "desc" {
		t.Fatalf("tmpl = %+v", tmpl)
	}
	if _, err := c.CreateTemplate(context.Background(), CreateTemplateRequest{Name: "t2", Spec: map[string]any{}}); err != nil {
		t.Fatalf("CreateTemplate() error = %v", err)
	}
	updated, err := c.UpdateTemplate(context.Background(), "t1", UpdateTemplateRequest{Spec: map[string]any{}})
	if err != nil {
		t.Fatalf("UpdateTemplate() error = %v", err)
	}
	if updated.Description != "updated" {
		t.Fatalf("updated = %+v", updated)
	}
	if err := c.DeleteTemplate(context.Background(), "t1"); err != nil {
		t.Fatalf("DeleteTemplate() error = %v", err)
	}
}
