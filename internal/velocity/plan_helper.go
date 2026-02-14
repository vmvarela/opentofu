// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"context"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/states"
)

// PlanHelper provides velocity optimizations for the planning phase.
// It integrates with the existing tofu.Context.Plan() workflow.
type PlanHelper struct {
	engine *VelocityEngine
	scope  *RefreshScope
	filter *RefreshFilter
}

// PlanHelperOpts configures the PlanHelper.
type PlanHelperOpts struct {
	// Enabled activates velocity optimizations
	Enabled bool

	// Strategy determines refresh calculation method
	Strategy RefreshStrategy

	// EnableStaticInjection allows skipping refresh for unchanged deps
	EnableStaticInjection bool
}

// DefaultPlanHelperOpts returns default options (optimizations enabled).
func DefaultPlanHelperOpts() *PlanHelperOpts {
	return &PlanHelperOpts{
		Enabled:               true,
		Strategy:              RefreshStrategyTargeted,
		EnableStaticInjection: true,
	}
}

// NewPlanHelper creates a PlanHelper for the given state.
// Returns nil if state is nil or empty.
func NewPlanHelper(state *states.State, opts *PlanHelperOpts) *PlanHelper {
	if opts == nil || !opts.Enabled {
		return nil
	}

	if state == nil || state.Empty() {
		return nil
	}

	engine := NewVelocityEngine(state, &EngineOpts{
		Strategy:              opts.Strategy,
		EnableStaticInjection: opts.EnableStaticInjection,
	})

	return &PlanHelper{
		engine: engine,
	}
}

// ComputeRefreshScope calculates which resources need to be refreshed
// based on the targets and detected changes.
func (h *PlanHelper) ComputeRefreshScope(
	ctx context.Context,
	targets []addrs.Targetable,
	changedResources []addrs.AbsResource,
) error {
	if h == nil {
		return nil
	}

	scope, err := h.engine.ComputeRefreshScope(ctx, targets, changedResources)
	if err != nil {
		return err
	}

	h.scope = scope
	h.filter = NewRefreshFilter(scope)

	// Log optimization stats
	stats := scope.Stats()
	log.Printf("[INFO] Velocity optimization: refresh %d/%d resources (%.1f%% savings)",
		stats.RefreshCount, stats.TotalResources, stats.SavingsPercent())

	return nil
}

// GetRefreshFilter returns a filter for determining which resources to refresh.
func (h *PlanHelper) GetRefreshFilter() *RefreshFilter {
	if h == nil {
		return DisabledFilter()
	}
	return h.filter
}

// GetScope returns the computed refresh scope.
func (h *PlanHelper) GetScope() *RefreshScope {
	if h == nil {
		return nil
	}
	return h.scope
}

// ShouldRefreshResource checks if a specific resource should be refreshed.
func (h *PlanHelper) ShouldRefreshResource(addr addrs.AbsResource) bool {
	if h == nil || h.filter == nil {
		return true // Default: refresh everything
	}
	return h.filter.ShouldRefresh(addr)
}

// ShouldRefreshInstance checks if a specific instance should be refreshed.
func (h *PlanHelper) ShouldRefreshInstance(addr addrs.AbsResourceInstance) bool {
	if h == nil || h.filter == nil {
		return true
	}
	return h.filter.ShouldRefreshInstance(addr)
}

// GetOptimizedTargets returns an optimized set of targets based on the
// computed dependency cone. This can be used to limit the scope of the plan.
//
// If there are no explicit targets, this returns the resources that need
// refresh (avoiding full-state operations).
func (h *PlanHelper) GetOptimizedTargets(originalTargets []addrs.Targetable) []addrs.Targetable {
	if h == nil || h.scope == nil {
		return originalTargets
	}

	// If there are already targets, respect them
	if len(originalTargets) > 0 {
		return originalTargets
	}

	// No optimization if everything needs refresh
	if h.scope.Stats().SavingsPercent() < 10 {
		return originalTargets
	}

	// Convert refresh addresses to targets
	// Note: This is an aggressive optimization that might not be desired
	// in all cases, so we return original targets by default
	return originalTargets
}

// Stats returns optimization statistics.
func (h *PlanHelper) Stats() RefreshStats {
	if h == nil || h.scope == nil {
		return RefreshStats{}
	}
	return h.scope.Stats()
}

// LogSummary logs a summary of the velocity optimization.
func (h *PlanHelper) LogSummary() {
	if h == nil || h.scope == nil {
		return
	}

	stats := h.scope.Stats()
	if stats.TotalResources == 0 {
		return
	}

	log.Printf("[INFO] Velocity: Total=%d, Refresh=%d, Static=%d, Savings=%.1f%%",
		stats.TotalResources, stats.RefreshCount, stats.StaticCount, stats.SavingsPercent())
}

// DetectChangedResources compares the configuration with the state to
// identify resources that have configuration changes.
// This is a placeholder - the actual implementation would need access
// to the configuration diff.
func DetectChangedResources(
	state *states.State,
	configResourceAddrs []addrs.ConfigResource,
) []addrs.AbsResource {
	// TODO: Implement actual change detection by comparing config with state
	// For now, return empty slice (no changes detected = maximum optimization)
	return nil
}
