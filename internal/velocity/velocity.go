// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"context"
	"fmt"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/zclconf/go-cty/cty"
)

// RefreshStrategy defines how refresh operations should be executed.
type RefreshStrategy int

const (
	// RefreshStrategyFull refreshes all resources (current behavior).
	RefreshStrategyFull RefreshStrategy = iota

	// RefreshStrategyTargeted refreshes only targeted resources and their cones.
	RefreshStrategyTargeted

	// RefreshStrategyOptimized uses change detection to minimize refresh.
	RefreshStrategyOptimized
)

// VelocityEngine is the main entry point for velocity optimizations.
// It wraps state operations with dependency-aware optimizations.
type VelocityEngine struct {
	// stateGraph is the in-memory DAG representation of the state
	stateGraph *StateGraph

	// lockManager handles resource locking
	lockManager ResourceLockManager

	// strategy determines how refresh operations are performed
	strategy RefreshStrategy

	// enableStaticInjection allows skipping refresh for unchanged dependencies
	enableStaticInjection bool
}

// EngineOpts configures the VelocityEngine.
type EngineOpts struct {
	// Strategy defines the refresh strategy to use
	Strategy RefreshStrategy

	// EnableStaticInjection enables skipping refresh for unchanged dependencies
	EnableStaticInjection bool

	// LockManager is the lock manager to use (nil = no locking)
	LockManager ResourceLockManager
}

// DefaultEngineOpts returns sensible default options.
func DefaultEngineOpts() *EngineOpts {
	return &EngineOpts{
		Strategy:              RefreshStrategyTargeted,
		EnableStaticInjection: true,
		LockManager:           nil,
	}
}

// NewVelocityEngine creates a new VelocityEngine from a state.
func NewVelocityEngine(state *states.State, opts *EngineOpts) *VelocityEngine {
	if opts == nil {
		opts = DefaultEngineOpts()
	}

	engine := &VelocityEngine{
		stateGraph:            FromState(state),
		strategy:              opts.Strategy,
		enableStaticInjection: opts.EnableStaticInjection,
		lockManager:           opts.LockManager,
	}

	if engine.lockManager == nil {
		engine.lockManager = NewNoOpLockManager()
	}

	return engine
}

// ComputeRefreshScope calculates the minimal set of resources that need
// to be refreshed for an operation targeting the given resources.
//
// This is the main optimization function that transforms O(n) refresh
// operations into O(subgraph) by analyzing the dependency graph.
func (e *VelocityEngine) ComputeRefreshScope(
	ctx context.Context,
	targets []addrs.Targetable,
	changedResources []addrs.AbsResource,
) (*RefreshScope, error) {

	scope := &RefreshScope{
		Strategy:     e.strategy,
		AllResources: e.stateGraph.AllNodes(),
	}

	// Convert Targetable to AbsResource
	targetAddrs := e.resolveTargets(targets)

	switch e.strategy {
	case RefreshStrategyFull:
		// Full refresh - all resources
		scope.ToRefresh = scope.AllResources
		scope.Static = nil

	case RefreshStrategyTargeted:
		// Compute minimal subgraph based on targets
		if len(targetAddrs) == 0 {
			// No targets means refresh everything
			scope.ToRefresh = scope.AllResources
		} else {
			subgraph := e.stateGraph.ComputeMinimalSubgraph(targetAddrs)
			scope.ToRefresh = subgraph.AllNodes()
			scope.Subgraph = subgraph
		}

	case RefreshStrategyOptimized:
		// Use change detection and static injection
		if len(targetAddrs) == 0 {
			targetAddrs = e.allResourceAddrs()
		}

		cone := e.stateGraph.ComputeChangeCone(targetAddrs, changedResources)
		scope.Cone = cone

		if e.enableStaticInjection {
			scope.ToRefresh = cone.RequiresRefresh
			scope.Static = cone.StaticInjectable
		} else {
			scope.ToRefresh = append(cone.RequiresRefresh, cone.StaticInjectable...)
		}

		// Also store the subgraph for ordering
		scope.Subgraph = e.stateGraph.ComputeMinimalSubgraphWithCone(cone)
	}

	// Log optimization stats
	e.logOptimization(scope)

	return scope, nil
}

// resolveTargets converts Targetable addresses to AbsResource addresses.
func (e *VelocityEngine) resolveTargets(targets []addrs.Targetable) []addrs.AbsResource {
	result := make([]addrs.AbsResource, 0, len(targets))

	for _, target := range targets {
		switch t := target.(type) {
		case addrs.AbsResource:
			result = append(result, t)
		case addrs.AbsResourceInstance:
			result = append(result, t.ContainingResource())
		case addrs.ConfigResource:
			// Find all instances of this config resource in the state
			for _, node := range e.stateGraph.nodes {
				if node.Addr.Config().Equal(t) {
					result = append(result, node.Addr)
				}
			}
		}
	}

	return result
}

