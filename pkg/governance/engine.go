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
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

// Violation describes a single policy check that failed, with a stable machine
// code and a human-readable message. The Router surfaces the code to the UI so
// it can render a friendly error next to the right form field.
type Violation struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error implements error.
func (v Violation) Error() string { return v.Message }

// Violations is a slice of policy violations.
type Violations []Violation

// Error joins all violations with "; ".
func (vs Violations) Error() string {
	if len(vs) == 0 {
		return ""
	}
	msgs := make([]string, len(vs))
	for i, v := range vs {
		msgs[i] = v.Message
	}
	return strings.Join(msgs, "; ")
}

// Violated reports whether there are any violations.
func (vs Violations) Violated() bool { return len(vs) > 0 }

// Check runs every applicable policy rule against a proposed sandbox create.
//
// `policy` is the effective namespace-resolved policy (see Resolve).
// `usage` is the current consumption snapshot for the scope.
func Check(policy Policy, usage Usage, sandbox *agenttierv1alpha1.Sandbox) Violations {
	var out Violations

	// Template restrictions apply first — if the template isn't allowed, no
	// other checks matter.
	if len(policy.AllowedTemplates) > 0 && sandbox.Spec.TemplateRef != nil {
		if !contains(policy.AllowedTemplates, sandbox.Spec.TemplateRef.Name) {
			out = append(out, Violation{
				Code:    "template_not_allowed",
				Message: fmt.Sprintf("template %q is not permitted in this namespace (allowed: %s)", sandbox.Spec.TemplateRef.Name, strings.Join(policy.AllowedTemplates, ", ")),
			})
		}
	}

	// Image registry allowlist — only applies when the sandbox overrides the
	// template image. The template's own image is trusted.
	if len(policy.ApprovedRegistries) > 0 && sandbox.Spec.Image != nil && sandbox.Spec.Image.Repository != "" {
		if !hasRegistryPrefix(sandbox.Spec.Image.Repository, policy.ApprovedRegistries) {
			out = append(out, Violation{
				Code:    "image_registry_not_approved",
				Message: fmt.Sprintf("image %q is not in an approved registry (%s)", sandbox.Spec.Image.Repository, strings.Join(policy.ApprovedRegistries, ", ")),
			})
		}
	}

	// Quota checks.
	if policy.MaxSandboxesTotal > 0 && usage.TotalSandboxes >= policy.MaxSandboxesTotal {
		out = append(out, Violation{
			Code:    "namespace_quota_exceeded",
			Message: fmt.Sprintf("namespace already has %d sandboxes (max %d)", usage.TotalSandboxes, policy.MaxSandboxesTotal),
		})
	}
	if policy.MaxSandboxesPerUser > 0 && usage.UserSandboxes >= policy.MaxSandboxesPerUser {
		out = append(out, Violation{
			Code:    "user_quota_exceeded",
			Message: fmt.Sprintf("user already owns %d sandboxes in this namespace (max %d)", usage.UserSandboxes, policy.MaxSandboxesPerUser),
		})
	}

	// Agent-mode quota: only relevant when this sandbox is itself agent-mode.
	if sandbox.Spec.Mode == agenttierv1alpha1.SandboxModeAgent &&
		policy.MaxAgentSandboxes > 0 &&
		usage.AgentSandboxes >= policy.MaxAgentSandboxes {
		out = append(out, Violation{
			Code:    "agent_sandbox_quota_exceeded",
			Message: fmt.Sprintf("namespace already has %d agent-mode sandboxes (max %d)", usage.AgentSandboxes, policy.MaxAgentSandboxes),
		})
	}

	// Agent image allowlist: a separate, typically tighter list than
	// ApprovedRegistries because agent code has more freedom inside the
	// sandbox than an interactive developer environment. Only enforced
	// when the sandbox is mode: agent AND has an image override (template
	// images are trusted).
	if sandbox.Spec.Mode == agenttierv1alpha1.SandboxModeAgent &&
		len(policy.AllowedAgentImages) > 0 &&
		sandbox.Spec.Image != nil && sandbox.Spec.Image.Repository != "" {
		if !hasRegistryPrefix(sandbox.Spec.Image.Repository, policy.AllowedAgentImages) {
			out = append(out, Violation{
				Code:    "agent_image_not_approved",
				Message: fmt.Sprintf("agent image %q is not in the approved-agent-images list (%s)", sandbox.Spec.Image.Repository, strings.Join(policy.AllowedAgentImages, ", ")),
			})
		}
	}

	// Resource/timeout value caps — factored into CheckResourceLimits so
	// callers that should never evaluate the sandbox-COUNT quotas above (e.g.
	// PATCH, which creates no new sandbox) can run just this subset.
	out = append(out, CheckResourceLimits(policy, sandbox)...)

	return out
}

