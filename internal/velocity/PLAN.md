# OpenTofu Velocity Optimization - Implementation Plan

## Overview

This document describes the implementation of Stategraph's "Velocity" optimizations for OpenTofu. The goal is to transform O(n) refresh operations into O(subgraph) by only refreshing resources that are actually affected by configuration changes.

## Problem Statement

Currently, OpenTofu:
1. Loads the entire state file (monolithic)
2. Refreshes ALL resources on every plan/apply
3. Locks the entire state for any operation

This is inefficient for large infrastructure with hundreds/thousands of resources.

## Solution: Velocity Optimizations

### 1. In-Memory DAG Representation (`StateGraph`)

**Location:** `internal/velocity/state_graph.go`

Instead of treating state as a flat list, we convert it to a Directed Acyclic Graph (DAG):

```go
type StateGraph struct {
    graph *dag.AcyclicGraph
    nodes map[string]*ResourceNode
    state *states.State
}

// Convert state to graph
graph := velocity.FromState(state)
```

Each resource becomes a node with:
- Upstream dependencies (what it depends on)
- Downstream dependents (what depends on it)

### 2. Dependency Coning Algorithm

**Location:** `internal/velocity/dependency_cone.go`

The "cone" concept from Stategraph:

#### Top Cone (Upstream Dependencies)
```go
// Get all resources that target depends on
topCone := graph.ComputeTopCone([]addrs.AbsResource{target})
```

#### Bottom Cone (Downstream Dependents)
```go
// Get all resources that depend on target (blast radius)
bottomCone := graph.ComputeBottomCone([]addrs.AbsResource{target})
```

#### Change Cone (Full Impact Analysis)
```go
// Compute which resources need refresh vs static injection
cone := graph.ComputeChangeCone(targets, changedResources)
// cone.RequiresRefresh - resources needing provider refresh
// cone.StaticInjectable - resources whose values can be injected from cache
```

### 3. Subgraph Execution

**Location:** `internal/velocity/subgraph.go`

For targeted operations, we compute the minimal subgraph:

```go
// Only include resources relevant to the operation
subgraph := graph.ComputeMinimalSubgraph(targetAddrs)

// Get reduction statistics
reduction := subgraph.Reduction(graph.Size())
fmt.Printf("Processing %d/%d resources (%.1f%% savings)\n",
    reduction.SubgraphResources, reduction.TotalResources, reduction.Savings())
```

### 4. Granular Locking Interface

**Location:** `internal/velocity/lock_manager.go`

Abstract interface for resource-level locking:

```go
type ResourceLockManager interface {
    LockResources(ctx context.Context, resources []addrs.AbsResource, info *ResourceLockInfo) (LockHandle, error)
    Granularity() LockGranularity
    SupportsParallelOperations() bool
}
```

**Implementations:**
- `GlobalFallbackLockManager` - Adapter for existing backends (S3, Local, GCS)
- `NoOpLockManager` - For testing or when locking is disabled
- Future: `PostgresLockManager` - True row-level locking

### 5. VelocityEngine (Main Entry Point)

**Location:** `internal/velocity/velocity.go`

```go
engine := velocity.NewVelocityEngine(state, &velocity.EngineOpts{
    Strategy:              velocity.RefreshStrategyOptimized,
    EnableStaticInjection: true,
})

scope, err := engine.ComputeRefreshScope(ctx, targets, changedResources)
// scope.ToRefresh - resources that need provider refresh
// scope.Static - resources that can use cached values
```

## UX Considerations

### CLI Output Enhancement

When velocity optimizations are active, show users the savings:

```
Refreshing state... [Velocity: 15/200 resources (92.5% savings)]
  aws_instance.web: Refreshing...
  aws_security_group.web: Using cached state (no changes detected)
```

### New Flags

```bash
# Force full refresh (disable optimizations)
tofu plan --refresh-strategy=full

# Show optimization details
tofu plan --verbose-refresh
```

### Diagnostic Commands

```bash
# Show dependency graph
tofu graph --velocity

# Show what would be refreshed
tofu plan --dry-run-refresh
```

## Integration Points

### With `tofu.Context.Plan()`

The `VelocityEngine.ComputeRefreshScope()` should be called at the start of Plan:

```go
func (c *Context) Plan(...) (*plans.Plan, tfdiags.Diagnostics) {
    // NEW: Compute refresh scope using velocity
    engine := velocity.NewVelocityEngine(prevRunState, velocityOpts)
    scope, _ := engine.ComputeRefreshScope(ctx, opts.Targets, changedResources)
    
    // Only refresh resources in scope.ToRefresh
    // Use cached values for resources in scope.Static
}
```

### With Locking

```go
// Adapt existing statemgr.Locker to ResourceLockManager
lockManager := velocity.ResourceLockManagerAdapter(stateManager)

// Lock only the resources we're operating on
handle, _ := lockManager.LockResources(ctx, scope.RefreshAddresses(), lockInfo)
defer handle.Unlock(ctx)
```

## Package Structure

```
internal/velocity/
├── doc.go                 # Package documentation
├── resource_node.go       # ResourceNode type
├── state_graph.go         # StateGraph DAG implementation
├── dependency_cone.go     # Cone calculation algorithms
├── subgraph.go            # Subgraph extraction
├── lock_manager.go        # Locking abstractions
├── velocity.go            # VelocityEngine main entry point
└── velocity_test.go       # Comprehensive tests
```

## Remaining Work

### Phase 3.3: Integration with Refresh Cycle
- Modify `tofu/context_plan.go` to use VelocityEngine
- Add configuration options to enable/disable optimizations
- Add metrics/logging for optimization stats

### Phase 5.1: Full Integration
- Wire up VelocityEngine in the Plan/Apply workflow
- Handle edge cases (orphaned resources, imports, etc.)
- Add integration tests

### Phase 5.3: Regression Testing
- Run full OpenTofu test suite
- Verify backward compatibility
- Performance benchmarks

## Testing

All unit tests pass:

```
=== RUN   TestStateGraphFromState
=== RUN   TestResourceNodeDependencies
=== RUN   TestComputeTopCone
=== RUN   TestComputeBottomCone
=== RUN   TestComputeMinimalSubgraph
=== RUN   TestComputeChangeCone
=== RUN   TestSubgraphReduction
=== RUN   TestVelocityEngine
=== RUN   TestGlobalFallbackLockManager
=== RUN   TestNoOpLockManager
PASS
```

## Performance Expectations

For a state with N resources where only M are affected:
- **Before:** O(N) refresh operations
- **After:** O(M) refresh operations + O(N) graph construction (one-time)

Example: 200 resources, changing 1 EC2 instance
- Traditional: Refresh 200 resources
- Velocity: Refresh ~5-15 resources (instance + direct dependencies/dependents)
- Savings: 90-95%
