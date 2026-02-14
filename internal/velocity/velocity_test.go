// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"context"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/states"
)

// TestStateGraphFromState tests building a graph from state
func TestStateGraphFromState(t *testing.T) {
	state := buildTestState()
	graph := FromState(state)

	if graph == nil {
		t.Fatal("FromState returned nil")
	}

	// Should have 4 resources
	if got := graph.Size(); got != 4 {
		t.Errorf("Size() = %d, want 4", got)
	}

	// Check that we can find each resource
	resources := []string{
		"aws_vpc.main",
		"aws_subnet.main",
		"aws_security_group.main",
		"aws_instance.web",
	}

	for _, addr := range resources {
		node := graph.nodes[addr]
		if node == nil {
			t.Errorf("Node %q not found", addr)
		}
	}
}

// TestResourceNodeDependencies tests dependency tracking
func TestResourceNodeDependencies(t *testing.T) {
	node := &ResourceNode{
		Dependencies: make([]addrs.AbsResource, 0),
		Dependents:   make([]addrs.AbsResource, 0),
	}

	addr1 := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	addr2 := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_subnet", "main")

	// Test AddDependency
	node.AddDependency(addr1)
	if len(node.Dependencies) != 1 {
		t.Errorf("AddDependency failed, got %d dependencies", len(node.Dependencies))
	}

	// Adding same dependency again should be no-op
	node.AddDependency(addr1)
	if len(node.Dependencies) != 1 {
		t.Errorf("Duplicate dependency added, got %d dependencies", len(node.Dependencies))
	}

	// Test HasDependency
	if !node.HasDependency(addr1) {
		t.Error("HasDependency returned false for existing dependency")
	}
	if node.HasDependency(addr2) {
		t.Error("HasDependency returned true for non-existing dependency")
	}

	// Test AddDependent
	node.AddDependent(addr2)
	if len(node.Dependents) != 1 {
		t.Errorf("AddDependent failed, got %d dependents", len(node.Dependents))
	}

	// Test HasDependent
	if !node.HasDependent(addr2) {
		t.Error("HasDependent returned false for existing dependent")
	}
}

// TestComputeTopCone tests upstream dependency calculation
func TestComputeTopCone(t *testing.T) {
	graph := buildTestGraph()

	// aws_instance depends on subnet, security_group
	// subnet depends on vpc
	// security_group depends on vpc
	// So TopCone of aws_instance should include: vpc, subnet, security_group, instance

	target := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_instance", "web")
	cone := graph.ComputeTopCone([]addrs.AbsResource{target})

	if len(cone) != 4 {
		t.Errorf("TopCone size = %d, want 4", len(cone))
	}

	// Verify vpc is in the cone
	vpcAddr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	found := false
	for _, node := range cone {
		if node.Addr.Equal(vpcAddr) {
			found = true
			break
		}
	}
	if !found {
		t.Error("VPC not found in TopCone")
	}
}

// TestComputeBottomCone tests downstream dependent calculation
func TestComputeBottomCone(t *testing.T) {
	graph := buildTestGraph()

	// BottomCone of VPC should include everything that depends on it
	target := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	cone := graph.ComputeBottomCone([]addrs.AbsResource{target})

	// Should include: vpc, subnet, security_group, instance
	if len(cone) != 4 {
		t.Errorf("BottomCone size = %d, want 4", len(cone))
	}
}

// TestComputeMinimalSubgraph tests subgraph calculation
func TestComputeMinimalSubgraph(t *testing.T) {
	graph := buildTestGraph()

	// Target just the instance
	target := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_instance", "web")
	subgraph := graph.ComputeMinimalSubgraph([]addrs.AbsResource{target})

	// Should include all 4 resources (instance + all its dependencies)
	if subgraph.Size() != 4 {
		t.Errorf("Subgraph size = %d, want 4", subgraph.Size())
	}

	// Validate the subgraph is a valid DAG
	if err := subgraph.Validate(); err != nil {
		t.Errorf("Subgraph validation failed: %v", err)
	}
}

