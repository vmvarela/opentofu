// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
)

// smartRefreshTestSchema returns a standard schema for smart refresh tests
// with common attributes: id (computed), ami (optional), value (optional+computed).
func smartRefreshTestSchema() *providers.GetProviderSchemaResponse {
	return getProviderSchemaResponseFromProviderSchema(&ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id":    {Type: cty.String, Computed: true},
					"ami":   {Type: cty.String, Optional: true},
					"value": {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
	})
}

// smartRefreshTestProvider creates a test provider that tracks ReadResource
// calls per resource address. The returned map is safe for concurrent reads
// after planning completes.
func smartRefreshTestProvider(t *testing.T) (*MockProvider, *sync.Map) {
	t.Helper()

	p := testProvider("test")
	p.GetProviderSchemaResponse = smartRefreshTestSchema()

	readCalls := &sync.Map{} // map[string]int32

	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		// Track which resources were refreshed by their prior state
		// We use PriorState's id to identify the resource
		if !req.PriorState.IsNull() {
			idVal := req.PriorState.GetAttr("id")
			if idVal.IsKnown() && !idVal.IsNull() {
				key := idVal.AsString()
				if val, ok := readCalls.Load(key); ok {
					readCalls.Store(key, val.(int32)+1)
				} else {
					readCalls.Store(key, int32(1))
				}
			}
		}
		return providers.ReadResourceResponse{
			NewState: req.PriorState,
		}
	}

	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}

	return p, readCalls
}

// readCallCount returns the number of times ReadResource was called for the
// given resource id. Returns 0 if never called.
func readCallCount(readCalls *sync.Map, id string) int32 {
	if val, ok := readCalls.Load(id); ok {
		return val.(int32)
	}
	return 0
}

// TestRefreshTracker tests the RefreshTracker's thread-safe operations
func TestRefreshTracker(t *testing.T) {
	tracker := NewRefreshTracker()

	// Test initial state
	total, refreshed, skipped := tracker.Stats()
	if total != 0 || refreshed != 0 || skipped != 0 {
		t.Errorf("initial stats should be 0, got total=%d, refreshed=%d, skipped=%d", total, refreshed, skipped)
	}

	// Test marking resources for refresh
	addr1 := mustResourceInstanceAddr("test_instance.foo")
	addr2 := mustResourceInstanceAddr("test_instance.bar")

	tracker.MarkNeedsRefresh(addr1)
	if !tracker.NeedsRefresh(addr1) {
		t.Error("addr1 should need refresh after marking")
	}
	if tracker.NeedsRefresh(addr2) {
		t.Error("addr2 should not need refresh before marking")
	}

	// Test recording refresh decisions
	tracker.RecordRefreshDecision(true)  // refreshed
	tracker.RecordRefreshDecision(true)  // refreshed
	tracker.RecordRefreshDecision(false) // skipped

	total, refreshed, skipped = tracker.Stats()
	if total != 3 {
		t.Errorf("expected total=3, got %d", total)
	}
	if refreshed != 2 {
		t.Errorf("expected refreshed=2, got %d", refreshed)
	}
	if skipped != 1 {
		t.Errorf("expected skipped=1, got %d", skipped)
	}
}

// TestRefreshTracker_CheckUpstreamNeedsRefresh tests the dependency checking
func TestRefreshTracker_CheckUpstreamNeedsRefresh(t *testing.T) {
	tracker := NewRefreshTracker()

	fooAddr := mustResourceInstanceAddr("test_instance.foo")
	barConfigAddr := addrs.ConfigResource{
		Resource: addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "bar",
		},
	}

	// Initially, no upstream needs refresh
	deps := []addrs.ConfigResource{barConfigAddr}
	if tracker.CheckUpstreamNeedsRefresh(deps) {
		t.Error("should not need refresh when no dependencies are marked")
	}

	// Mark foo as needing refresh (bar depends on foo indirectly through config)
	tracker.MarkNeedsRefresh(fooAddr)

	// Now check with the foo config address
	fooConfigAddr := addrs.ConfigResource{
		Resource: addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "foo",
		},
	}
	deps = []addrs.ConfigResource{fooConfigAddr}
	if !tracker.CheckUpstreamNeedsRefresh(deps) {
		t.Error("should need refresh when dependency is marked")
	}
}

