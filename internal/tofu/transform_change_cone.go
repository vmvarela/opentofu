// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"fmt"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/dag"
	"github.com/opentofu/opentofu/internal/states"
)

// ChangeConeTransformer is a GraphTransformer that filters the graph to only
// include resources in the "change cone" - resources that have configuration
// changes and their dependencies/dependents.
//
// This is used with -refresh=changed mode to not only skip refreshing unchanged
// resources, but also to skip planning them entirely.
//
// The transformer detects changes by comparing the configuration with the
// prior state. Resources are considered "changed" if:
//   - They are new (in config but not in state)
//   - They are orphaned (in state but not in config)
//   - They were marked as changed by the RefreshTracker during a previous walk
//     (this covers attribute-level changes detected at execution time)
type ChangeConeTransformer struct {
	// Skip indicates whether this transformer should be skipped.
	// Set to true during validate walks or when change cone filtering is disabled.
	Skip bool

	// SkipReason provides a human-readable reason for why the transformer was skipped.
	// Only used for logging when Skip is true.
	SkipReason string

	// RefreshTracker can be pre-populated with changed resources from a
	// previous walk. If empty, the transformer will detect changes itself.
	RefreshTracker *RefreshTracker

	// Config is the current configuration
	Config *configs.Config

	// State is the prior state
	State *states.State
}

// Transform filters the graph to only include the change cone.
// The change cone consists of:
// 1. Resources with direct configuration changes (seeds)
// 2. All descendants (resources that depend on changed resources)
// 3. All ancestors (dependencies needed for the above to work)
func (t *ChangeConeTransformer) Transform(_ context.Context, g *Graph) error {
	// Skip if explicitly disabled
	if t.Skip {
		if t.SkipReason != "" {
			log.Printf("[TRACE] ChangeConeTransformer: skipping (%s)", t.SkipReason)
		}
		return nil
	}

	// If no config or state, don't filter
	if t.Config == nil || t.State == nil {
		log.Printf("[TRACE] ChangeConeTransformer: no config or state, skipping filtering")
		return nil
	}

	// First, try to use pre-populated changed resources from RefreshTracker
	var changedResources []addrs.ConfigResource
	if t.RefreshTracker != nil {
		changedResources = t.RefreshTracker.GetChangedResources()
	}

	// If no pre-populated changes, detect them now
	if len(changedResources) == 0 {
		changedResources = t.detectChangedResources()
	}

	if len(changedResources) == 0 {
		// No changes detected - keep the full graph
		// This is the safe default to avoid missing any changes
		log.Printf("[TRACE] ChangeConeTransformer: no configuration changes detected, keeping full graph")
		return nil
	}

	log.Printf("[DEBUG] ChangeConeTransformer: %d resources with configuration changes", len(changedResources))
	for _, addr := range changedResources {
		log.Printf("[TRACE] ChangeConeTransformer: changed resource: %s", addr)
	}

	// Build the change cone
	changeCone, err := t.buildChangeCone(g, changedResources)
	if err != nil {
		return fmt.Errorf("building change cone: %w", err)
	}

	if changeCone.Len() == 0 {
		log.Printf("[DEBUG] ChangeConeTransformer: change cone is empty, keeping full graph")
		return nil
	}

	log.Printf("[DEBUG] ChangeConeTransformer: change cone contains %d vertices", changeCone.Len())

	// Include root module outputs whose resource dependencies are all
	// within the change cone. This mirrors the TargetingTransformer behavior
	// to ensure outputs derived from changed resources are updated.
	outputNodes := t.getChangeConeOutputNodes(changeCone, g)
	for v := range outputNodes {
		changeCone.Add(v)
	}
	if outputNodes.Len() > 0 {
		log.Printf("[DEBUG] ChangeConeTransformer: added %d root output nodes to change cone", outputNodes.Len())
	}

	// Remove vertices not in the change cone
	totalVertices := int64(len(g.Vertices()))
	removed := 0
	for _, v := range g.Vertices() {
		if !changeCone.Include(v) {
			log.Printf("[TRACE] ChangeConeTransformer: removing %q, not in change cone", dag.VertexName(v))
			g.Remove(v)
			removed++
		}
	}

	log.Printf("[DEBUG] ChangeConeTransformer: removed %d vertices from graph", removed)

	// Record stats in the RefreshTracker for the diagnostic summary
	if t.RefreshTracker != nil {
		kept := totalVertices - int64(removed)
		t.RefreshTracker.RecordChangeConeStats(totalVertices, kept)
	}

	return nil
}

