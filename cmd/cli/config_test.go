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
	"os"
	"path/filepath"
	"testing"
)

func TestCLIConfig_ResolveRequiresAPIURL(t *testing.T) {
	t.Setenv("AGENTTIER_API_URL", "")
	t.Setenv("AGENTTIER_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	c := &cliConfig{output: "text"}
	if err := c.resolve(); err == nil {
		t.Fatal("expected error when no api-url configured, got nil")
	}
}

func TestCLIConfig_ResolveFallsBackToEnv(t *testing.T) {
	t.Setenv("AGENTTIER_API_URL", "https://example.com")
	t.Setenv("AGENTTIER_API_KEY", "env-key")
	t.Setenv("AGENTTIER_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	c := &cliConfig{output: "text"}
	if err := c.resolve(); err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if c.apiURL != "https://example.com" {
		t.Errorf("apiURL = %q, want https://example.com", c.apiURL)
	}
	if c.apiKey != "env-key" {
		t.Errorf("apiKey = %q, want env-key", c.apiKey)
	}
}

func TestCLIConfig_ResolveRejectsBadOutput(t *testing.T) {
	t.Setenv("AGENTTIER_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	c := &cliConfig{apiURL: "https://example.com", output: "yaml"}
	if err := c.resolve(); err == nil {
		t.Fatal("expected error for invalid --output value, got nil")
	}
}

func TestCLIConfig_FlagsWinOverEnvAndSavedConfig(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	t.Setenv("AGENTTIER_CONFIG", configFile)
	if err := saveConfig(savedConfig{APIURL: "https://saved.example.com", APIKey: "saved-key"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	t.Setenv("AGENTTIER_API_URL", "https://env.example.com")

	c := &cliConfig{apiURL: "https://flag.example.com", output: "text"}
	if err := c.resolve(); err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if c.apiURL != "https://flag.example.com" {
		t.Errorf("apiURL = %q, want the flag value (highest precedence)", c.apiURL)
	}
	// apiKey wasn't set via flag/env, so it should fall through to saved config.
	if c.apiKey != "saved-key" {
		t.Errorf("apiKey = %q, want saved-key (saved config is the lowest-precedence fallback)", c.apiKey)
	}
}

func TestSaveConfig_WritesRestrictedPermissions(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "nested", "config.json")
	t.Setenv("AGENTTIER_CONFIG", configFile)

	if err := saveConfig(savedConfig{APIURL: "https://example.com", APIKey: "secret"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 0600", perm)
	}

	loaded := loadSavedConfig()
	if loaded.APIURL != "https://example.com" || loaded.APIKey != "secret" {
		t.Errorf("loaded = %+v, want round-tripped values", loaded)
	}
}

func TestLoadSavedConfig_MissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("AGENTTIER_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.json"))
	cfg := loadSavedConfig()
	if cfg != (savedConfig{}) {
		t.Errorf("cfg = %+v, want zero value for missing file", cfg)
	}
}