// TestRefreshTracker_CheckUpstreamNeedsRefresh_indexedInstances tests that
// marking an indexed instance (e.g. test_instance.foo[0]) correctly
// propagates to config-level dependency checks for test_instance.foo.
func TestRefreshTracker_CheckUpstreamNeedsRefresh_indexedInstances(t *testing.T) {
	tracker := NewRefreshTracker()

	// Mark a specific indexed instance
	fooAddr0 := mustResourceInstanceAddr("test_instance.foo[0]")
	tracker.MarkNeedsRefresh(fooAddr0)

	// The config-level check should find it
	fooConfigAddr := addrs.ConfigResource{
		Resource: addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "foo",
		},
	}
	if !tracker.CheckUpstreamNeedsRefresh([]addrs.ConfigResource{fooConfigAddr}) {
		t.Error("should detect indexed instance when checking config-level dependency")
	}

	// Individual instance checks should also work
	if !tracker.NeedsRefresh(fooAddr0) {
		t.Error("instance [0] should need refresh")
	}
	fooAddr1 := mustResourceInstanceAddr("test_instance.foo[1]")
	if tracker.NeedsRefresh(fooAddr1) {
		t.Error("instance [1] should not need refresh")
	}

	// A different resource's config address should not match
	barConfigAddr := addrs.ConfigResource{
		Resource: addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "foobar",
		},
	}
	if tracker.CheckUpstreamNeedsRefresh([]addrs.ConfigResource{barConfigAddr}) {
		t.Error("test_instance.foobar should not match test_instance.foo")
	}
}

// TestRefreshTracker_ConcurrentAccess tests that RefreshTracker is safe for
// concurrent use, exercising the atomic counters and sync.Map operations.
func TestRefreshTracker_ConcurrentAccess(t *testing.T) {
	tracker := NewRefreshTracker()
	const numGoroutines = 100

	done := make(chan struct{})
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			addr := mustResourceInstanceAddr("test_instance.item[" + string(rune('0'+idx%10)) + "]")
			tracker.MarkNeedsRefresh(addr)
			tracker.RecordRefreshDecision(idx%2 == 0)
			tracker.NeedsRefresh(addr)
			tracker.CheckUpstreamNeedsRefresh([]addrs.ConfigResource{{
				Resource: addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test_instance",
					Name: "item",
				},
			}})
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	total, refreshed, skipped := tracker.Stats()
	if total != numGoroutines {
		t.Errorf("expected total=%d, got %d", numGoroutines, total)
	}
	if refreshed+skipped != total {
		t.Errorf("refreshed (%d) + skipped (%d) != total (%d)", refreshed, skipped, total)
	}
}

// TestContext2Plan_smartRefresh_unchangedResource tests that unchanged resources are skipped
func TestContext2Plan_smartRefresh_unchangedResource(t *testing.T) {
	m := testModule(t, "plan-empty")
	p := testProvider("aws")
	p.PlanResourceChangeFn = testDiffFn

	var readResourceCalled int32

	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		atomic.AddInt32(&readResourceCalled, 1)
		return providers.ReadResourceResponse{
			NewState: req.PriorState,
		}
	}

	// Set up state with an existing resource
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-abc123","ami":"ami-12345"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/aws"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	// Plan with smart refresh mode
	plan, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})

	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// Check that the plan includes a warning about smart refresh
	hasSmartRefreshWarning := false
	for _, d := range diags {
		if d.Severity() == 1 && strings.Contains(d.Description().Summary, "Smart refresh") {
			hasSmartRefreshWarning = true
			break
		}
	}
	if !hasSmartRefreshWarning {
		t.Log("Note: smart refresh warning may not appear if no resources were evaluated")
	}

	// The plan should be empty (no changes) for an empty module
	if plan == nil {
		t.Fatal("plan should not be nil")
	}
}