// detectChangedResources compares config with state to find changed resources.
// This is a simplified detection that identifies:
// - New resources (in config, not in state)
// - Orphaned resources (in state, not in config)
//
// Note: Attribute-level changes are not detected here because that requires
// evaluating the configuration with a proper EvalContext. That detection
// happens during the graph walk in detectConfigChange(). If we have
// RefreshTracker data from a previous walk, we use that instead.
func (t *ChangeConeTransformer) detectChangedResources() []addrs.ConfigResource {
	// Walk the configuration to find resources
	configResources := t.collectConfigResources(t.Config.Module, addrs.RootModule)

	// Walk the state to find resources
	stateResources := t.collectStateResources()

	// Short-circuit: if both are empty, there's nothing to compare
	if len(configResources) == 0 && len(stateResources) == 0 {
		return nil
	}

	var changed []addrs.ConfigResource

	// Find new resources (in config but not in state)
	for configAddrStr, configAddr := range configResources {
		if _, exists := stateResources[configAddrStr]; !exists {
			log.Printf("[TRACE] ChangeConeTransformer: %s is new (not in state)", configAddr)
			changed = append(changed, configAddr)
		}
	}

	// Find orphaned resources (in state but not in config)
	for stateAddrStr, stateAddr := range stateResources {
		if _, exists := configResources[stateAddrStr]; !exists {
			log.Printf("[TRACE] ChangeConeTransformer: %s is orphaned (not in config)", stateAddr)
			changed = append(changed, stateAddr)
		}
	}

	return changed
}

// collectConfigResources walks the config module tree and collects all resource addresses.
func (t *ChangeConeTransformer) collectConfigResources(mod *configs.Module, modAddr addrs.Module) map[string]addrs.ConfigResource {
	resources := make(map[string]addrs.ConfigResource)

	if mod == nil {
		return resources
	}

	// Collect managed resources
	for _, r := range mod.ManagedResources {
		addr := r.Addr().InModule(modAddr)
		resources[addr.String()] = addr
	}

	// Collect data resources
	for _, r := range mod.DataResources {
		addr := r.Addr().InModule(modAddr)
		resources[addr.String()] = addr
	}

	// Recursively collect from child modules
	for name := range mod.ModuleCalls {
		if childMod := t.Config.Descendent(modAddr.Child(name)); childMod != nil {
			childResources := t.collectConfigResources(childMod.Module, modAddr.Child(name))
			for k, v := range childResources {
				resources[k] = v
			}
		}
	}

	return resources
}

// collectStateResources walks the state and collects all resource addresses.
func (t *ChangeConeTransformer) collectStateResources() map[string]addrs.ConfigResource {
	resources := make(map[string]addrs.ConfigResource)

	if t.State == nil {
		return resources
	}

	for _, mod := range t.State.Modules {
		for _, rs := range mod.Resources {
			addr := rs.Addr.Config()
			resources[addr.String()] = addr
		}
	}

	return resources
}

