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

package governance

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

func sandboxWith(mods func(s *agenttierv1alpha1.Sandbox)) *agenttierv1alpha1.Sandbox {
	s := &agenttierv1alpha1.Sandbox{
		Spec: agenttierv1alpha1.SandboxSpec{
			TemplateRef: &agenttierv1alpha1.TemplateReference{Name: "general-coding"},
			CreatedBy:   &agenttierv1alpha1.UserIdentity{Sub: "user-1"},
		},
	}
	if mods != nil {
		mods(s)
	}
	return s
}

func TestCheck_NoLimits(t *testing.T) {
	v := Check(Policy{}, Usage{}, sandboxWith(nil))
	if v.Violated() {
		t.Fatalf("expected no violations, got %v", v)
	}
}

func TestCheck_TemplateAllowlist(t *testing.T) {
	policy := Policy{AllowedTemplates: []string{"claude-code-bedrock"}}

	ok := Check(policy, Usage{}, sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.TemplateRef.Name = "claude-code-bedrock"
	}))
	if ok.Violated() {
		t.Fatalf("allowed template should pass: %v", ok)
	}

	bad := Check(policy, Usage{}, sandboxWith(nil)) // template = general-coding
	if !bad.Violated() {
		t.Fatal("disallowed template should violate")
	}
	if bad[0].Code != "template_not_allowed" {
		t.Fatalf("expected template_not_allowed, got %s", bad[0].Code)
	}
}

func TestCheck_UserQuotaExceeded(t *testing.T) {
	policy := Policy{MaxSandboxesPerUser: 2}

	// Under quota.
	v := Check(policy, Usage{UserSandboxes: 1}, sandboxWith(nil))
	if v.Violated() {
		t.Fatalf("under quota should pass: %v", v)
	}

	// At quota — creating one more would exceed.
	v = Check(policy, Usage{UserSandboxes: 2}, sandboxWith(nil))
	if !v.Violated() || v[0].Code != "user_quota_exceeded" {
		t.Fatalf("at quota should violate user_quota_exceeded, got %v", v)
	}
}

func TestCheck_NamespaceQuotaExceeded(t *testing.T) {
	policy := Policy{MaxSandboxesTotal: 5}
	v := Check(policy, Usage{TotalSandboxes: 5}, sandboxWith(nil))
	if !v.Violated() || v[0].Code != "namespace_quota_exceeded" {
		t.Fatalf("expected namespace_quota_exceeded, got %v", v)
	}
}

func TestCheck_ResourceCaps(t *testing.T) {
	policy := Policy{MaxCPU: "2", MaxMemory: "4Gi", MaxStorage: "20Gi"}

	// All within limits.
	s := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Resources = &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"cpu":    resource.MustParse("1"),
				"memory": resource.MustParse("2Gi"),
			},
		}
		s.Spec.Storage = &agenttierv1alpha1.StorageSpec{Size: resource.MustParse("10Gi")}
	})
	if v := Check(policy, Usage{}, s); v.Violated() {
		t.Fatalf("within-limits sandbox should pass: %v", v)
	}

	// CPU over.
	s = sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Resources = &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"cpu": resource.MustParse("4")},
		}
	})
	v := Check(policy, Usage{}, s)
	if !v.Violated() || v[0].Code != "cpu_limit_exceeded" {
		t.Fatalf("expected cpu_limit_exceeded, got %v", v)
	}

	// Storage over.
	s = sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Storage = &agenttierv1alpha1.StorageSpec{Size: resource.MustParse("50Gi")}
	})
	v = Check(policy, Usage{}, s)
	if !v.Violated() || v[0].Code != "storage_limit_exceeded" {
		t.Fatalf("expected storage_limit_exceeded, got %v", v)
	}
}