// CheckResourceLimits runs only the per-field VALUE-limit checks
// (maxCpu/maxMemory/maxStorage/maxTimeout/maxIdleTimeout) against a proposed
// sandbox spec — never the sandbox-COUNT quotas (MaxSandboxesTotal/
// MaxSandboxesPerUser/MaxAgentSandboxes) or the template/registry allowlists,
// which only make sense when a *new* sandbox is being created.
//
// Check calls this internally for the create path, which also needs the
// count-quota and allowlist checks. Callers that re-validate an existing
// sandbox's fields without creating a new one — PATCH being the motivating
// case (FR2.4/NFR3) — must call this instead of Check, since Check's count
// checks would spuriously reject a value-only change once a namespace is
// already at its sandbox-count cap.
func CheckResourceLimits(policy Policy, sandbox *agenttierv1alpha1.Sandbox) Violations {
	var out Violations

	// Resource caps — only check sandbox overrides. The template's own
	// resource requests are validated at template-creation time (future).
	if policy.MaxCPU != "" && sandbox.Spec.Resources != nil {
		if cpu, ok := sandbox.Spec.Resources.Limits["cpu"]; ok {
			if exceedsQuantity(cpu, policy.MaxCPU) {
				out = append(out, Violation{
					Code:    "cpu_limit_exceeded",
					Message: fmt.Sprintf("cpu limit %s exceeds governance cap %s", cpu.String(), policy.MaxCPU),
				})
			}
		}
	}
	if policy.MaxMemory != "" && sandbox.Spec.Resources != nil {
		if mem, ok := sandbox.Spec.Resources.Limits["memory"]; ok {
			if exceedsQuantity(mem, policy.MaxMemory) {
				out = append(out, Violation{
					Code:    "memory_limit_exceeded",
					Message: fmt.Sprintf("memory limit %s exceeds governance cap %s", mem.String(), policy.MaxMemory),
				})
			}
		}
	}
	if policy.MaxStorage != "" && sandbox.Spec.Storage != nil && !sandbox.Spec.Storage.Size.IsZero() {
		if exceedsQuantity(sandbox.Spec.Storage.Size, policy.MaxStorage) {
			out = append(out, Violation{
				Code:    "storage_limit_exceeded",
				Message: fmt.Sprintf("storage size %s exceeds governance cap %s", sandbox.Spec.Storage.Size.String(), policy.MaxStorage),
			})
		}
	}

	// Timeout caps.
	if policy.MaxTimeout != "" && sandbox.Spec.Timeout != nil {
		if exceedsDuration(sandbox.Spec.Timeout.Duration, policy.MaxTimeout) {
			out = append(out, Violation{
				Code:    "timeout_exceeded",
				Message: fmt.Sprintf("timeout %s exceeds governance cap %s", sandbox.Spec.Timeout.Duration, policy.MaxTimeout),
			})
		}
	}
	if policy.MaxIdleTimeout != "" && sandbox.Spec.IdleTimeout != nil {
		if exceedsDuration(sandbox.Spec.IdleTimeout.Duration, policy.MaxIdleTimeout) {
			out = append(out, Violation{
				Code:    "idle_timeout_exceeded",
				Message: fmt.Sprintf("idleTimeout %s exceeds governance cap %s", sandbox.Spec.IdleTimeout.Duration, policy.MaxIdleTimeout),
			})
		}
	}

	return out
}

// CheckBulk evaluates the aggregate sandbox-count caps for a proposed batch
// create of n sandboxes, agentN of which are mode: agent. Unlike Check (which
// validates a single sandbox against policy), CheckBulk is a whole-batch
// fail-fast gate (DD4): it does not evaluate per-item concerns like template
// allowlists or image registries — those remain per-item checks via Check.
// Callers resolve the effective policy and count current usage once up
// front, then reject the entire batch here before creating anything.
func CheckBulk(policy Policy, usage Usage, n int, agentN int) Violations {
	var out Violations

	if policy.MaxSandboxesTotal > 0 && usage.TotalSandboxes+n > policy.MaxSandboxesTotal {
		out = append(out, Violation{
			Code:    "namespace_quota_exceeded",
			Message: fmt.Sprintf("namespace has %d sandboxes; creating %d more would exceed the max of %d", usage.TotalSandboxes, n, policy.MaxSandboxesTotal),
		})
	}
	if policy.MaxSandboxesPerUser > 0 && usage.UserSandboxes+n > policy.MaxSandboxesPerUser {
		out = append(out, Violation{
			Code:    "user_quota_exceeded",
			Message: fmt.Sprintf("user owns %d sandboxes in this namespace; creating %d more would exceed the max of %d", usage.UserSandboxes, n, policy.MaxSandboxesPerUser),
		})
	}
	if policy.MaxAgentSandboxes > 0 && agentN > 0 && usage.AgentSandboxes+agentN > policy.MaxAgentSandboxes {
		out = append(out, Violation{
			Code:    "agent_sandbox_quota_exceeded",
			Message: fmt.Sprintf("namespace has %d agent-mode sandboxes; creating %d more would exceed the max of %d", usage.AgentSandboxes, agentN, policy.MaxAgentSandboxes),
		})
	}

	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func hasRegistryPrefix(image string, registries []string) bool {
	for _, prefix := range registries {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(image, prefix) {
			return true
		}
	}
	return false
}

func exceedsQuantity(got resource.Quantity, capStr string) bool {
	capQ, err := resource.ParseQuantity(capStr)
	if err != nil {
		// Misconfigured cap — fail open rather than wedge every create.
		return false
	}
	return got.Cmp(capQ) > 0
}

func exceedsDuration(got time.Duration, capStr string) bool {
	// `0` means "infinite" on the sandbox side, which always exceeds any cap.
	cap, err := time.ParseDuration(capStr)
	if err != nil {
		return false
	}
	if got == 0 {
		return true
	}
	return got > cap
}

// ClampConcurrency returns the effective per-sandbox max concurrent invokes,
// applying the policy's MaxConcurrentInvokesPerSandbox as a cluster ceiling
// over the sandbox-or-template-supplied value. Both arguments are 0 = no
// limit; the result follows the same convention.
//
// The policy ceiling wins when the requested value exceeds it. When the
// policy is unset, the requested value passes through unchanged.
func ClampConcurrency(policy Policy, requested int32) int32 {
	if policy.MaxConcurrentInvokesPerSandbox <= 0 {
		return requested
	}
	ceiling := int32(policy.MaxConcurrentInvokesPerSandbox)
	if requested == 0 || requested > ceiling {
		return ceiling
	}
	return requested
}
