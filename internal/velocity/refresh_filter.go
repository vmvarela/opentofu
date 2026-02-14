// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"github.com/opentofu/opentofu/internal/addrs"
)

// RefreshFilter determines whether a specific resource should be refreshed.
// This is used to integrate velocity optimizations with the existing refresh logic.
type RefreshFilter struct {
	// enabled indicates if velocity filtering is active
	enabled bool

	// refreshSet contains resource addresses that need refresh
	refreshSet map[string]bool

	// staticSet contains resource addresses that can use cached values
	staticSet map[string]bool

	// stats tracks refresh optimization statistics
	stats RefreshStats
}

// NewRefreshFilter creates a RefreshFilter from a RefreshScope.
// If scope is nil, returns a filter that allows all refreshes.
func NewRefreshFilter(scope *RefreshScope) *RefreshFilter {
	if scope == nil {
		return &RefreshFilter{
			enabled:    false,
			refreshSet: nil,
			staticSet:  nil,
		}
	}

	filter := &RefreshFilter{
		enabled:    true,
		refreshSet: make(map[string]bool),
		staticSet:  make(map[string]bool),
		stats:      scope.Stats(),
	}

	for _, node := range scope.ToRefresh {
		filter.refreshSet[node.Addr.String()] = true
	}

	for _, node := range scope.Static {
		filter.staticSet[node.Addr.String()] = true
	}

	return filter
}

// DisabledFilter returns a filter that doesn't filter anything (all resources refresh).
func DisabledFilter() *RefreshFilter {
	return &RefreshFilter{
		enabled: false,
	}
}

// ShouldRefresh returns true if the given resource should be refreshed.
// If velocity filtering is disabled, always returns true.
func (f *RefreshFilter) ShouldRefresh(addr addrs.AbsResource) bool {
	if !f.enabled {
		return true
	}
	return f.refreshSet[addr.String()]
}

// ShouldRefreshInstance returns true if the given resource instance should be refreshed.
func (f *RefreshFilter) ShouldRefreshInstance(addr addrs.AbsResourceInstance) bool {
	return f.ShouldRefresh(addr.ContainingResource())
}

// IsStatic returns true if the resource can use cached values.
func (f *RefreshFilter) IsStatic(addr addrs.AbsResource) bool {
	if !f.enabled {
		return false
	}
	return f.staticSet[addr.String()]
}

// IsEnabled returns true if velocity filtering is active.
func (f *RefreshFilter) IsEnabled() bool {
	return f.enabled
}

// Stats returns the refresh optimization statistics.
func (f *RefreshFilter) Stats() RefreshStats {
	return f.stats
}

// AddToRefresh adds an address to the refresh set.
// This is useful for dynamic additions (e.g., imported resources).
func (f *RefreshFilter) AddToRefresh(addr addrs.AbsResource) {
	if f.refreshSet == nil {
		f.refreshSet = make(map[string]bool)
	}
	f.refreshSet[addr.String()] = true
	// Remove from static if present
	delete(f.staticSet, addr.String())
}

// RefreshCount returns the number of resources that will be refreshed.
func (f *RefreshFilter) RefreshCount() int {
	return len(f.refreshSet)
}

// StaticCount returns the number of resources using cached values.
func (f *RefreshFilter) StaticCount() int {
	return len(f.staticSet)
}
