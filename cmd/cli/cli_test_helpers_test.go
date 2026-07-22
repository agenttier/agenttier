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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// newFakeRouter starts an httptest.Server and returns args pre-populated
// with --api-url pointing at it, so run* functions under test talk to a
// fake Router instead of a real one. Callers append their own flags/args.
func newFakeRouter(t *testing.T, handler http.HandlerFunc) (args []string, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return []string{"--api-url", srv.URL}, srv
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Used because the CLI command functions write
// directly to os.Stdout (via fmt.Print*/printJSON) rather than accepting
// an io.Writer — matching the existing legacy commands' style in main.go.
func captureStdout(t *testing.T, fn func() int) (output string, exitCode int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	exitCode = fn()

	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(data), exitCode
}
