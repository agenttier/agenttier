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
	"testing"

	"github.com/agenttier/agenttier/pkg/router/auth"
)

func TestClaimsFromAuth_CopiesScopedFields(t *testing.T) {
	ac := &auth.Claims{
		Sub:          "u-1",
		Email:        "u@example.com",
		Name:         "U",
		IsAdmin:      false,
		SandboxID:    "sbx-1",
		ActionGroups: []string{"run-command", "files:read"},
	}

	rc := claimsFromAuth(ac)
	if rc.SandboxID != "sbx-1" {
		t.Errorf("SandboxID = %q, want sbx-1", rc.SandboxID)
	}
	if len(rc.ActionGroups) != 2 || rc.ActionGroups[0] != "run-command" || rc.ActionGroups[1] != "files:read" {
		t.Errorf("ActionGroups = %v, want [run-command files:read]", rc.ActionGroups)
	}
}

func TestClaimsFromAuth_UserLevelLeavesScopeFieldsEmpty(t *testing.T) {
	ac := &auth.Claims{Sub: "u-1", IsAdmin: true}

	rc := claimsFromAuth(ac)
	if rc.SandboxID != "" {
		t.Errorf("expected empty SandboxID for a user-level key, got %q", rc.SandboxID)
	}
	if len(rc.ActionGroups) != 0 {
		t.Errorf("expected no ActionGroups for a user-level key, got %v", rc.ActionGroups)
	}
}

func TestClaimsFromAuth_NilInputReturnsNil(t *testing.T) {
	if got := claimsFromAuth(nil); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}
