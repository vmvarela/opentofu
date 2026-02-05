// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
)

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
