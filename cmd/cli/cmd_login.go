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
	"context"
	"flag"
	"fmt"
	"os"
)

// runLogin implements `agenttier login --api-url ... [--api-key ...|--token
// ...]`, saving connection settings to ~/.config/agenttier/config.json
// (or $AGENTTIER_CONFIG) and verifying them against GET /user/me — mirrors
// the Python CLI's cmd_login.
func runLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cfg.apiURL == "" {
		fmt.Fprintln(os.Stderr, "login requires --api-url")
		return 2
	}
	saved := loadSavedConfig()
	saved.APIURL = cfg.apiURL
	if cfg.apiKey != "" {
		saved.APIKey = cfg.apiKey
	}
	if cfg.token != "" {
		saved.Token = cfg.token
	}
	if err := saveConfig(saved); err != nil {
		return errExit("login", err)
	}
	fmt.Printf("Saved config to %s\n", configPath())

	c, err := cfg.client()
	if err != nil {
		return errExit("login", err)
	}
	who, err := c.GetCurrentUser(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttier: saved config but auth check failed: %v\n", err)
		return 1
	}
	label := who.Email
	if label == "" {
		label = who.Name
	}
	if label == "" {
		label = who.Sub
	}
	fmt.Printf("Authenticated as: %s\n", label)
	return 0
}