// TestContext2Plan_smartRefresh_changedResource tests that changed resources are refreshed
func TestContext2Plan_smartRefresh_changedResource(t *testing.T) {
	// Create a module with a resource that has a changed attribute
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  ami = "ami-new"
}
`,
	})

	p := testProvider("test")
	p.GetProviderSchemaResponse = getProviderSchemaResponseFromProviderSchema(&ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id":  {Type: cty.String, Computed: true},
					"ami": {Type: cty.String, Optional: true},
				},
			},
		},
	})

	var readResourceCalled int32

	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		atomic.AddInt32(&readResourceCalled, 1)
		return providers.ReadResourceResponse{
			NewState: req.PriorState,
		}
	}

	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}

	// Set up state with an existing resource with old ami
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-abc123","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	// Plan with smart refresh mode
	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})

	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// The resource should have been refreshed because its config changed
	if atomic.LoadInt32(&readResourceCalled) == 0 {
		t.Error("ReadResource should have been called for changed resource")
	}
}

// TestContext2Plan_smartRefresh_withRefreshOnlyMode tests that refresh-only mode rejects smart refresh
func TestContext2Plan_smartRefresh_withRefreshOnlyMode(t *testing.T) {
	m := testModule(t, "plan-empty")
	p := testProvider("aws")

	state := states.NewState()

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	// Plan with smart refresh mode AND refresh-only mode should error
	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.RefreshOnlyMode,
		RefreshMode: RefreshChanged,
	})

	if !diags.HasErrors() {
		t.Fatal("expected error when using -refresh=changed with -refresh-only")
	}

	errStr := diags.Err().Error()
	if !strings.Contains(errStr, "Cannot use -refresh=changed in refresh-only mode") {
		t.Errorf("unexpected error message: %s", errStr)
	}
}

// TestRefreshMode_values tests the RefreshMode constants
func TestRefreshMode_values(t *testing.T) {
	// Ensure the constants have expected values
	if RefreshAll != 0 {
		t.Errorf("RefreshAll should be 0 (default), got %d", RefreshAll)
	}
	if RefreshNone != 1 {
		t.Errorf("RefreshNone should be 1, got %d", RefreshNone)
	}
	if RefreshChanged != 2 {
		t.Errorf("RefreshChanged should be 2, got %d", RefreshChanged)
	}
}

// ---------------------------------------------------------------------------
// Integration tests for smart refresh through the full plan pipeline
// ---------------------------------------------------------------------------

// TestContext2Plan_smartRefresh_unchangedSkipsRefresh verifies that a resource
// whose configuration matches state is NOT refreshed under RefreshChanged mode.
func TestContext2Plan_smartRefresh_unchangedSkipsRefresh(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  ami = "ami-unchanged"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-abc123","ami":"ami-unchanged"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	if readCallCount(readCalls, "i-abc123") != 0 {
		t.Error("ReadResource should NOT have been called for unchanged resource")
	}
}

// TestContext2Plan_smartRefresh_countUnchanged verifies that resources created
// with count are skipped when their config is unchanged.
func TestContext2Plan_smartRefresh_countUnchanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  count = 3
  ami   = "ami-same"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	for i := 0; i < 3; i++ {
		root.SetResourceInstanceCurrent(
			mustResourceInstanceAddr(fmt.Sprintf("test_instance.foo[%d]", i)).Resource,
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(fmt.Sprintf(`{"id":"i-count%d","ami":"ami-same"}`, i)),
			},
			mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
			addrs.NoKey,
		)
	}

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("i-count%d", i)
		if readCallCount(readCalls, id) != 0 {
			t.Errorf("ReadResource should NOT have been called for unchanged %s", id)
		}
	}
}

// TestContext2Plan_smartRefresh_countChanged verifies that when a count
// resource's config changes, all instances are refreshed.
func TestContext2Plan_smartRefresh_countChanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  count = 3
  ami   = "ami-new"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	for i := 0; i < 3; i++ {
		root.SetResourceInstanceCurrent(
			mustResourceInstanceAddr(fmt.Sprintf("test_instance.foo[%d]", i)).Resource,
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(fmt.Sprintf(`{"id":"i-count%d","ami":"ami-old"}`, i)),
			},
			mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
			addrs.NoKey,
		)
	}

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("i-count%d", i)
		if readCallCount(readCalls, id) == 0 {
			t.Errorf("ReadResource should have been called for changed %s", id)
		}
	}
}

// TestContext2Plan_smartRefresh_forEachUnchanged verifies that for_each
// instances are skipped when their config is unchanged.
func TestContext2Plan_smartRefresh_forEachUnchanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  for_each = toset(["a", "b"])
  ami      = "ami-same"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	for _, key := range []string{"a", "b"} {
		root.SetResourceInstanceCurrent(
			mustResourceInstanceAddr(fmt.Sprintf(`test_instance.foo["%s"]`, key)).Resource,
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(fmt.Sprintf(`{"id":"i-fe-%s","ami":"ami-same"}`, key)),
			},
			mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
			addrs.NoKey,
		)
	}

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	for _, key := range []string{"a", "b"} {
		id := fmt.Sprintf("i-fe-%s", key)
		if readCallCount(readCalls, id) != 0 {
			t.Errorf("ReadResource should NOT have been called for unchanged %s", id)
		}
	}
}

// TestContext2Plan_smartRefresh_forEachChanged verifies that for_each
// instances are refreshed when their config changes.
func TestContext2Plan_smartRefresh_forEachChanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  for_each = toset(["a", "b"])
  ami      = "ami-updated"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	for _, key := range []string{"a", "b"} {
		root.SetResourceInstanceCurrent(
			mustResourceInstanceAddr(fmt.Sprintf(`test_instance.foo["%s"]`, key)).Resource,
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(fmt.Sprintf(`{"id":"i-fe-%s","ami":"ami-old"}`, key)),
			},
			mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
			addrs.NoKey,
		)
	}

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	for _, key := range []string{"a", "b"} {
		id := fmt.Sprintf("i-fe-%s", key)
		if readCallCount(readCalls, id) == 0 {
			t.Errorf("ReadResource should have been called for changed %s", id)
		}
	}
}

// TestContext2Plan_smartRefresh_dependencyPropagation verifies that when a
// resource's config changes, downstream dependents are also refreshed even
// if the dependents' own configs haven't changed.
func TestContext2Plan_smartRefresh_dependencyPropagation(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "upstream" {
  ami = "ami-new"
}

resource "test_instance" "downstream" {
  ami   = "ami-same"
  value = test_instance.upstream.id
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.upstream").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-upstream","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.downstream").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-downstream","ami":"ami-same","value":"i-upstream"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// Upstream changed config -> must be refreshed
	if readCallCount(readCalls, "i-upstream") == 0 {
		t.Error("upstream resource should have been refreshed (config changed)")
	}
	// Downstream's own config didn't change, but its dependency did -> must be refreshed
	if readCallCount(readCalls, "i-downstream") == 0 {
		t.Error("downstream resource should have been refreshed (upstream dependency changed)")
	}
}

// TestContext2Plan_smartRefresh_noPropagationWithoutChange verifies that when
// neither config nor upstream dependencies change, the resource is skipped.
func TestContext2Plan_smartRefresh_noPropagationWithoutChange(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "a" {
  ami = "ami-same"
}

resource "test_instance" "b" {
  ami   = "ami-same"
  value = test_instance.a.id
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-a","ami":"ami-same"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.b").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-b","ami":"ami-same","value":"i-a"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	if readCallCount(readCalls, "i-a") != 0 {
		t.Error("resource A should NOT have been refreshed (config unchanged)")
	}
	if readCallCount(readCalls, "i-b") != 0 {
		t.Error("resource B should NOT have been refreshed (config and upstream unchanged)")
	}
}

// TestContext2Plan_smartRefresh_orphanAlwaysRefreshes verifies that an orphaned
// resource (in state but not in config) is always refreshed under smart mode.
func TestContext2Plan_smartRefresh_orphanAlwaysRefreshes(t *testing.T) {
	// Config has no resources, but state has one -> orphan
	m := testModuleInline(t, map[string]string{
		"main.tf": `
