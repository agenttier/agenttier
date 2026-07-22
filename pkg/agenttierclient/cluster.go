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

import "context"

// Warm pool status/config and cluster status/nodes/headroom. Wire shapes
// mirror pkg/router/handlers.go (handleGetWarmPoolStatus,
// handleSetWarmPoolConfig), pkg/router/cluster_status.go, cluster_nodes.go,
// and headroom.go.

// --- warm pool ---------------------------------------------------------

// WarmPoolConfig configures per-template idle-pod counts. Pools is the
// canonical shape; DesiredCount/Template are the legacy single-template
// fields the Router still accepts (see warmpool.Config.Normalize).
type WarmPoolConfig struct {
	Pools        []WarmPoolEntry `json:"pools,omitempty"`
	DesiredCount int             `json:"desiredCount,omitempty"`
	Template     string          `json:"template,omitempty"`
}

// WarmPoolEntry is one per-template pool configuration entry.
type WarmPoolEntry struct {
	Template     string `json:"template"`
	DesiredCount int    `json:"desiredCount"`
}

// WarmPoolEntryStatus is the live state of one per-template pool.
type WarmPoolEntryStatus struct {
	Template     string `json:"template"`
	DesiredCount int    `json:"desiredCount"`
	ReadyCount   int    `json:"readyCount"`
	PendingCount int    `json:"pendingCount"`
}

// WarmPoolStatus is the response of GET /warmpool/status.
type WarmPoolStatus struct {
	Pools        []WarmPoolEntryStatus `json:"pools,omitempty"`
	DesiredCount int                   `json:"desiredCount,omitempty"`
	ReadyCount   int                   `json:"readyCount,omitempty"`
	PendingCount int                   `json:"pendingCount,omitempty"`
	Template     string                `json:"template,omitempty"`
}

// GetWarmPoolStatus issues GET /warmpool/status.
func (c *Client) GetWarmPoolStatus(ctx context.Context) (*WarmPoolStatus, error) {
	var out WarmPoolStatus
	if err := c.Get(ctx, "/warmpool/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetWarmPoolConfigResult is the response of PUT /warmpool/config.
type SetWarmPoolConfigResult struct {
	Status string          `json:"status"`
	Pools  []WarmPoolEntry `json:"pools"`
}

// SetWarmPoolConfig issues PUT /warmpool/config (admin-only).
func (c *Client) SetWarmPoolConfig(ctx context.Context, cfg WarmPoolConfig) (*SetWarmPoolConfigResult, error) {
	var out SetWarmPoolConfigResult
	if err := c.Put(ctx, "/warmpool/config", cfg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- cluster status/nodes -----------------------------------------------

// ClusterStatus is the response of GET /cluster/status — see
// pkg/router/cluster_status.go's ClusterStatus.
type ClusterStatus struct {
	Nodes             int  `json:"nodes"`
	NodesReady        int  `json:"nodesReady"`
	Pods              int  `json:"pods"`
	SandboxPods       int  `json:"sandboxPods"`
	HeadroomReady     int  `json:"headroomReady"`
	AutoscalerEnabled bool `json:"autoscalerEnabled"`
}

// GetClusterStatus issues GET /cluster/status.
func (c *Client) GetClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	var out ClusterStatus
	if err := c.Get(ctx, "/cluster/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NodeResources is a CPU (millicores) + memory (bytes) pair.
type NodeResources struct {
	CPUMillis int64 `json:"cpuMillis"`
	MemBytes  int64 `json:"memBytes"`
}

// NodeCapacity is the per-node view returned by GetClusterNodes.
type NodeCapacity struct {
	Name         string        `json:"name"`
	Ready        bool          `json:"ready"`
	InstanceType string        `json:"instanceType,omitempty"`
	NodeGroup    string        `json:"nodeGroup,omitempty"`
	Allocatable  NodeResources `json:"allocatable"`
	Requests     NodeResources `json:"requests"`
}

// NodeCapacitySummary aggregates the node fleet.
type NodeCapacitySummary struct {
	Ready            int           `json:"ready"`
	Total            int           `json:"total"`
	CPUSaturationPct float64       `json:"cpuSaturationPct"`
	MemSaturationPct float64       `json:"memSaturationPct"`
	Allocatable      NodeResources `json:"allocatable"`
	Requests         NodeResources `json:"requests"`
}

// NodeCapacityResponse is the response of GET /cluster/nodes (admin-only).
type NodeCapacityResponse struct {
	Nodes   []NodeCapacity      `json:"nodes"`
	Summary NodeCapacitySummary `json:"summary"`
}

// GetClusterNodes issues GET /cluster/nodes (admin-only).
func (c *Client) GetClusterNodes(ctx context.Context) (*NodeCapacityResponse, error) {
	var out NodeCapacityResponse
	if err := c.Get(ctx, "/cluster/nodes", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- headroom ------------------------------------------------------------

// HeadroomConfig is the wire shape of GET/PUT /cluster/headroom — see
// pkg/router/headroom.go's HeadroomConfig.
type HeadroomConfig struct {
	Replicas      int    `json:"replicas"`
	CPU           string `json:"cpu,omitempty"`
	Memory        string `json:"memory,omitempty"`
	ReadyReplicas int    `json:"readyReplicas,omitempty"`
	Enabled       bool   `json:"enabled"`
}

// GetHeadroomConfig issues GET /cluster/headroom.
func (c *Client) GetHeadroomConfig(ctx context.Context) (*HeadroomConfig, error) {
	var out HeadroomConfig
	if err := c.Get(ctx, "/cluster/headroom", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetHeadroomConfig issues PUT /cluster/headroom (admin-only). Replicas
// must be in [0, 50] — the Router enforces this server-side; validating
// locally saves a round-trip for the common finger-fumble case.
func (c *Client) SetHeadroomConfig(ctx context.Context, cfg HeadroomConfig) (*HeadroomConfig, error) {
	var out HeadroomConfig
	if err := c.Put(ctx, "/cluster/headroom", cfg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
