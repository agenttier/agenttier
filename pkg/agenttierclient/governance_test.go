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
	"net/http"
	"testing"
)

func TestListPolicies(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cluster":    map[string]any{"maxSandboxesTotal": 100},
			"namespaces": []map[string]any{{"namespace": "team-a", "policy": map[string]any{"maxSandboxesPerUser": 5}}},
		})
	})
	out, err := c.ListPolicies(context.Background())
	if err != nil {
		t.Fatalf("ListPolicies() error = %v", err)
	}
	if out.Cluster == nil || out.Cluster.MaxSandboxesTotal != 100 {
		t.Fatalf("Cluster = %+v", out.Cluster)
	}
	if len(out.Namespaces) != 1 || out.Namespaces[0].Namespace != "team-a" {
		t.Fatalf("Namespaces = %+v", out.Namespaces)
	}
}

func TestGetSetDeletePolicy(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody Policy
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if r.Method == http.MethodPut {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "team-a", "policy": map[string]any{"maxCpu": "4"}})
		case http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "team-a", "policy": map[string]any{"maxCpu": "8"}})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	pol, err := c.GetPolicy(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	if pol.MaxCPU != "4" {
		t.Fatalf("pol = %+v", pol)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/governance/policies/team-a" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}

	updated, err := c.SetPolicy(context.Background(), "team-a", Policy{MaxCPU: "8"})
	if err != nil {
		t.Fatalf("SetPolicy() error = %v", err)
	}
	if updated.MaxCPU != "8" {
		t.Fatalf("updated = %+v", updated)
	}
	if gotBody.MaxCPU != "8" {
		t.Errorf("gotBody = %+v", gotBody)
	}

	if err := c.DeletePolicy(context.Background(), "team-a"); err != nil {
		t.Fatalf("DeletePolicy() error = %v", err)
	}
}

func TestSetClusterPolicy(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/governance/policies" {
			t.Errorf("path = %s, want /api/v1/governance/policies", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"scope": "cluster", "policy": map[string]any{"maxSandboxesTotal": 50}})
	})
	pol, err := c.SetClusterPolicy(context.Background(), Policy{MaxSandboxesTotal: 50})
	if err != nil {
		t.Fatalf("SetClusterPolicy() error = %v", err)
	}
	if pol.MaxSandboxesTotal != 50 {
		t.Fatalf("pol = %+v", pol)
	}
}

func TestGetEffectivePolicy(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "team-a", "policy": map[string]any{"maxMemory": "8Gi"}})
	})
	pol, err := c.GetEffectivePolicy(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetEffectivePolicy() error = %v", err)
	}
	if gotQuery != "namespace=team-a" {
		t.Errorf("query = %q, want namespace=team-a", gotQuery)
	}
	if pol.MaxMemory != "8Gi" {
		t.Fatalf("pol = %+v", pol)
	}
}

func TestListAuditEvents(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{{"eventType": "Created", "sandboxId": "demo"}},
		})
	})
	events, err := c.ListAuditEvents(context.Background())
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].EventType != "Created" {
		t.Fatalf("events = %+v", events)
	}
}

func TestGetUsageAnalytics(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_sandboxes":      3,
			"status_breakdown":     map[string]int{"Running": 2},
			"template_breakdown":   map[string]int{"general-coding": 3},
			"avg_startup_ms":       1500,
			"startup_sample_count": 3,
		})
	})
	out, err := c.GetUsageAnalytics(context.Background())
	if err != nil {
		t.Fatalf("GetUsageAnalytics() error = %v", err)
	}
	if out.TotalSandboxes != 3 || out.StatusBreakdown["Running"] != 2 {
		t.Fatalf("out = %+v", out)
	}
}

func TestGetCostEstimates(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"running_sandboxes":       2,
			"total_estimated_monthly": 123.45,
			"per_template":            []map[string]any{{"template": "t1", "hourly_cost": 0.1, "count": 2}},
		})
	})
	out, err := c.GetCostEstimates(context.Background())
	if err != nil {
		t.Fatalf("GetCostEstimates() error = %v", err)
	}
	if out.RunningSandboxes != 2 || len(out.PerTemplate) != 1 {
		t.Fatalf("out = %+v", out)
	}
}

func TestAdminListSandboxesAndSharing(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/admin/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
		case "/api/v1/admin/sharing":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	if _, err := c.AdminListSandboxes(context.Background()); err != nil {
		t.Fatalf("AdminListSandboxes() error = %v", err)
	}
	if _, err := c.AdminListSharing(context.Background()); err != nil {
		t.Fatalf("AdminListSharing() error = %v", err)
	}
}
