// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/zclconf/go-cty/cty"
)

// ResourceNode represents a single resource in the state graph.
// It wraps a state resource with dependency metadata for DAG traversal.
type ResourceNode struct {
	// Addr is the absolute address of this resource
	Addr addrs.AbsResource

	// Resource is a reference to the underlying state resource
	Resource *states.Resource

	// Dependencies are the addresses this resource depends on (upstream)
	Dependencies []addrs.AbsResource

	// Dependents are the addresses that depend on this resource (downstream)
	Dependents []addrs.AbsResource

	// CachedValues holds static values from the state that can be injected
	// to avoid refresh when the resource hasn't changed
	CachedValues map[string]cty.Value

	// NeedsRefresh indicates whether this resource needs to be refreshed
	NeedsRefresh bool
}

// NewResourceNode creates a ResourceNode from a state resource.
func NewResourceNode(resource *states.Resource) *ResourceNode {
	return &ResourceNode{
		Addr:         resource.Addr,
		Resource:     resource,
		Dependencies: make([]addrs.AbsResource, 0),
		Dependents:   make([]addrs.AbsResource, 0),
		CachedValues: make(map[string]cty.Value),
		NeedsRefresh: true,
	}
}

// AddDependency adds an upstream dependency to this node.
func (n *ResourceNode) AddDependency(addr addrs.AbsResource) {
	for _, dep := range n.Dependencies {
		if dep.Equal(addr) {
			return
		}
	}
	n.Dependencies = append(n.Dependencies, addr)
}

// AddDependent adds a downstream dependent to this node.
func (n *ResourceNode) AddDependent(addr addrs.AbsResource) {
	for _, dep := range n.Dependents {
		if dep.Equal(addr) {
			return
		}
	}
	n.Dependents = append(n.Dependents, addr)
}

// HasDependency checks if this node depends on the given address.
func (n *ResourceNode) HasDependency(addr addrs.AbsResource) bool {
	for _, dep := range n.Dependencies {
		if dep.Equal(addr) {
			return true
		}
	}
	return false
}

// HasDependent checks if the given address depends on this node.
func (n *ResourceNode) HasDependent(addr addrs.AbsResource) bool {
	for _, dep := range n.Dependents {
		if dep.Equal(addr) {
			return true
		}
	}
	return false
}

// Name returns a string representation suitable for DAG vertex naming.
func (n *ResourceNode) Name() string {
	return n.Addr.String()
}

// String implements fmt.Stringer for debugging.
func (n *ResourceNode) String() string {
	return n.Addr.String()
}
