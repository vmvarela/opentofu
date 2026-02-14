// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"context"
	"sync"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/dag"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// StateGraph represents the state as a Directed Acyclic Graph.
// Instead of treating state as a flat list of resources, StateGraph
// models the dependency relationships enabling efficient subgraph operations.
type StateGraph struct {
	// mu protects concurrent access to the graph
	mu sync.RWMutex

	// graph is the underlying DAG implementation
	graph *dag.AcyclicGraph

	// nodes maps resource addresses to their corresponding nodes
	nodes map[string]*ResourceNode

	// state is a reference to the original state (read-only after construction)
	state *states.State
}

// NewStateGraph creates a new empty StateGraph.
func NewStateGraph() *StateGraph {
	return &StateGraph{
		graph: &dag.AcyclicGraph{},
		nodes: make(map[string]*ResourceNode),
	}
}

// FromState constructs a StateGraph from an existing State.
// This converts the flat resource list into a DAG based on dependencies.
func FromState(state *states.State) *StateGraph {
	sg := NewStateGraph()
	sg.state = state

	if state == nil {
		return sg
	}

	// First pass: create all nodes
	for _, module := range state.Modules {
		for _, resource := range module.Resources {
			node := NewResourceNode(resource)
			sg.addNode(node)
		}
	}

	// Second pass: establish dependency edges
	// Dependencies are extracted from the resource instances' dependency metadata
	for _, module := range state.Modules {
		for _, resource := range module.Resources {
			sg.extractDependencies(resource)
		}
	}

	return sg
}

// addNode adds a ResourceNode to the graph.
func (sg *StateGraph) addNode(node *ResourceNode) {
	sg.mu.Lock()
	defer sg.mu.Unlock()

	key := node.Addr.String()
	sg.nodes[key] = node
	sg.graph.Add(node)
}

// extractDependencies analyzes a resource and establishes edges in the graph.
func (sg *StateGraph) extractDependencies(resource *states.Resource) {
	sg.mu.Lock()
	defer sg.mu.Unlock()

	sourceKey := resource.Addr.String()
	sourceNode, exists := sg.nodes[sourceKey]
	if !exists {
		return
	}

	// Extract dependencies from each instance
	for _, instance := range resource.Instances {
		if instance.Current == nil {
			continue
		}

		// Dependencies are stored in the resource instance object
		for _, dep := range instance.Current.Dependencies {
			// dep is addrs.ConfigResource, we need to find matching AbsResources
			for _, targetNode := range sg.nodes {
				if targetNode.Addr.Config().Equal(dep) {
					// Create bidirectional edge tracking
					sourceNode.AddDependency(targetNode.Addr)
					targetNode.AddDependent(sourceNode.Addr)

					// Add edge to the underlying DAG (source depends on target)
					sg.graph.Connect(dag.BasicEdge(sourceNode, targetNode))
				}
			}
		}
	}
}

// GetNode returns the ResourceNode for the given address, or nil if not found.
func (sg *StateGraph) GetNode(addr addrs.AbsResource) *ResourceNode {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	return sg.nodes[addr.String()]
}

// AllNodes returns all resource nodes in the graph.
func (sg *StateGraph) AllNodes() []*ResourceNode {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	result := make([]*ResourceNode, 0, len(sg.nodes))
	for _, node := range sg.nodes {
		result = append(result, node)
	}
	return result
}

// Size returns the number of resources in the graph.
func (sg *StateGraph) Size() int {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	return len(sg.nodes)
}

// Validate checks that the graph is a valid DAG (no cycles).
func (sg *StateGraph) Validate() error {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	return sg.graph.Validate()
}

// TopologicalOrder returns resources in dependency order (dependencies first).
func (sg *StateGraph) TopologicalOrder() []*ResourceNode {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	vertices := sg.graph.TopologicalOrder()
	result := make([]*ResourceNode, 0, len(vertices))

	for _, v := range vertices {
		if node, ok := v.(*ResourceNode); ok {
			result = append(result, node)
		}
	}

	return result
}

// ReverseTopologicalOrder returns resources in reverse dependency order
// (dependents first, then their dependencies).
func (sg *StateGraph) ReverseTopologicalOrder() []*ResourceNode {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	vertices := sg.graph.ReverseTopologicalOrder()
	result := make([]*ResourceNode, 0, len(vertices))

	for _, v := range vertices {
		if node, ok := v.(*ResourceNode); ok {
			result = append(result, node)
		}
	}

	return result
}

// Walk performs a parallel walk of the graph, calling fn for each node.
// Nodes are visited in dependency order with parallelism where possible.
func (sg *StateGraph) Walk(ctx context.Context, fn func(context.Context, *ResourceNode) error) error {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	diags := sg.graph.Walk(func(v dag.Vertex) tfdiags.Diagnostics {
		node, ok := v.(*ResourceNode)
		if !ok {
			return nil
		}

		if err := fn(ctx, node); err != nil {
			var diags tfdiags.Diagnostics
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Error walking resource node",
				err.Error(),
			))
			return diags
		}
		return nil
	})

	if diags.HasErrors() {
		return diags.Err()
	}
	return nil
}
