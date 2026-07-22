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

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFilesLs(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": "/workspace",
			"entries": []map[string]any{
				{"name": "main.go", "size": 42, "isDir": false},
			},
		})
	})
	out, code := captureStdout(t, func() int {
		return runFilesLs(append(baseArgs, "demo"))
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("output = %q", out)
	}
}

func TestRunFilesCat(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("file contents"))
	})
	out, code := captureStdout(t, func() int {
		return runFilesCat(append(baseArgs, "demo", "/workspace/notes.txt"))
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if out != "file contents" {
		t.Errorf("out = %q", out)
	}
}

func TestRunFilesUploadDownload(t *testing.T) {
	var uploadedBody []byte
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			buf, _ := io.ReadAll(r.Body)
			uploadedBody = buf
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"path": "/workspace/out.txt", "bytes": len(buf)})
		case http.MethodGet:
			_, _ = w.Write([]byte("downloaded-content"))
		}
	})

	dir := t.TempDir()
	localUpload := filepath.Join(dir, "upload.txt")
	if err := os.WriteFile(localUpload, []byte("upload-payload"), 0o644); err != nil {
		t.Fatalf("seed upload file: %v", err)
	}
	code := runFilesUpload(append(baseArgs, "demo", localUpload, "/workspace/out.txt"))
	if code != 0 {
		t.Fatalf("upload exit code = %d", code)
	}
	if string(uploadedBody) != "upload-payload" {
		t.Errorf("uploadedBody = %q", uploadedBody)
	}

	localDownload := filepath.Join(dir, "download.txt")
	code = runFilesDownload(append(baseArgs, "demo", "/workspace/out.txt", localDownload))
	if code != 0 {
		t.Fatalf("download exit code = %d", code)
	}
	data, err := os.ReadFile(localDownload)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != "downloaded-content" {
		t.Errorf("downloaded data = %q", data)
	}
}

func TestRunFilesWrite_RequiresDataOrFile(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without --data or --file")
	})
	code := runFilesWrite(append(baseArgs, "demo", "/workspace/x.txt"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunFilesWrite_InlineData(t *testing.T) {
	var gotBody []byte
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		gotBody = buf
		_ = json.NewEncoder(w).Encode(map[string]any{"path": "/workspace/x.txt", "bytes": len(buf)})
	})
	args := append(baseArgs, "--data", "hello", "demo", "/workspace/x.txt")
	code := runFilesWrite(args)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if string(gotBody) != "hello" {
		t.Errorf("gotBody = %q", gotBody)
	}
}
