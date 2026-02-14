// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/dag"
)

// Subgraph represents a minimal subset of the StateGraph needed for an operation.
// This enables O(subgraph) operations instead of O(n) full-state operations.
type Subgraph struct {
	// graph is the underlying DAG for this subgraph
	graph *dag.AcyclicGraph

	// nodes contains only the resources in this subgraph
	nodes map[string]*ResourceNode

	// parent is a reference to the full StateGraph
	parent *StateGraph

	// targets are the original target addresses that defined this subgraph
	targets []addrs.AbsResource
}

// ComputeMinimalSubgraph calculates the minimal set of resources needed
// to execute an operation targeting the given resources.
//
// The subgraph includes:
// 1. The target resources themselves
// 2. All upstream dependencies (TopCone) - needed for the targets to exist
// 3. All downstream dependents (BottomCone) - affected by changes to targets
//
// This transforms refresh operations from O(n) to O(subgraph) by only
// including resources that are actually relevant to the operation.
func (sg *StateGraph) ComputeMinimalSubgraph(targets []addrs.AbsResource) *Subgraph {
	sub := &Subgraph{
		graph:   &dag.AcyclicGraph{},
		nodes:   make(map[string]*ResourceNode),
		parent:  sg,
		targets: targets,
	}

	// Collect all nodes in the subgraph
	topCone := sg.ComputeTopCone(targets)
	bottomCone := sg.ComputeBottomCone(targets)

	// Add all nodes to the subgraph (deduplicating)
	for _, node := range topCone {
		sub.addNode(node)
	}
	for _, node := range bottomCone {
		sub.addNode(node)
	}

	// Rebuild edges between nodes that are both in the subgraph
	for _, node := range sub.nodes {
		for _, depAddr := range node.Dependencies {
			if depNode, exists := sub.nodes[depAddr.String()]; exists {
				sub.graph.Connect(dag.BasicEdge(node, depNode))
			}
		}
	}

	return sub
}

// ComputeMinimalSubgraphWithCone creates a subgraph using a pre-computed
// dependency cone. This is more efficient when the cone has already been
// calculated for other purposes.
func (sg *StateGraph) ComputeMinimalSubgraphWithCone(cone *DependencyCone) *Subgraph {
	sub := &Subgraph{
		graph:   &dag.AcyclicGraph{},
		nodes:   make(map[string]*ResourceNode),
		parent:  sg,
		targets: make([]addrs.AbsResource, 0),
	}

	// Add changed resources as targets
	for _, node := range cone.Changed {
		sub.targets = append(sub.targets, node.Addr)
	}

	// Add all nodes from both cones
	for _, node := range cone.TopConeNodes {
		sub.addNode(node)
	}
	for _, node := range cone.BottomConeNodes {
		sub.addNode(node)
	}

	// Rebuild edges
	for _, node := range sub.nodes {
		for _, depAddr := range node.Dependencies {
			if depNode, exists := sub.nodes[depAddr.String()]; exists {
				sub.graph.Connect(dag.BasicEdge(node, depNode))
			}
		}
	}

	return sub
}

// addNode adds a resource node to the subgraph.
func (sub *Subgraph) addNode(node *ResourceNode) {
	key := node.Addr.String()
	if _, exists := sub.nodes[key]; exists {
		return
	}
	sub.nodes[key] = node
	sub.graph.Add(node)
}

// Size returns the number of resources in the subgraph.
func (sub *Subgraph) Size() int {
	return len(sub.nodes)
}

// GetNode returns the ResourceNode for the given address in this subgraph.
func (sub *Subgraph) GetNode(addr addrs.AbsResource) *ResourceNode {
	return sub.nodes[addr.String()]
}

// Contains checks if the given address is part of this subgraph.
func (sub *Subgraph) Contains(addr addrs.AbsResource) bool {
	_, exists := sub.nodes[addr.String()]
	return exists
}

// AllNodes returns all resource nodes in the subgraph.
func (sub *Subgraph) AllNodes() []*ResourceNode {
	result := make([]*ResourceNode, 0, len(sub.nodes))
	for _, node := range sub.nodes {
		result = append(result, node)
	}
	return result
}

// AllAddresses returns all resource addresses in the subgraph.
func (sub *Subgraph) AllAddresses() []addrs.AbsResource {
	result := make([]addrs.AbsResource, 0, len(sub.nodes))
	for _, node := range sub.nodes {
		result = append(result, node.Addr)
	}
	return result
}

// TopologicalOrder returns resources in dependency order within this subgraph.
func (sub *Subgraph) TopologicalOrder() []*ResourceNode {
	vertices := sub.graph.TopologicalOrder()
	result := make([]*ResourceNode, 0, len(vertices))

	for _, v := range vertices {
		if node, ok := v.(*ResourceNode); ok {
			result = append(result, node)
		}
	}

	return result
}

// ReverseTopologicalOrder returns resources in reverse dependency order.
func (sub *Subgraph) ReverseTopologicalOrder() []*ResourceNode {
	vertices := sub.graph.ReverseTopologicalOrder()
	result := make([]*ResourceNode, 0, len(vertices))

	for _, v := range vertices {
		if node, ok := v.(*ResourceNode); ok {
			result = append(result, node)
		}
	}

	return result
}

// Validate checks that the subgraph is a valid DAG.
func (sub *Subgraph) Validate() error {
	return sub.graph.Validate()
}

// FilterRefreshable returns a new subgraph containing only resources that
// need to be refreshed (excludes StaticInjectable resources from the cone).
func (sub *Subgraph) FilterRefreshable(cone *DependencyCone) *Subgraph {
	filtered := &Subgraph{
		graph:   &dag.AcyclicGraph{},
		nodes:   make(map[string]*ResourceNode),
		parent:  sub.parent,
		targets: sub.targets,
	}

	// Build set of static injectable addresses
	staticSet := make(map[string]bool)
	for _, node := range cone.StaticInjectable {
		staticSet[node.Addr.String()] = true
	}

	// Add only nodes that require refresh
	for key, node := range sub.nodes {
		if !staticSet[key] {
			filtered.addNode(node)
		}
	}

	// Rebuild edges
	for _, node := range filtered.nodes {
		for _, depAddr := range node.Dependencies {
			if depNode, exists := filtered.nodes[depAddr.String()]; exists {
				filtered.graph.Connect(dag.BasicEdge(node, depNode))
			}
		}
	}

	return filtered
}

// Reduction returns statistics about the optimization achieved.
func (sub *Subgraph) Reduction(totalResources int) SubgraphReduction {
	return SubgraphReduction{
		TotalResources:    totalResources,
		SubgraphResources: sub.Size(),
		TargetCount:       len(sub.targets),
	}
}

// SubgraphReduction contains statistics about optimization achieved.
type SubgraphReduction struct {
	// TotalResources is the total number of resources in the full state
	TotalResources int

	// SubgraphResources is the number of resources in the subgraph
	SubgraphResources int

	// TargetCount is the number of target resources
	TargetCount int
}

// Percentage returns the percentage of resources that need to be processed.
func (r SubgraphReduction) Percentage() float64 {
	if r.TotalResources == 0 {
		return 100.0
	}
	return (float64(r.SubgraphResources) / float64(r.TotalResources)) * 100
}

// Savings returns the percentage of resources that can be skipped.
func (r SubgraphReduction) Savings() float64 {
	return 100.0 - r.Percentage()
}

// ResourcesSaved returns the absolute number of resources skipped.
func (r SubgraphReduction) ResourcesSaved() int {
	return r.TotalResources - r.SubgraphResources
}
