// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package velocity implements performance optimizations for OpenTofu operations
// inspired by Stategraph's "Velocity" approach.
//
// Key concepts:
//   - StateGraph: In-memory DAG representation of state resources
//   - Dependency Coning: Calculate minimal affected resources for changes
//   - Subgraph Execution: Execute operations on minimal resource subset
//   - Granular Locking: Interface for resource-level locking (future backends)
//
// This package transforms O(n) refresh operations into O(subgraph) by only
// refreshing resources that are actually affected by configuration changes.
package velocity