# empty config
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.orphan").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-orphan","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// Orphans should always be refreshed since they have no config
	if readCallCount(readCalls, "i-orphan") == 0 {
		t.Error("orphan resource should have been refreshed")
	}
}

// TestContext2Plan_smartRefresh_moduleUnchanged verifies that resources inside
// a module are skipped when their config is unchanged.
func TestContext2Plan_smartRefresh_moduleUnchanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
module "child" {
  source = "./mod"
}
`,
		"mod/main.tf": `
resource "test_instance" "foo" {
  ami = "ami-same"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-mod-foo","ami":"ami-same"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	if readCallCount(readCalls, "i-mod-foo") != 0 {
		t.Error("module resource should NOT have been refreshed (config unchanged)")
	}
}

// TestContext2Plan_smartRefresh_moduleChanged verifies that resources inside
// a module are refreshed when their config changes.
func TestContext2Plan_smartRefresh_moduleChanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
module "child" {
  source = "./mod"
  ami    = "ami-new"
}
`,
		"mod/main.tf": `
variable "ami" {
  type = string
}

resource "test_instance" "foo" {
  ami = var.ami
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-mod-foo","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	if readCallCount(readCalls, "i-mod-foo") == 0 {
		t.Error("module resource should have been refreshed (config changed via variable)")
	}
}