// buildChangeCone constructs the set of vertices that should be included in the plan.
// This includes:
// - Changed resources (seeds)
// - All descendants (resources depending on changed resources - to detect ripple effects)
// - All ancestors (dependencies needed for proper planning)
func (t *ChangeConeTransformer) buildChangeCone(g *Graph, changedResources []addrs.ConfigResource) (dag.Set, error) {
	changeCone := make(dag.Set)

	// Step 1: Find all vertices corresponding to changed resources
	changedVertices := t.findChangedVertices(g, changedResources)
	if len(changedVertices) == 0 {
		log.Printf("[DEBUG] ChangeConeTransformer: no changed vertices found in graph")
		return changeCone, nil
	}

	log.Printf("[DEBUG] ChangeConeTransformer: found %d changed vertices", len(changedVertices))

	// Step 2: Add changed vertices and their descendants (resources that depend on them)
	// This is critical to detect ripple effects of the change
	for _, v := range changedVertices {
		changeCone.Add(v)

		// Include all descendants - resources that depend on this changed resource
		descendants, err := g.Descendents(v)
		if err != nil {
			return changeCone, fmt.Errorf("failed to get descendants of %q: %w", dag.VertexName(v), err)
		}
		for desc := range descendants {
			changeCone.Add(desc)
		}
		log.Printf("[TRACE] ChangeConeTransformer: %q has %d descendants", dag.VertexName(v), descendants.Len())
	}

	// Step 3: Add all ancestors (dependencies) of everything in the cone so far
	// This ensures we have all the dependencies needed for proper planning
	coneCopy := make(dag.Set)
	for v := range changeCone {
		coneCopy.Add(v)
	}

	for v := range coneCopy {
		ancestors, err := g.Ancestors(v)
		if err != nil {
			return changeCone, fmt.Errorf("failed to get ancestors of %q: %w", dag.VertexName(v), err)
		}
		for anc := range ancestors {
			changeCone.Add(anc)
		}
	}

	return changeCone, nil
}

// findChangedVertices finds all graph vertices that correspond to the given
// changed config resources. Uses a map for O(1) lookups instead of nested iteration.
func (t *ChangeConeTransformer) findChangedVertices(g *Graph, changedResources []addrs.ConfigResource) []dag.Vertex {
	// Build a set of changed resource address strings for O(1) lookups
	changedSet := make(map[string]struct{}, len(changedResources))
	for _, addr := range changedResources {
		changedSet[addr.String()] = struct{}{}
	}

	var result []dag.Vertex

	for _, v := range g.Vertices() {
		// Check if this vertex is a resource node and get its address
		var vertexAddr addrs.ConfigResource
		switch r := v.(type) {
		case GraphNodeResourceInstance:
			vertexAddr = r.ResourceInstanceAddr().ContainingResource().Config()
		case GraphNodeConfigResource:
			vertexAddr = r.ResourceAddr()
		default:
			// Not a resource node, skip
			continue
		}

		// Check if this vertex matches any changed resource
		if _, changed := changedSet[vertexAddr.String()]; changed {
			result = append(result, v)
			log.Printf("[TRACE] ChangeConeTransformer: matched changed resource %q to vertex %q", vertexAddr, dag.VertexName(v))
		}
	}

	return result
}

// getChangeConeOutputNodes finds root module output nodes whose resource
// dependencies are all within the change cone, and returns them (along with
// their own dependencies) as a set.
//
// This mirrors the logic in TargetingTransformer.getTargetedOutputNodes:
// outputs cannot be targeted/filtered independently, so we include root
// outputs that are derived entirely from resources already in the cone.
func (t *ChangeConeTransformer) getChangeConeOutputNodes(changeCone dag.Set, g *Graph) dag.Set {
	outputNodes := make(dag.Set)

	for _, v := range g.Vertices() {
		// Identify output nodes via the graphNodeTemporaryValue interface
		tv, ok := v.(graphNodeTemporaryValue)
		if !ok {
			continue
		}

		// Root module outputs return false from temporaryValue() --
		// non-root module outputs return true and should be skipped here.
		// We use walkInvalid since we don't care about the specific operation.
		if tv.temporaryValue(walkInvalid) {
			continue
		}

		// Check if ALL resource ancestors of this output are in the change cone
		deps, _ := g.Ancestors(v)
		found := 0
		for _, d := range deps {
			switch d.(type) {
			case GraphNodeResourceInstance:
			case GraphNodeConfigResource:
			default:
				continue
			}

			if !changeCone.Include(d) {
				// A resource dependency is NOT in the change cone, so
				// we can't include this output
				found = 0
				break
			}

			found++
		}

		if found > 0 {
			// All resource dependencies are in the cone -- include the
			// output and all its own dependencies
			outputNodes.Add(v)
			for _, d := range deps {
				outputNodes.Add(d)
			}
			log.Printf("[TRACE] ChangeConeTransformer: including root output %q (all %d resource deps in cone)", dag.VertexName(v), found)
		}
	}

	return outputNodes
}