func TestCheck_TimeoutCap(t *testing.T) {
	policy := Policy{MaxTimeout: "4h", MaxIdleTimeout: "1h"}

	// Sandbox well within limits.
	s := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Timeout = &metav1.Duration{Duration: 2 * time.Hour}
		s.Spec.IdleTimeout = &metav1.Duration{Duration: 30 * time.Minute}
	})
	if v := Check(policy, Usage{}, s); v.Violated() {
		t.Fatalf("within timeout caps should pass: %v", v)
	}

	// Timeout too long.
	s = sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Timeout = &metav1.Duration{Duration: 24 * time.Hour}
	})
	v := Check(policy, Usage{}, s)
	if !v.Violated() || v[0].Code != "timeout_exceeded" {
		t.Fatalf("expected timeout_exceeded, got %v", v)
	}

	// "Infinite" (0) also violates a cap.
	s = sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Timeout = &metav1.Duration{Duration: 0}
	})
	v = Check(policy, Usage{}, s)
	if !v.Violated() || v[0].Code != "timeout_exceeded" {
		t.Fatalf("expected timeout_exceeded for 0 (infinite), got %v", v)
	}
}

func TestCheck_RegistryAllowlist(t *testing.T) {
	policy := Policy{ApprovedRegistries: []string{"ghcr.io/agenttier"}}

	// No sandbox image override — trusts template.
	if v := Check(policy, Usage{}, sandboxWith(nil)); v.Violated() {
		t.Fatalf("no image override should pass: %v", v)
	}

	// Approved override.
	s := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Image = &agenttierv1alpha1.ImageSpec{Repository: "ghcr.io/agenttier/sandbox-custom:latest"}
	})
	if v := Check(policy, Usage{}, s); v.Violated() {
		t.Fatalf("approved image should pass: %v", v)
	}

	// Disallowed override.
	s = sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Image = &agenttierv1alpha1.ImageSpec{Repository: "docker.io/random/image:latest"}
	})
	v := Check(policy, Usage{}, s)
	if !v.Violated() || v[0].Code != "image_registry_not_approved" {
		t.Fatalf("expected image_registry_not_approved, got %v", v)
	}
}

func TestMergePolicies(t *testing.T) {
	cluster := Policy{
		MaxCPU:              "4",
		MaxMemory:           "8Gi",
		MaxSandboxesPerUser: 5,
		AllowedTemplates:    []string{"a", "b"},
	}
	ns := Policy{
		MaxCPU:              "2",           // override
		MaxSandboxesPerUser: 0,             // don't override (zero means no limit)
		AllowedTemplates:    []string{"c"}, // override
	}
	merged := mergePolicies(cluster, ns)

	if merged.MaxCPU != "2" {
		t.Errorf("expected MaxCPU overridden to 2, got %s", merged.MaxCPU)
	}
	if merged.MaxMemory != "8Gi" {
		t.Errorf("expected MaxMemory inherited as 8Gi, got %s", merged.MaxMemory)
	}
	if merged.MaxSandboxesPerUser != 5 {
		t.Errorf("expected MaxSandboxesPerUser inherited as 5, got %d", merged.MaxSandboxesPerUser)
	}
	if len(merged.AllowedTemplates) != 1 || merged.AllowedTemplates[0] != "c" {
		t.Errorf("expected AllowedTemplates overridden to [c], got %v", merged.AllowedTemplates)
	}
}

func TestCountUsage(t *testing.T) {
	list := &agenttierv1alpha1.SandboxList{
		Items: []agenttierv1alpha1.Sandbox{
			{
				Spec: agenttierv1alpha1.SandboxSpec{
					CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u1"},
				},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
			},
			{
				Spec: agenttierv1alpha1.SandboxSpec{
					CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u1"},
				},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseStopped},
			},
			{
				Spec: agenttierv1alpha1.SandboxSpec{
					CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u2"},
				},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
			},
			{
				// Error state — should not count.
				Spec: agenttierv1alpha1.SandboxSpec{
					CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u1"},
				},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseError},
			},
		},
	}

	got := CountUsage(list, "u1")
	if got.TotalSandboxes != 3 {
		t.Errorf("expected 3 total (error excluded), got %d", got.TotalSandboxes)
	}
	if got.UserSandboxes != 2 {
		t.Errorf("expected 2 for u1, got %d", got.UserSandboxes)
	}
}

func TestCheck_AgentSandboxQuota(t *testing.T) {
	policy := Policy{MaxAgentSandboxes: 2}
	usage := Usage{TotalSandboxes: 10, AgentSandboxes: 2}
	sb := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Mode = agenttierv1alpha1.SandboxModeAgent
	})

	violations := Check(policy, usage, sb)
	if !violations.Violated() {
		t.Fatal("expected agent_sandbox_quota_exceeded violation")
	}
	if violations[0].Code != "agent_sandbox_quota_exceeded" {
		t.Errorf("expected agent_sandbox_quota_exceeded, got %s", violations[0].Code)
	}
}