// allResourceAddrs returns all resource addresses in the state graph.
func (e *VelocityEngine) allResourceAddrs() []addrs.AbsResource {
	nodes := e.stateGraph.AllNodes()
	result := make([]addrs.AbsResource, len(nodes))
	for i, node := range nodes {
		result[i] = node.Addr
	}
	return result
}

// logOptimization logs statistics about the optimization achieved.
func (e *VelocityEngine) logOptimization(scope *RefreshScope) {
	total := len(scope.AllResources)
	refresh := len(scope.ToRefresh)
	static := len(scope.Static)

	if total == 0 {
		return
	}

	savings := float64(total-refresh) / float64(total) * 100

	log.Printf("[DEBUG] Velocity: Total=%d, Refresh=%d, Static=%d, Savings=%.1f%%",
		total, refresh, static, savings)
}

// RefreshScope contains the computed scope of resources for refresh.
type RefreshScope struct {
	// Strategy used to compute this scope
	Strategy RefreshStrategy

	// AllResources is the complete set of resources in the state
	AllResources []*ResourceNode

	// ToRefresh is the set of resources that need provider refresh
	ToRefresh []*ResourceNode

	// Static is the set of resources whose values can be statically injected
	Static []*ResourceNode

	// Subgraph is the computed subgraph (if applicable)
	Subgraph *Subgraph

	// Cone is the computed dependency cone (if applicable)
	Cone *DependencyCone

	// stats contains precomputed statistics
	stats RefreshStats
}

// RefreshAddresses returns the addresses that need to be refreshed.
func (s *RefreshScope) RefreshAddresses() []addrs.AbsResource {
	result := make([]addrs.AbsResource, len(s.ToRefresh))
	for i, node := range s.ToRefresh {
		result[i] = node.Addr
	}
	return result
}

// StaticAddresses returns the addresses that can be statically injected.
func (s *RefreshScope) StaticAddresses() []addrs.AbsResource {
	result := make([]addrs.AbsResource, len(s.Static))
	for i, node := range s.Static {
		result[i] = node.Addr
	}
	return result
}

// ShouldRefresh checks if a specific resource needs to be refreshed.
func (s *RefreshScope) ShouldRefresh(addr addrs.AbsResource) bool {
	key := addr.String()
	for _, node := range s.ToRefresh {
		if node.Addr.String() == key {
			return true
		}
	}
	return false
}

// IsStatic checks if a specific resource can be statically injected.
func (s *RefreshScope) IsStatic(addr addrs.AbsResource) bool {
	key := addr.String()
	for _, node := range s.Static {
		if node.Addr.String() == key {
			return true
		}
	}
	return false
}

// Stats returns optimization statistics.
func (s *RefreshScope) Stats() RefreshStats {
	if s.stats.TotalResources > 0 {
		return s.stats
	}
	// Compute on demand if not pre-computed
	stats := RefreshStats{
		TotalResources:      len(s.AllResources),
		RefreshCount:        len(s.ToRefresh),
		StaticCount:         len(s.Static),
		ResourcesInSubgraph: len(s.ToRefresh),
	}
	stats.SavingsPercent = stats.ComputeSavingsPercent()
	return stats
}

// RefreshStats contains statistics about the refresh optimization.
type RefreshStats struct {
	TotalResources      int
	RefreshCount        int
	StaticCount         int
	ResourcesInSubgraph int
	SavingsPercent      float64
}

// ComputeSavingsPercent returns the percentage of resources that don't need refresh.
func (s *RefreshStats) ComputeSavingsPercent() float64 {
	if s.TotalResources == 0 {
		return 0
	}
	return float64(s.TotalResources-s.RefreshCount) / float64(s.TotalResources) * 100
}

// String returns a human-readable representation.
func (s RefreshStats) String() string {
	return fmt.Sprintf("Refresh %d/%d resources (%.1f%% savings)",
		s.RefreshCount, s.TotalResources, s.SavingsPercent)
}

// Engine is a simplified interface to the velocity optimization system.
// This is the main entry point used by the tofu package.
type Engine struct {
	state           *states.State
	stateGraph      *StateGraph
	targetAddrs     []string
	changedFromConf []addrs.AbsResource // Resources detected as changed from config comparison
}

