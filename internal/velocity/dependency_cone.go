// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"github.com/opentofu/opentofu/internal/addrs"
)

// ConeType specifies the direction of the dependency cone calculation.
type ConeType int

const (
	// TopCone calculates upstream dependencies (what this resource needs).
	TopCone ConeType = iota

	// BottomCone calculates downstream dependents (what depends on this resource).
	BottomCone

	// FullCone calculates both upstream and downstream (complete change impact).
	FullCone
)

// DependencyCone represents a computed cone of resources affected by a change.
// It contains both the resources that need to be refreshed and those that
// can have their values statically injected from cached state.
type DependencyCone struct {
	// Changed is the set of resources that triggered the cone calculation
	Changed []*ResourceNode

	// TopCone contains upstream dependencies (resources this depends on)
	TopConeNodes []*ResourceNode

	// BottomCone contains downstream dependents (resources depending on this)
	BottomConeNodes []*ResourceNode

	// StaticInjectable are TopCone resources whose values can be injected
	// without refresh because they haven't changed
	StaticInjectable []*ResourceNode

	// RequiresRefresh is the final set of resources that need provider refresh
	RequiresRefresh []*ResourceNode
}

// ComputeTopCone calculates all upstream dependencies for the given resources.
// These are resources that the targets depend on (their dependencies).
// This is useful for ensuring all prerequisite resources are available.
func (sg *StateGraph) ComputeTopCone(targets []addrs.AbsResource) []*ResourceNode {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	visited := make(map[string]bool)
	result := make([]*ResourceNode, 0)

	var traverse func(addr addrs.AbsResource)
	traverse = func(addr addrs.AbsResource) {
		key := addr.String()
		if visited[key] {
			return
		}
		visited[key] = true

		node := sg.nodes[key]
		if node == nil {
			return
		}

		// Recursively traverse all dependencies
		for _, depAddr := range node.Dependencies {
			traverse(depAddr)
		}

		result = append(result, node)
	}

	for _, target := range targets {
		traverse(target)
	}

	return result
}

// ComputeBottomCone calculates all downstream dependents for the given resources.
// These are resources that depend on the targets (their dependents).
// This is useful for determining the "blast radius" of a change.
func (sg *StateGraph) ComputeBottomCone(targets []addrs.AbsResource) []*ResourceNode {
	sg.mu.RLock()
	defer sg.mu.RUnlock()

	visited := make(map[string]bool)
	result := make([]*ResourceNode, 0)

	var traverse func(addr addrs.AbsResource)
	traverse = func(addr addrs.AbsResource) {
		key := addr.String()
		if visited[key] {
			return
		}
		visited[key] = true

		node := sg.nodes[key]
		if node == nil {
			return
		}

		result = append(result, node)

		// Recursively traverse all dependents
		for _, depAddr := range node.Dependents {
			traverse(depAddr)
		}
	}

	for _, target := range targets {
		traverse(target)
	}

	return result
}

// ComputeChangeCone calculates the full cone of affected resources for a change.
// This combines both TopCone (dependencies) and BottomCone (dependents) to get
// the complete set of resources that might be affected by changes to the targets.
//
// The cone calculation follows Stategraph's approach:
//   - TopCone: Resources needed for the target to exist (upstream)
//   - BottomCone: Resources impacted by changes to the target (downstream)
//
// The function also identifies which TopCone resources can have their values
// statically injected (avoiding refresh) based on change detection.
func (sg *StateGraph) ComputeChangeCone(targets []addrs.AbsResource, changedAddrs []addrs.AbsResource) *DependencyCone {
	cone := &DependencyCone{
		Changed:          make([]*ResourceNode, 0),
		TopConeNodes:     make([]*ResourceNode, 0),
		BottomConeNodes:  make([]*ResourceNode, 0),
		StaticInjectable: make([]*ResourceNode, 0),
		RequiresRefresh:  make([]*ResourceNode, 0),
	}

	// Build set of changed addresses for fast lookup
	changedSet := make(map[string]bool)
	for _, addr := range changedAddrs {
		changedSet[addr.String()] = true
		if node := sg.GetNode(addr); node != nil {
			cone.Changed = append(cone.Changed, node)
		}
	}

	// Compute both cones
	cone.TopConeNodes = sg.ComputeTopCone(targets)
	cone.BottomConeNodes = sg.ComputeBottomCone(targets)

	// Determine which TopCone resources can be statically injected
	// (resources that haven't changed and don't need refresh)
	topConeSet := make(map[string]bool)
	for _, node := range cone.TopConeNodes {
		topConeSet[node.Addr.String()] = true
	}

	bottomConeSet := make(map[string]bool)
	for _, node := range cone.BottomConeNodes {
		bottomConeSet[node.Addr.String()] = true
	}

	// A resource requires refresh if:
	// 1. It's in the changed set, OR
	// 2. It's in the BottomCone (affected by changes), OR
	// 3. It's in both TopCone and depends on something changed
	for _, node := range cone.TopConeNodes {
		key := node.Addr.String()

		if changedSet[key] || bottomConeSet[key] {
			cone.RequiresRefresh = append(cone.RequiresRefresh, node)
		} else if sg.dependsOnChanged(node, changedSet) {
			cone.RequiresRefresh = append(cone.RequiresRefresh, node)
		} else {
			// This resource hasn't changed and doesn't depend on anything
			// that changed, so its values can be statically injected
			cone.StaticInjectable = append(cone.StaticInjectable, node)
		}
	}

	// All BottomCone resources require refresh
	for _, node := range cone.BottomConeNodes {
		key := node.Addr.String()
		// Avoid duplicates (if also in TopCone, already processed)
		if !topConeSet[key] {
			cone.RequiresRefresh = append(cone.RequiresRefresh, node)
		}
	}

	return cone
}

// dependsOnChanged checks if a node transitively depends on any changed resource.
func (sg *StateGraph) dependsOnChanged(node *ResourceNode, changedSet map[string]bool) bool {
	visited := make(map[string]bool)

	var check func(n *ResourceNode) bool
	check = func(n *ResourceNode) bool {
		key := n.Addr.String()
		if visited[key] {
			return false
		}
		visited[key] = true

		for _, depAddr := range n.Dependencies {
			if changedSet[depAddr.String()] {
				return true
			}
			if depNode := sg.nodes[depAddr.String()]; depNode != nil {
				if check(depNode) {
					return true
				}
			}
		}
		return false
	}

	return check(node)
}

// GetRefreshAddresses returns the addresses that need to be refreshed.
// This is a convenience method that extracts addresses from RequiresRefresh.
func (cone *DependencyCone) GetRefreshAddresses() []addrs.AbsResource {
	result := make([]addrs.AbsResource, len(cone.RequiresRefresh))
	for i, node := range cone.RequiresRefresh {
		result[i] = node.Addr
	}
	return result
}

// GetStaticAddresses returns the addresses that can be statically injected.
func (cone *DependencyCone) GetStaticAddresses() []addrs.AbsResource {
	result := make([]addrs.AbsResource, len(cone.StaticInjectable))
	for i, node := range cone.StaticInjectable {
		result[i] = node.Addr
	}
	return result
}

// RefreshCount returns the number of resources requiring refresh.
func (cone *DependencyCone) RefreshCount() int {
	return len(cone.RequiresRefresh)
}

// StaticCount returns the number of resources that can be statically injected.
func (cone *DependencyCone) StaticCount() int {
	return len(cone.StaticInjectable)
}

// TotalCount returns the total number of resources in the cone.
func (cone *DependencyCone) TotalCount() int {
	return cone.RefreshCount() + cone.StaticCount()
}