func TestCheck_AgentSandboxQuotaIgnoresCodeMode(t *testing.T) {
	policy := Policy{MaxAgentSandboxes: 2}
	usage := Usage{AgentSandboxes: 2} // already at the cap
	sb := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Mode = agenttierv1alpha1.SandboxModeCode
	})

	if Check(policy, usage, sb).Violated() {
		t.Error("MaxAgentSandboxes should not block a new code-mode sandbox")
	}
}

// TestCheckResourceLimits_IgnoresCountQuotas is the review-2 regression test
// at the governance-engine level: CheckResourceLimits must never evaluate
// the sandbox-COUNT quotas (MaxSandboxesTotal/MaxSandboxesPerUser/
// MaxAgentSandboxes) or the template/registry allowlists — only the
// maxCpu/maxMemory/maxStorage/maxTimeout/maxIdleTimeout value caps. A
// namespace already AT every count cap must still pass a value-only check.
func TestCheckResourceLimits_IgnoresCountQuotas(t *testing.T) {
	policy := Policy{
		MaxSandboxesTotal:   1,
		MaxSandboxesPerUser: 1,
		MaxAgentSandboxes:   1,
		AllowedTemplates:    []string{"claude-code-bedrock"}, // sandbox uses "general-coding" — would violate under Check
		MaxIdleTimeout:      "1h",
	}
	sb := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.IdleTimeout = &metav1.Duration{Duration: 30 * time.Minute}
	})

	if v := CheckResourceLimits(policy, sb); v.Violated() {
		t.Fatalf("CheckResourceLimits must ignore count quotas and allowlists, got %v", v)
	}

	// Sanity: Check (the create-path validator) DOES still reject this same
	// sandbox+policy on the template allowlist — proving CheckResourceLimits
	// is a strict subset, not a differently-behaving duplicate.
	if v := Check(policy, Usage{}, sb); !v.Violated() {
		t.Fatal("expected Check to still reject the disallowed template (sanity check)")
	}
}

// TestCheckResourceLimits_StillCatchesValueCapViolations confirms the
// extraction didn't drop enforcement of the actual value caps it owns.
func TestCheckResourceLimits_StillCatchesValueCapViolations(t *testing.T) {
	policy := Policy{MaxCPU: "1"}
	sb := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Resources = &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"cpu": resource.MustParse("4")},
		}
	})
	v := CheckResourceLimits(policy, sb)
	if !v.Violated() || v[0].Code != "cpu_limit_exceeded" {
		t.Fatalf("expected cpu_limit_exceeded, got %v", v)
	}
}

func TestCheck_AllowedAgentImages(t *testing.T) {
	policy := Policy{AllowedAgentImages: []string{"ghcr.io/agenttier/sandbox-langgraph"}}
	sb := sandboxWith(func(s *agenttierv1alpha1.Sandbox) {
		s.Spec.Mode = agenttierv1alpha1.SandboxModeAgent
		s.Spec.Image = &agenttierv1alpha1.ImageSpec{Repository: "ghcr.io/random/agent:latest"}
	})

	violations := Check(policy, Usage{}, sb)
	if !violations.Violated() {
		t.Fatal("expected agent_image_not_approved violation")
	}
	if violations[0].Code != "agent_image_not_approved" {
		t.Errorf("expected agent_image_not_approved, got %s", violations[0].Code)
	}
}

func TestClampConcurrency(t *testing.T) {
	cases := []struct {
		name      string
		policy    Policy
		requested int32
		want      int32
	}{
		{"unset policy passes through", Policy{}, 5, 5},
		{"unset request keeps unlimited", Policy{}, 0, 0},
		{"under ceiling passes through", Policy{MaxConcurrentInvokesPerSandbox: 8}, 3, 3},
		{"over ceiling clamps", Policy{MaxConcurrentInvokesPerSandbox: 4}, 10, 4},
		{"unset request clamped to ceiling", Policy{MaxConcurrentInvokesPerSandbox: 4}, 0, 4},
		{"equal to ceiling unchanged", Policy{MaxConcurrentInvokesPerSandbox: 4}, 4, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampConcurrency(tc.policy, tc.requested)
			if got != tc.want {
				t.Errorf("ClampConcurrency(%+v, %d) = %d, want %d", tc.policy, tc.requested, got, tc.want)
			}
		})
	}
}