// NewEngine creates a new velocity Engine with the given state and target addresses.
func NewEngine(state *states.State, targetAddrs []string) *Engine {
	return &Engine{
		state:       state,
		stateGraph:  FromState(state),
		targetAddrs: targetAddrs,
	}
}

// NewEngineWithChangeDetection creates a velocity Engine that automatically
// detects which resources have configuration changes and uses those as targets.
// This is used when -velocity is enabled without -target.
func NewEngineWithChangeDetection(state *states.State, configResources []addrs.ConfigResource) *Engine {
	detector := NewConfigChangeDetector(state)
	changedResources := detector.DetectChanges(configResources)

	// Convert AbsResource to string addresses
	targetAddrs := make([]string, len(changedResources))
	for i, res := range changedResources {
		targetAddrs[i] = res.String()
	}

	return &Engine{
		state:           state,
		stateGraph:      FromState(state),
		targetAddrs:     targetAddrs,
		changedFromConf: changedResources,
	}
}

// NewEngineWithAttributeDetection creates a velocity Engine that detects
// attribute-level changes by evaluating static expressions (literals, variables,
// locals) and comparing them with state values. This provides more accurate
// change detection than structural-only detection.
func NewEngineWithAttributeDetection(
	config *configs.Config,
	state *states.State,
	variables map[string]cty.Value,
) *Engine {
	// Use attribute-level change detection
	detector := NewAttributeChangeDetector(config, state, variables)
	changedResources := detector.DetectChangedResources()

	// Convert AbsResource to string addresses
	targetAddrs := make([]string, len(changedResources))
	for i, res := range changedResources {
		targetAddrs[i] = res.String()
	}

	log.Printf("[DEBUG] Velocity: Attribute detection found %d changed resources", len(changedResources))

	return &Engine{
		state:           state,
		stateGraph:      FromState(state),
		targetAddrs:     targetAddrs,
		changedFromConf: changedResources,
	}
}

// ComputeRefreshScope calculates which resources need to be refreshed based on
// the refresh strategy and static injection settings.
func (e *Engine) ComputeRefreshScope(strategy RefreshStrategy, staticInjection bool) *RefreshScope {
	scope := &RefreshScope{
		Strategy:     strategy,
		AllResources: e.stateGraph.AllNodes(),
	}

	// Convert string addresses to AbsResource
	targetAddrs := e.parseTargetAddrs()

	switch strategy {
	case RefreshStrategyFull:
		// Full refresh - all resources
		scope.ToRefresh = scope.AllResources
		scope.Static = nil

	case RefreshStrategyTargeted:
		// Compute minimal subgraph based on targets
		if len(targetAddrs) == 0 {
			// No targets means refresh everything
			scope.ToRefresh = scope.AllResources
		} else {
			subgraph := e.stateGraph.ComputeMinimalSubgraph(targetAddrs)
			scope.ToRefresh = subgraph.AllNodes()
			scope.Subgraph = subgraph
		}

	case RefreshStrategyOptimized:
		// Use change detection and static injection
		if len(targetAddrs) == 0 {
			// No targets - get all resource addresses
			for _, node := range e.stateGraph.nodes {
				targetAddrs = append(targetAddrs, node.Addr)
			}
		}

		// For optimized strategy, compute the change cone
		cone := e.stateGraph.ComputeChangeCone(targetAddrs, nil)
		scope.Cone = cone

		if staticInjection {
			scope.ToRefresh = cone.RequiresRefresh
			scope.Static = cone.StaticInjectable
		} else {
			scope.ToRefresh = append(cone.RequiresRefresh, cone.StaticInjectable...)
		}

		// Also store the subgraph for ordering
		scope.Subgraph = e.stateGraph.ComputeMinimalSubgraphWithCone(cone)
	}

	// Compute stats
	stats := &RefreshStats{
		TotalResources:      len(scope.AllResources),
		RefreshCount:        len(scope.ToRefresh),
		StaticCount:         len(scope.Static),
		ResourcesInSubgraph: len(scope.ToRefresh),
	}
	stats.SavingsPercent = stats.ComputeSavingsPercent()
	scope.stats = *stats

	return scope
}

// parseTargetAddrs converts string target addresses to AbsResource.
func (e *Engine) parseTargetAddrs() []addrs.AbsResource {
	result := make([]addrs.AbsResource, 0, len(e.targetAddrs))

	for _, target := range e.targetAddrs {
		// Look up the resource in the state graph by string match
		for _, node := range e.stateGraph.nodes {
			if node.Addr.String() == target {
				result = append(result, node.Addr)
				break
			}
		}
	}

	return result
}