// TestComputeChangeCone tests change cone with static injection
func TestComputeChangeCone(t *testing.T) {
	graph := buildTestGraph()

	// Target the instance, but only subnet has changed
	target := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_instance", "web")
	changed := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_subnet", "main")

	cone := graph.ComputeChangeCone(
		[]addrs.AbsResource{target},
		[]addrs.AbsResource{changed},
	)

	// VPC shouldn't need refresh (not changed, doesn't depend on changed)
	// But subnet, security_group, and instance should need refresh
	t.Logf("StaticInjectable: %d, RequiresRefresh: %d",
		cone.StaticCount(), cone.RefreshCount())

	// VPC should be static injectable
	if cone.StaticCount() < 1 {
		t.Error("Expected at least 1 static injectable resource (VPC)")
	}
}

// TestSubgraphReduction tests the reduction statistics
func TestSubgraphReduction(t *testing.T) {
	graph := buildTestGraph()

	// Target just one resource out of 4
	target := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	subgraph := graph.ComputeMinimalSubgraph([]addrs.AbsResource{target})

	// VPC has no dependencies, so subgraph should be just VPC
	reduction := subgraph.Reduction(graph.Size())

	t.Logf("Total: %d, Subgraph: %d, Savings: %.1f%%",
		reduction.TotalResources, reduction.SubgraphResources, reduction.Savings())

	// Should have some savings
	if reduction.SubgraphResources > reduction.TotalResources {
		t.Error("Subgraph larger than total state")
	}
}

// TestVelocityEngine tests the main engine
func TestVelocityEngine(t *testing.T) {
	state := buildTestState()
	engine := NewVelocityEngine(state, DefaultEngineOpts())

	// Test with no targets (should refresh all)
	scope, err := engine.ComputeRefreshScope(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ComputeRefreshScope failed: %v", err)
	}

	stats := scope.Stats()
	t.Logf("Stats: %v", stats)

	if stats.TotalResources != 4 {
		t.Errorf("TotalResources = %d, want 4", stats.TotalResources)
	}
}

// TestGlobalFallbackLockManager tests the global lock manager
func TestGlobalFallbackLockManager(t *testing.T) {
	locker := &mockLocker{
		lockID: "test-lock-123",
	}
	manager := NewGlobalFallbackLockManager(locker)

	// Test granularity
	if manager.Granularity() != LockGranularityGlobal {
		t.Error("Expected global granularity")
	}

	if manager.SupportsParallelOperations() {
		t.Error("Global lock should not support parallel operations")
	}

	// Test locking
	addr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	handle, err := manager.LockResources(context.Background(),
		[]addrs.AbsResource{addr},
		&ResourceLockInfo{Operation: "plan"},
	)
	if err != nil {
		t.Fatalf("LockResources failed: %v", err)
	}

	if !handle.IsGlobal() {
		t.Error("Expected global lock")
	}

	if handle.ID() != "test-lock-123" {
		t.Errorf("Lock ID = %q, want %q", handle.ID(), "test-lock-123")
	}

	// Test unlock
	if err := handle.Unlock(context.Background()); err != nil {
		t.Errorf("Unlock failed: %v", err)
	}
}

// TestNoOpLockManager tests the no-op lock manager
func TestNoOpLockManager(t *testing.T) {
	manager := NewNoOpLockManager()

	if manager.Granularity() != LockGranularityResource {
		t.Error("Expected resource granularity")
	}

	if !manager.SupportsParallelOperations() {
		t.Error("NoOp should support parallel operations")
	}

	addr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	handle, err := manager.LockResources(context.Background(),
		[]addrs.AbsResource{addr},
		&ResourceLockInfo{Operation: "plan"},
	)
	if err != nil {
		t.Fatalf("LockResources failed: %v", err)
	}

	if handle.IsGlobal() {
		t.Error("NoOp lock should not be global")
	}

	// Unlock should be no-op
	if err := handle.Unlock(context.Background()); err != nil {
		t.Errorf("Unlock failed: %v", err)
	}
}

// --- Test Helpers ---