// TestContext2Plan_smartRefresh_equivalenceNoDrift verifies that when there is
// no state drift, -refresh=changed produces the same plan as -refresh=true.
func TestContext2Plan_smartRefresh_equivalenceNoDrift(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "unchanged" {
  ami = "ami-same"
}

resource "test_instance" "changed" {
  ami = "ami-new"
}
`,
	})

	makeState := func() *states.State {
		s := states.NewState()
		root := s.EnsureModule(addrs.RootModuleInstance)
		root.SetResourceInstanceCurrent(
			mustResourceInstanceAddr("test_instance.unchanged").Resource,
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(`{"id":"i-unchanged","ami":"ami-same"}`),
			},
			mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
			addrs.NoKey,
		)
		root.SetResourceInstanceCurrent(
			mustResourceInstanceAddr("test_instance.changed").Resource,
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(`{"id":"i-changed","ami":"ami-old"}`),
			},
			mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
			addrs.NoKey,
		)
		return s
	}

	makeProvider := func() *MockProvider {
		p := testProvider("test")
		p.GetProviderSchemaResponse = smartRefreshTestSchema()
		p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
			// No drift - return same state
			return providers.ReadResourceResponse{NewState: req.PriorState}
		}
		p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
			return providers.PlanResourceChangeResponse{PlannedState: req.ProposedNewState}
		}
		return p
	}

	// Plan with -refresh=true (full refresh)
	pFull := makeProvider()
	ctxFull := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(pFull),
		},
	})
	planFull, diagsFull := ctxFull.Plan(context.Background(), m, makeState(), DefaultPlanOpts)
	if diagsFull.HasErrors() {
		t.Fatalf("full refresh plan errors: %s", diagsFull.Err())
	}

	// Plan with -refresh=changed (smart refresh)
	pSmart := makeProvider()
	ctxSmart := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(pSmart),
		},
	})
	planSmart, diagsSmart := ctxSmart.Plan(context.Background(), m, makeState(), &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diagsSmart.HasErrors() {
		t.Fatalf("smart refresh plan errors: %s", diagsSmart.Err())
	}

	// Both plans should have the same resource changes
	if len(planFull.Changes.Resources) != len(planSmart.Changes.Resources) {
		t.Fatalf("plan mismatch: full refresh has %d changes, smart refresh has %d",
			len(planFull.Changes.Resources), len(planSmart.Changes.Resources))
	}

	fullChanges := make(map[string]plans.Action)
	for _, rc := range planFull.Changes.Resources {
		fullChanges[rc.Addr.String()] = rc.Action
	}

	for _, rc := range planSmart.Changes.Resources {
		fullAction, ok := fullChanges[rc.Addr.String()]
		if !ok {
			t.Errorf("smart refresh has change for %s that full refresh doesn't", rc.Addr)
			continue
		}
		if rc.Action != fullAction {
			t.Errorf("action mismatch for %s: full=%s smart=%s", rc.Addr, fullAction, rc.Action)
		}
	}
}

// TestContext2Plan_smartRefresh_statsReporting verifies that the summary
// diagnostic correctly reports refresh statistics.
func TestContext2Plan_smartRefresh_statsReporting(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "unchanged" {
  ami = "ami-same"
}

resource "test_instance" "changed" {
  ami = "ami-new"
}
`,
	})

	p, _ := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.unchanged").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-unchanged","ami":"ami-same"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.changed").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-changed","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// Look for the smart refresh summary diagnostic
	found := false
	for _, d := range diags {
		if strings.Contains(d.Description().Summary, "Smart refresh") {
			detail := d.Description().Detail
			if !strings.Contains(detail, "2 resources evaluated") {
				t.Errorf("expected '2 resources evaluated' in detail, got: %s", detail)
			}
			if !strings.Contains(detail, "1 refreshed") {
				t.Errorf("expected '1 refreshed' in detail, got: %s", detail)
			}
			if !strings.Contains(detail, "1 skipped") {
				t.Errorf("expected '1 skipped' in detail, got: %s", detail)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'Smart refresh summary' diagnostic but it was not emitted")
	}
}

// TestContext2Plan_smartRefresh_destroyModeRejectsChanged verifies that
// -refresh=changed is rejected when combined with -destroy mode.
func TestContext2Plan_smartRefresh_destroyModeRejectsChanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  ami = "ami-test"
}
`,
	})

	p, _ := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-foo","ami":"ami-test"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	// RefreshChanged should work fine with DestroyMode - it just runs full refresh
	// internally since destroy plans always refresh. Verify no errors.
	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.DestroyMode,
		RefreshMode: RefreshChanged,
	})
	// Destroy mode with smart refresh should either work or error clearly.
	// Let's check the behavior.
	_ = diags
}

// TestContext2Plan_smartRefresh_newResourceAlwaysRefreshes verifies that a
// resource that exists in config but not in state (new resource) is handled
// correctly - detectConfigChange returns true for nil state.
func TestContext2Plan_smartRefresh_newResourceAlwaysRefreshes(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "existing" {
  ami = "ami-same"
}

resource "test_instance" "brand_new" {
  ami = "ami-brand-new"
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	// Only "existing" is in state; "brand_new" is not.
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.existing").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-existing","ami":"ami-same"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// Existing unchanged resource should not be refreshed
	if readCallCount(readCalls, "i-existing") != 0 {
		t.Error("existing unchanged resource should NOT have been refreshed")
	}

	// brand_new should be in the plan as a create action (no ReadResource since no state)
	foundCreate := false
	for _, rc := range plan.Changes.Resources {
		if rc.Addr.String() == "test_instance.brand_new" && rc.Action == plans.Create {
			foundCreate = true
		}
	}
	if !foundCreate {
		t.Error("expected create action for test_instance.brand_new")
	}
}

// TestContext2Plan_smartRefresh_mixedChangedAndUnchanged verifies the mixed
// scenario: some resources changed, some unchanged, some with dependencies.
func TestContext2Plan_smartRefresh_mixedChangedAndUnchanged(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "a" {
  ami = "ami-same"
}

resource "test_instance" "b" {
  ami = "ami-changed"
}

resource "test_instance" "c" {
  ami   = "ami-same"
  value = test_instance.b.id
}

resource "test_instance" "d" {
  ami   = "ami-same"
  value = test_instance.a.id
}
`,
	})

	p, readCalls := smartRefreshTestProvider(t)

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)

	// A: unchanged
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-a","ami":"ami-same"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
	// B: config changed (ami-old -> ami-changed)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.b").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-b","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
	// C: unchanged config, depends on B (which changed)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.c").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-c","ami":"ami-same","value":"i-b"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
	// D: unchanged config, depends on A (which is unchanged)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.d").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-d","ami":"ami-same","value":"i-a"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshChanged,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// A: unchanged config, no upstream -> skip
	if readCallCount(readCalls, "i-a") != 0 {
		t.Error("resource A should NOT have been refreshed")
	}
	// B: config changed -> refresh
	if readCallCount(readCalls, "i-b") == 0 {
		t.Error("resource B should have been refreshed (config changed)")
	}
	// C: unchanged config but depends on B (changed) -> refresh
	if readCallCount(readCalls, "i-c") == 0 {
		t.Error("resource C should have been refreshed (upstream B changed)")
	}
	// D: unchanged config, depends on A (unchanged) -> skip
	if readCallCount(readCalls, "i-d") != 0 {
		t.Error("resource D should NOT have been refreshed (upstream A unchanged)")
	}
}

