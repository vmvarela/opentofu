// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"context"
	"sync"

	"github.com/opentofu/opentofu/internal/addrs"
)

// LockGranularity indicates the level of granularity a lock manager supports.
type LockGranularity int

const (
	// LockGranularityGlobal means the entire state is locked as one unit.
	// This is the fallback for backends like S3 and Local.
	LockGranularityGlobal LockGranularity = iota

	// LockGranularityResource means individual resources can be locked.
	// This enables concurrent operations on non-overlapping resource sets.
	// Future backends like Postgres could implement this.
	LockGranularityResource
)

// ResourceLockManager provides an abstraction for resource-level locking.
// This interface allows different backends to implement varying levels of
// lock granularity while maintaining a consistent API.
//
// Current backends (S3, Local, GCS) only support global locking, so they
// use GlobalFallbackLockManager which implements this interface but locks
// the entire state regardless of which resources are requested.
//
// Future backends (e.g., Postgres) could implement true resource-level
// locking by acquiring row-level locks only for the specified resources.
type ResourceLockManager interface {
	// LockResources acquires locks for the given set of resource addresses.
	// The implementation may lock more than requested (e.g., global lock).
	// Returns a LockHandle that must be used to release the locks.
	LockResources(ctx context.Context, resources []addrs.AbsResource, info *ResourceLockInfo) (LockHandle, error)

	// Granularity returns the level of lock granularity this manager supports.
	Granularity() LockGranularity

	// SupportsParallelOperations returns true if this manager allows
	// concurrent operations on non-overlapping resource sets.
	SupportsParallelOperations() bool
}

// LockHandle represents an acquired lock that can be released.
type LockHandle interface {
	// Unlock releases the lock(s) held by this handle.
	Unlock(ctx context.Context) error

	// LockedResources returns the actual resources that are locked.
	// For global locks, this returns nil (indicating all resources).
	LockedResources() []addrs.AbsResource

	// IsGlobal returns true if this is a global lock (all resources).
	IsGlobal() bool

	// ID returns a unique identifier for this lock.
	ID() string
}

// ResourceLockInfo contains metadata about the lock request.
type ResourceLockInfo struct {
	// Operation describes what operation is being performed
	Operation string

	// Who identifies the user/process requesting the lock
	Who string

	// Info contains additional context about the lock
	Info string

	// Resources contains the addresses being locked
	Resources []addrs.AbsResource
}

// GlobalFallbackLockManager implements ResourceLockManager but always
// acquires a global lock regardless of which resources are requested.
// This is the adapter for existing backends that only support global locking.
type GlobalFallbackLockManager struct {
	// locker is the underlying global lock mechanism
	locker GlobalLocker

	// mu protects concurrent access to lock state
	mu sync.Mutex

	// activeLock holds the current lock handle if one is held
	activeLock *globalLockHandle
}

// GlobalLocker is the interface that existing backends implement.
// This matches the signature of statemgr.Locker.
type GlobalLocker interface {
	Lock(ctx context.Context, info *GlobalLockInfo) (string, error)
	Unlock(ctx context.Context, id string) error
}

// GlobalLockInfo matches statemgr.LockInfo structure.
type GlobalLockInfo struct {
	ID        string
	Operation string
	Info      string
	Who       string
}

// NewGlobalFallbackLockManager creates a new GlobalFallbackLockManager.
func NewGlobalFallbackLockManager(locker GlobalLocker) *GlobalFallbackLockManager {
	return &GlobalFallbackLockManager{
		locker: locker,
	}
}

// LockResources acquires a global lock. The resources parameter is ignored
// because this manager only supports global locking.
func (m *GlobalFallbackLockManager) LockResources(ctx context.Context, resources []addrs.AbsResource, info *ResourceLockInfo) (LockHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Convert to global lock info
	globalInfo := &GlobalLockInfo{
		Operation: info.Operation,
		Info:      info.Info,
		Who:       info.Who,
	}

	id, err := m.locker.Lock(ctx, globalInfo)
	if err != nil {
		return nil, err
	}

	handle := &globalLockHandle{
		manager:   m,
		id:        id,
		resources: resources,
	}

	m.activeLock = handle
	return handle, nil
}

// Granularity returns LockGranularityGlobal.
func (m *GlobalFallbackLockManager) Granularity() LockGranularity {
	return LockGranularityGlobal
}

// SupportsParallelOperations returns false because global locking
// prevents concurrent operations.
func (m *GlobalFallbackLockManager) SupportsParallelOperations() bool {
	return false
}

// globalLockHandle implements LockHandle for global locks.
type globalLockHandle struct {
	manager   *GlobalFallbackLockManager
	id        string
	resources []addrs.AbsResource
}

// Unlock releases the global lock.
func (h *globalLockHandle) Unlock(ctx context.Context) error {
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()

	err := h.manager.locker.Unlock(ctx, h.id)
	if err == nil {
		h.manager.activeLock = nil
	}
	return err
}

// LockedResources returns nil for global locks (all resources are locked).
func (h *globalLockHandle) LockedResources() []addrs.AbsResource {
	return nil // Global lock - all resources
}

// IsGlobal returns true.
func (h *globalLockHandle) IsGlobal() bool {
	return true
}

// ID returns the lock identifier.
func (h *globalLockHandle) ID() string {
	return h.id
}

// ResourceLockManagerAdapter creates a ResourceLockManager from an existing
// GlobalLocker (like statemgr.Locker). This is the bridge between the new
// velocity package and existing OpenTofu locking infrastructure.
func ResourceLockManagerAdapter(locker GlobalLocker) ResourceLockManager {
	return NewGlobalFallbackLockManager(locker)
}

// NoOpLockManager is a lock manager that doesn't actually lock anything.
// Useful for testing or when locking is disabled.
type NoOpLockManager struct{}

// NewNoOpLockManager creates a new NoOpLockManager.
func NewNoOpLockManager() *NoOpLockManager {
	return &NoOpLockManager{}
}

// LockResources returns a no-op lock handle immediately.
func (m *NoOpLockManager) LockResources(ctx context.Context, resources []addrs.AbsResource, info *ResourceLockInfo) (LockHandle, error) {
	return &noOpLockHandle{
		resources: resources,
	}, nil
}

// Granularity returns LockGranularityResource since no actual locking occurs.
func (m *NoOpLockManager) Granularity() LockGranularity {
	return LockGranularityResource
}

// SupportsParallelOperations returns true since no locking means no contention.
func (m *NoOpLockManager) SupportsParallelOperations() bool {
	return true
}

// noOpLockHandle is a lock handle that does nothing.
type noOpLockHandle struct {
	resources []addrs.AbsResource
}

// Unlock is a no-op.
func (h *noOpLockHandle) Unlock(ctx context.Context) error {
	return nil
}

// LockedResources returns the requested resources.
func (h *noOpLockHandle) LockedResources() []addrs.AbsResource {
	return h.resources
}

// IsGlobal returns false since we "support" resource-level locking.
func (h *noOpLockHandle) IsGlobal() bool {
	return false
}

// ID returns a placeholder ID.
func (h *noOpLockHandle) ID() string {
	return "noop-lock"
}