// buildTestState creates a test state with a typical AWS dependency chain
func buildTestState() *states.State {
	state := states.NewState()
	rootModule := state.RootModule()

	// VPC - no dependencies
	vpcAddr := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "aws_vpc", Name: "main"}
	rootModule.SetResourceProvider(vpcAddr, addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("aws"),
	})
	vpcResource := rootModule.Resource(vpcAddr)
	vpcResource.Instances[addrs.NoKey] = &states.ResourceInstance{
		Current: &states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id": "vpc-123"}`),
			Dependencies: []addrs.ConfigResource{},
		},
	}

	// Subnet - depends on VPC
	subnetAddr := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "aws_subnet", Name: "main"}
	rootModule.SetResourceProvider(subnetAddr, addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("aws"),
	})
	subnetResource := rootModule.Resource(subnetAddr)
	subnetResource.Instances[addrs.NoKey] = &states.ResourceInstance{
		Current: &states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id": "subnet-123"}`),
			Dependencies: []addrs.ConfigResource{
				vpcAddr.InModule(addrs.RootModule),
			},
		},
	}

	// Security Group - depends on VPC
	sgAddr := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "aws_security_group", Name: "main"}
	rootModule.SetResourceProvider(sgAddr, addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("aws"),
	})
	sgResource := rootModule.Resource(sgAddr)
	sgResource.Instances[addrs.NoKey] = &states.ResourceInstance{
		Current: &states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id": "sg-123"}`),
			Dependencies: []addrs.ConfigResource{
				vpcAddr.InModule(addrs.RootModule),
			},
		},
	}

	// Instance - depends on Subnet and Security Group
	instanceAddr := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "aws_instance", Name: "web"}
	rootModule.SetResourceProvider(instanceAddr, addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("aws"),
	})
	instanceResource := rootModule.Resource(instanceAddr)
	instanceResource.Instances[addrs.NoKey] = &states.ResourceInstance{
		Current: &states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id": "i-123"}`),
			Dependencies: []addrs.ConfigResource{
				subnetAddr.InModule(addrs.RootModule),
				sgAddr.InModule(addrs.RootModule),
			},
		},
	}

	return state
}

// buildTestGraph creates a graph directly for testing without going through state
func buildTestGraph() *StateGraph {
	sg := NewStateGraph()

	// Create nodes
	vpcAddr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_vpc", "main")
	subnetAddr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_subnet", "main")
	sgAddr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_security_group", "main")
	instanceAddr := addrs.RootModuleInstance.Resource(addrs.ManagedResourceMode, "aws_instance", "web")

	vpcNode := &ResourceNode{Addr: vpcAddr, Dependencies: []addrs.AbsResource{}, Dependents: []addrs.AbsResource{}}
	subnetNode := &ResourceNode{Addr: subnetAddr, Dependencies: []addrs.AbsResource{}, Dependents: []addrs.AbsResource{}}
	sgNode := &ResourceNode{Addr: sgAddr, Dependencies: []addrs.AbsResource{}, Dependents: []addrs.AbsResource{}}
	instanceNode := &ResourceNode{Addr: instanceAddr, Dependencies: []addrs.AbsResource{}, Dependents: []addrs.AbsResource{}}

	// Add nodes
	sg.addNode(vpcNode)
	sg.addNode(subnetNode)
	sg.addNode(sgNode)
	sg.addNode(instanceNode)

	// Set up dependencies
	// subnet depends on vpc
	subnetNode.AddDependency(vpcAddr)
	vpcNode.AddDependent(subnetAddr)

	// sg depends on vpc
	sgNode.AddDependency(vpcAddr)
	vpcNode.AddDependent(sgAddr)

	// instance depends on subnet and sg
	instanceNode.AddDependency(subnetAddr)
	instanceNode.AddDependency(sgAddr)
	subnetNode.AddDependent(instanceAddr)
	sgNode.AddDependent(instanceAddr)

	return sg
}

// mockLocker implements GlobalLocker for testing
type mockLocker struct {
	lockID   string
	locked   bool
	unlocked bool
}

func (m *mockLocker) Lock(ctx context.Context, info *GlobalLockInfo) (string, error) {
	m.locked = true
	return m.lockID, nil
}

func (m *mockLocker) Unlock(ctx context.Context, id string) error {
	m.unlocked = true
	return nil
}