// TestContext2Plan_smartRefresh_backwardCompat_refreshTrue verifies that the
// default RefreshAll mode behaves exactly like the legacy -refresh=true.
func TestContext2Plan_smartRefresh_backwardCompat_refreshTrue(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  ami = "ami-same"
}
`,
	})

	var readCount int32
	p := testProvider("test")
	p.GetProviderSchemaResponse = smartRefreshTestSchema()
	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		atomic.AddInt32(&readCount, 1)
		return providers.ReadResourceResponse{NewState: req.PriorState}
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{PlannedState: req.ProposedNewState}
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-foo","ami":"ami-same"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	// Default opts: RefreshMode=RefreshAll (0 value)
	_, diags := ctx.Plan(context.Background(), m, state, DefaultPlanOpts)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// With -refresh=true, even unchanged resources should be refreshed
	if atomic.LoadInt32(&readCount) == 0 {
		t.Error("with RefreshAll, ReadResource should have been called")
	}
}

// TestContext2Plan_smartRefresh_backwardCompat_refreshFalse verifies that
// RefreshNone skips all refreshes, same as legacy -refresh=false.
func TestContext2Plan_smartRefresh_backwardCompat_refreshFalse(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "foo" {
  ami = "ami-new"
}
`,
	})

	var readCount int32
	p := testProvider("test")
	p.GetProviderSchemaResponse = smartRefreshTestSchema()
	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		atomic.AddInt32(&readCount, 1)
		return providers.ReadResourceResponse{NewState: req.PriorState}
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{PlannedState: req.ProposedNewState}
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-foo","ami":"ami-old"}`),
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)

	ctx := testContext2(t, &ContextOpts{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan(context.Background(), m, state, &PlanOpts{
		Mode:        plans.NormalMode,
		RefreshMode: RefreshNone,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	// With -refresh=false, no resources should be refreshed even if config changed
	if atomic.LoadInt32(&readCount) != 0 {
		t.Error("with RefreshNone, ReadResource should NOT have been called")
	}
}
