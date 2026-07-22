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
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/agenttier/agenttier/pkg/agenttierclient"
)

// cliConfig resolves connection settings for the FR1.9 command families
// (sandbox/template/governance/audit/analytics/admin/user/apikeys/warmpool/
// cluster/webhooks). Kept separate from main.go's globalFlags — which the
// pre-existing configure/invoke commands use directly against net/http for
// SSE streaming — per decisions.md DL5: the shared client is additive, and
// those commands may migrate later or stay as-is.
type cliConfig struct {
	apiURL string
	apiKey string
	token  string
	output string // "text" or "json"
}

// savedConfig is the on-disk shape of ~/.config/agenttier/config.json,
// written by `agenttier login` and read as the lowest-precedence source
// for connection settings (CLI flag > env var > saved config).
type savedConfig struct {
	APIURL string `json:"api_url,omitempty"`
	APIKey string `json:"api_key,omitempty"`
	Token  string `json:"token,omitempty"`
}

// configPath returns the on-disk config file location, honoring
// AGENTTIER_CONFIG the same way the Python CLI does.
func configPath() string {
	if p := os.Getenv("AGENTTIER_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agenttier", "config.json")
}

// loadSavedConfig reads the config file. A missing or unreadable file is
// not an error — it just means no saved config exists yet.
func loadSavedConfig() savedConfig {
	path := configPath()
	if path == "" {
		return savedConfig{}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is this CLI's own fixed config location (configPath()), not attacker-controlled
	if err != nil {
		return savedConfig{}
	}
	var cfg savedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return savedConfig{}
	}
	return cfg
}

// saveConfig writes cfg to disk with 0600 permissions — the file may carry
// an API key or bearer token in plaintext.
func saveConfig(cfg savedConfig) error {
	path := configPath()
	if path == "" {
		return errors.New("cannot determine config file path (no home directory)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// #nosec G117 -- intentionally persisting the credential the user asked `login` to save (APIKey/Token fields); the write below is 0600
	data, err := json.MarshalIndent(cfg, "", "  ") //nolint:gosec // G117: intentionally persisting the credential the user asked `login` to save; the write below is 0600
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// registerCLIFlags wires the standard connection + output flags onto fs.
// Precedence resolved in resolveCLIConfig: CLI flag > env var > saved
// config file.
func registerCLIFlags(fs *flag.FlagSet) *cliConfig {
	c := &cliConfig{}
	fs.StringVar(&c.apiURL, "api-url", "", "Router base URL (env: AGENTTIER_API_URL).")
	fs.StringVar(&c.apiKey, "api-key", "", "API key (env: AGENTTIER_API_KEY).")
	fs.StringVar(&c.token, "token", "", "Bearer token / OIDC JWT (env: AGENTTIER_TOKEN).")
	fs.StringVar(&c.output, "output", "text", `Output format: "text" or "json".`)
	return c
}

// resolve fills in any unset fields from environment variables and the
// saved config file, in that precedence order (flags already won by being
// set directly on c by flag.Parse).
func (c *cliConfig) resolve() error {
	saved := loadSavedConfig()
	if c.apiURL == "" {
		c.apiURL = os.Getenv("AGENTTIER_API_URL")
	}
	if c.apiURL == "" {
		c.apiURL = saved.APIURL
	}
	if c.apiKey == "" {
		c.apiKey = os.Getenv("AGENTTIER_API_KEY")
	}
	if c.apiKey == "" {
		c.apiKey = saved.APIKey
	}
	if c.token == "" {
		c.token = os.Getenv("AGENTTIER_TOKEN")
	}
	if c.token == "" {
		c.token = saved.Token
	}
	if c.apiURL == "" {
		return errors.New("no API URL configured; pass --api-url, set AGENTTIER_API_URL, or run `agenttier login --api-url <URL>`")
	}
	if c.output != "text" && c.output != "json" {
		return errors.New(`--output must be "text" or "json"`)
	}
	return nil
}

// client builds an agenttierclient.Client from the resolved config.
func (c *cliConfig) client() (*agenttierclient.Client, error) {
	return agenttierclient.New(agenttierclient.Config{
		APIURL:      c.apiURL,
		APIKey:      c.apiKey,
		BearerToken: c.token,
	})
}
