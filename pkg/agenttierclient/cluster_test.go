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

func TestWarmPoolStatusAndConfig(t *testing.T) {
	var gotMethod string
	var gotBody WarmPoolConfig
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pools": []map[string]any{{"template": "t1", "desiredCount": 2, "readyCount": 2}},
			})
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "updated",
				"pools":  gotBody.Pools,
			})
		}
	})
	status, err := c.GetWarmPoolStatus(context.Background())
	if err != nil {
		t.Fatalf("GetWarmPoolStatus() error = %v", err)
	}
	if len(status.Pools) != 1 || status.Pools[0].Template != "t1" {
		t.Fatalf("status = %+v", status)
	}

	result, err := c.SetWarmPoolConfig(context.Background(), WarmPoolConfig{
		Pools: []WarmPoolEntry{{Template: "t1", DesiredCount: 3}},
	})
	if err != nil {
		t.Fatalf("SetWarmPoolConfig() error = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if result.Status != "updated" || len(result.Pools) != 1 || result.Pools[0].DesiredCount != 3 {
		t.Fatalf("result = %+v", result)
	}
}

func TestGetClusterStatus(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodes": 5, "nodesReady": 4, "pods": 20, "sandboxPods": 10, "autoscalerEnabled": true,
		})
	})
	out, err := c.GetClusterStatus(context.Background())
	if err != nil {
		t.Fatalf("GetClusterStatus() error = %v", err)
	}
	if out.Nodes != 5 || out.NodesReady != 4 || !out.AutoscalerEnabled {
		t.Fatalf("out = %+v", out)
	}
}

func TestGetClusterNodes(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodes": []map[string]any{
				{"name": "node-1", "ready": true, "instanceType": "t3.large"},
			},
			"summary": map[string]any{"ready": 1, "total": 1, "cpuSaturationPct": 42.5},
		})
	})
	out, err := c.GetClusterNodes(context.Background())
	if err != nil {
		t.Fatalf("GetClusterNodes() error = %v", err)
	}
	if len(out.Nodes) != 1 || out.Nodes[0].Name != "node-1" {
		t.Fatalf("out = %+v", out)
	}
	if out.Summary.CPUSaturationPct != 42.5 {
		t.Errorf("CPUSaturationPct = %v, want 42.5", out.Summary.CPUSaturationPct)
	}
}

func TestGetSetHeadroomConfig(t *testing.T) {
	var gotBody HeadroomConfig
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "replicas": 2, "readyReplicas": 2})
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "replicas": gotBody.Replicas})
		}
	})
	cfg, err := c.GetHeadroomConfig(context.Background())
	if err != nil {
		t.Fatalf("GetHeadroomConfig() error = %v", err)
	}
	if !cfg.Enabled || cfg.Replicas != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
	updated, err := c.SetHeadroomConfig(context.Background(), HeadroomConfig{Replicas: 5})
	if err != nil {
		t.Fatalf("SetHeadroomConfig() error = %v", err)
	}
	if updated.Replicas != 5 {
		t.Fatalf("updated = %+v", updated)
	}
}
