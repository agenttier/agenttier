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
	"net/url"
)

// Governance policy CRUD, audit-event listing, and usage/cost analytics.
// Wire shapes mirror pkg/router/handlers.go's handleListPolicies /
// handleGetPolicy / handleUpsertClusterPolicy / handleSetPolicy /
// handleDeletePolicy / handleGetEffectivePolicy / handleListAuditEvents /
// handleGetUsageAnalytics / handleGetCostEstimates, and
// handleAdminListSandboxes / handleAdminListSharing.

// --- governance ------------------------------------------------------------

// Policy mirrors pkg/governance.Policy's JSON shape. Zero/empty fields
// mean "no limit" on the Router side.
type Policy struct {
	MaxSandboxesPerUser            int      `json:"maxSandboxesPerUser,omitempty"`
	MaxSandboxesTotal              int      `json:"maxSandboxesTotal,omitempty"`
	MaxCPU                         string   `json:"maxCpu,omitempty"`
	MaxMemory                      string   `json:"maxMemory,omitempty"`
	MaxStorage                     string   `json:"maxStorage,omitempty"`
	MaxTimeout                     string   `json:"maxTimeout,omitempty"`
	MaxIdleTimeout                 string   `json:"maxIdleTimeout,omitempty"`
	AllowedTemplates               []string `json:"allowedTemplates,omitempty"`
	ApprovedRegistries             []string `json:"approvedRegistries,omitempty"`
	MaxAgentSandboxes              int      `json:"maxAgentSandboxes,omitempty"`
	AllowedAgentImages             []string `json:"allowedAgentImages,omitempty"`
	MaxConcurrentInvokesPerSandbox int      `json:"maxConcurrentInvokesPerSandbox,omitempty"`
	Description                    string   `json:"description,omitempty"`
}

// NamespacePolicy pairs a namespace with its policy, as returned by
// ListPolicies.
type NamespacePolicy struct {
	Namespace string `json:"namespace"`
	Policy    Policy `json:"policy"`
}

// ListPoliciesResult is the response of GET /governance/policies.
type ListPoliciesResult struct {
	Cluster    *Policy           `json:"cluster"`
	Namespaces []NamespacePolicy `json:"namespaces"`
}

// ListPolicies issues GET /governance/policies.
func (c *Client) ListPolicies(ctx context.Context) (*ListPoliciesResult, error) {
	var out ListPoliciesResult
	if err := c.Get(ctx, "/governance/policies", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetClusterPolicy issues PUT /governance/policies (admin-only; the
// cluster-wide default policy).
func (c *Client) SetClusterPolicy(ctx context.Context, policy Policy) (*Policy, error) {
	var out struct {
		Scope  string `json:"scope"`
		Policy Policy `json:"policy"`
	}
	if err := c.Put(ctx, "/governance/policies", policy, &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

// GetPolicy issues GET /governance/policies/{namespace}.
func (c *Client) GetPolicy(ctx context.Context, namespace string) (*Policy, error) {
	var out struct {
		Namespace string `json:"namespace"`
		Policy    Policy `json:"policy"`
	}
	if err := c.Get(ctx, "/governance/policies/"+url.PathEscape(namespace), &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

// SetPolicy issues PUT /governance/policies/{namespace} (admin-only).
func (c *Client) SetPolicy(ctx context.Context, namespace string, policy Policy) (*Policy, error) {
	var out struct {
		Namespace string `json:"namespace"`
		Policy    Policy `json:"policy"`
	}
	if err := c.Put(ctx, "/governance/policies/"+url.PathEscape(namespace), policy, &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

// DeletePolicy issues DELETE /governance/policies/{namespace} (admin-only).
func (c *Client) DeletePolicy(ctx context.Context, namespace string) error {
	return c.Delete(ctx, "/governance/policies/"+url.PathEscape(namespace), nil)
}

// GetEffectivePolicy issues GET /governance/effective?namespace=<ns>.
func (c *Client) GetEffectivePolicy(ctx context.Context, namespace string) (*Policy, error) {
	path := "/governance/effective"
	if namespace != "" {
		path += "?" + url.Values{"namespace": {namespace}}.Encode()
	}
	var out struct {
		Namespace string `json:"namespace"`
		Policy    Policy `json:"policy"`
	}
	if err := c.Get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

// --- audit -----------------------------------------------------------------

// AuditEvent is one entry from GET /audit/events.
type AuditEvent struct {
	Timestamp   string            `json:"timestamp,omitempty"`
	EventType   string            `json:"eventType,omitempty"`
	SandboxID   string            `json:"sandboxId,omitempty"`
	SandboxName string            `json:"sandboxName,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	UserEmail   string            `json:"userEmail,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
}

// ListAuditEvents issues GET /audit/events (admin-only).
func (c *Client) ListAuditEvents(ctx context.Context) ([]AuditEvent, error) {
	var out struct {
		Events []AuditEvent `json:"events"`
	}
	if err := c.Get(ctx, "/audit/events", &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// --- analytics ---------------------------------------------------------

// UsageAnalytics is the response of GET /analytics/usage (admin-only).
type UsageAnalytics struct {
	TotalSandboxes     int            `json:"total_sandboxes"`
	StatusBreakdown    map[string]int `json:"status_breakdown"`
	TemplateBreakdown  map[string]int `json:"template_breakdown"`
	AvgStartupMs       int64          `json:"avg_startup_ms"`
	StartupSampleCount int            `json:"startup_sample_count"`
}

// GetUsageAnalytics issues GET /analytics/usage (admin-only).
func (c *Client) GetUsageAnalytics(ctx context.Context) (*UsageAnalytics, error) {
	var out UsageAnalytics
	if err := c.Get(ctx, "/analytics/usage", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TemplateCost is one entry of CostEstimates.PerTemplate.
type TemplateCost struct {
	Template   string  `json:"template"`
	HourlyCost float64 `json:"hourly_cost"`
	Count      int     `json:"count"`
}

// CostEstimates is the response of GET /analytics/costs (admin-only).
type CostEstimates struct {
	RunningSandboxes      int            `json:"running_sandboxes"`
	StoppedSandboxes      int            `json:"stopped_sandboxes"`
	TotalHourlyCompute    float64        `json:"total_hourly_compute"`
	TotalHourlyStorage    float64        `json:"total_hourly_storage"`
	TotalEstimatedMonthly float64        `json:"total_estimated_monthly"`
	PerTemplate           []TemplateCost `json:"per_template"`
}

// GetCostEstimates issues GET /analytics/costs (admin-only).
func (c *Client) GetCostEstimates(ctx context.Context) (*CostEstimates, error) {
	var out CostEstimates
	if err := c.Get(ctx, "/analytics/costs", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- admin -----------------------------------------------------------------

// AdminListSandboxes issues GET /admin/sandboxes (admin-only). Returns the
// raw decoded response — the handler's shape is intentionally open-ended
// (design.md's FR6.6 admin-visibility extension adds scoped-key metadata
// here once Group 2 lands) so callers get the full payload rather than a
// narrowed struct that would silently drop new fields.
func (c *Client) AdminListSandboxes(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.Get(ctx, "/admin/sandboxes", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AdminListSharing issues GET /admin/sharing (admin-only).
func (c *Client) AdminListSharing(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.Get(ctx, "/admin/sharing", &out); err != nil {
		return nil, err
	}
	return out, nil
}