func TestCountUsage_AgentSandboxes(t *testing.T) {
	list := &agenttierv1alpha1.SandboxList{
		Items: []agenttierv1alpha1.Sandbox{
			{
				Spec:   agenttierv1alpha1.SandboxSpec{Mode: agenttierv1alpha1.SandboxModeAgent, CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u1"}},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
			},
			{
				Spec:   agenttierv1alpha1.SandboxSpec{Mode: agenttierv1alpha1.SandboxModeAgent},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseStopped},
			},
			{
				Spec:   agenttierv1alpha1.SandboxSpec{Mode: agenttierv1alpha1.SandboxModeCode},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
			},
			{
				// Error phase doesn't count.
				Spec:   agenttierv1alpha1.SandboxSpec{Mode: agenttierv1alpha1.SandboxModeAgent},
				Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseError},
			},
		},
	}
	u := CountUsage(list, "u1")
	if u.AgentSandboxes != 2 {
		t.Errorf("expected 2 agent sandboxes (Running + Stopped), got %d", u.AgentSandboxes)
	}
	if u.TotalSandboxes != 3 {
		t.Errorf("expected 3 total sandboxes (Error excluded), got %d", u.TotalSandboxes)
	}
	if u.UserSandboxes != 1 {
		t.Errorf("expected 1 user sandbox for u1, got %d", u.UserSandboxes)
	}
}

func TestCheckBulk(t *testing.T) {
	cases := []struct {
		name      string
		policy    Policy
		usage     Usage
		n         int
		agentN    int
		wantCodes []string
	}{
		{
			name:   "no limits passes",
			policy: Policy{},
			usage:  Usage{TotalSandboxes: 100, UserSandboxes: 50},
			n:      10,
		},
		{
			name:   "under total cap passes",
			policy: Policy{MaxSandboxesTotal: 20},
			usage:  Usage{TotalSandboxes: 5},
			n:      10,
		},
		{
			name:      "at total cap boundary rejects",
			policy:    Policy{MaxSandboxesTotal: 10},
			usage:     Usage{TotalSandboxes: 5},
			n:         5,
			wantCodes: nil, // 5+5==10, not > 10 — should pass
		},
		{
			name:      "over total cap rejects",
			policy:    Policy{MaxSandboxesTotal: 10},
			usage:     Usage{TotalSandboxes: 5},
			n:         6,
			wantCodes: []string{"namespace_quota_exceeded"},
		},
		{
			name:      "over per-user cap rejects",
			policy:    Policy{MaxSandboxesPerUser: 5},
			usage:     Usage{UserSandboxes: 3},
			n:         3,
			wantCodes: []string{"user_quota_exceeded"},
		},
		{
			name:      "over agent cap rejects",
			policy:    Policy{MaxAgentSandboxes: 2},
			usage:     Usage{AgentSandboxes: 1},
			n:         3,
			agentN:    2,
			wantCodes: []string{"agent_sandbox_quota_exceeded"},
		},
		{
			name:   "agent cap ignored when batch has no agent sandboxes",
			policy: Policy{MaxAgentSandboxes: 2},
			usage:  Usage{AgentSandboxes: 2},
			n:      3,
			agentN: 0,
		},
		{
			name:      "multiple caps exceeded reports all",
			policy:    Policy{MaxSandboxesTotal: 10, MaxSandboxesPerUser: 5},
			usage:     Usage{TotalSandboxes: 8, UserSandboxes: 4},
			n:         5,
			wantCodes: []string{"namespace_quota_exceeded", "user_quota_exceeded"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckBulk(tc.policy, tc.usage, tc.n, tc.agentN)
			if len(got) != len(tc.wantCodes) {
				t.Fatalf("CheckBulk() = %v, want codes %v", got, tc.wantCodes)
			}
			for i, code := range tc.wantCodes {
				if got[i].Code != code {
					t.Errorf("violation[%d].Code = %q, want %q", i, got[i].Code, code)
				}
			}
		})
	}
}

// Silence unused imports when only the new tests reference them.
var (
	_ = corev1.PodSpec{}
	_ = resource.Quantity{}
	_ = metav1.Time{}
	_ = time.Now
)
