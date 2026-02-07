// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/dag"
	"github.com/opentofu/opentofu/internal/states"
)

func TestChangeConeTransformer_skipAndNil(t *testing.T) {
	cases := []struct {
		name            string
		transformer     *ChangeConeTransformer
		initialVertices int
		wantVertices    int
		wantTotal       int64
		wantKept        int64
		wantRemoved     int64
	}{
		{
			name: "skip",
			transformer: &ChangeConeTransformer{
				Skip:           true,
				RefreshTracker: NewRefreshTracker(),
				Config:         &configs.Config{},
				State:          states.NewState(),
			},
			initialVertices: 2,
			wantVertices:    2,
			wantTotal:       0,
			wantKept:        0,
			wantRemoved:     0,
		},
		{
			name: "nil config",
			transformer: &ChangeConeTransformer{
				RefreshTracker: NewRefreshTracker(),
				Config:         nil,
				State:          states.NewState(),
			},
			initialVertices: 2,
			wantVertices:    2,
			wantTotal:       0,
			wantKept:        0,
			wantRemoved:     0,
		},
		{
			name: "nil state",
			transformer: &ChangeConeTransformer{
				RefreshTracker: NewRefreshTracker(),
				Config:         &configs.Config{},
				State:          nil,
			},
			initialVertices: 2,
			wantVertices:    2,
			wantTotal:       0,
			wantKept:        0,
			wantRemoved:     0,
		},
		{
			name: "nil config and state",
			transformer: &ChangeConeTransformer{
				RefreshTracker: NewRefreshTracker(),
				Config:         nil,
				State:          nil,
			},
			initialVertices: 2,
			wantVertices:    2,
			wantTotal:       0,
			wantKept:        0,
			wantRemoved:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := Graph{Path: addrs.RootModuleInstance}
			g.Add(&testChangeConeVertex{name: "one"})
			g.Add(&testChangeConeVertex{name: "two"})

			if len(g.Vertices()) != tc.initialVertices {
				t.Fatalf("expected %d initial vertices, got %d", tc.initialVertices, len(g.Vertices()))
			}

			if err := tc.transformer.Transform(t.Context(), &g); err != nil {
				t.Fatalf("err: %s", err)
			}

			if len(g.Vertices()) != tc.wantVertices {
				t.Fatalf("expected %d vertices, got %d", tc.wantVertices, len(g.Vertices()))
			}

			total, kept, removed := tc.transformer.RefreshTracker.ChangeConeStats()
			if total != tc.wantTotal || kept != tc.wantKept || removed != tc.wantRemoved {
				t.Fatalf("expected stats total=%d kept=%d removed=%d, got total=%d kept=%d removed=%d", tc.wantTotal, tc.wantKept, tc.wantRemoved, total, kept, removed)
			}
		})
	}
}

func TestChangeConeTransformer_newResourcePrunesGraph(t *testing.T) {
	cases := []struct {
		name         string
		config       *configs.Config
		state        *states.State
		changed      []string
		wantVertices []string
		wantTotal    int64
		wantKept     int64
		wantRemoved  int64
	}{
		{
			name: "new resource kept, existing pruned",
			config: testModuleInline(t, map[string]string{
				"main.tf": `
resource "test_instance" "existing" {}
resource "test_instance" "new" {}
`,
			}),
			state: func() *states.State {
				state := states.NewState()
				rootMod := state.EnsureModule(addrs.RootModuleInstance)
				addStateResource(t, rootMod, "existing")
				return state
			}(),
			wantVertices: []string{"test_instance.new"},
			wantTotal:    2,
			wantKept:     1,
			wantRemoved:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewRefreshTracker()
			g := Graph{Path: addrs.RootModuleInstance}
			g.Add(&testChangeConeResourceNode{
				name: "test_instance.existing",
				addr: addrs.RootModule.Resource(addrs.ManagedResourceMode, "test_instance", "existing"),
			})
			g.Add(&testChangeConeResourceNode{
				name: "test_instance.new",
				addr: addrs.RootModule.Resource(addrs.ManagedResourceMode, "test_instance", "new"),
			})

			tf := &ChangeConeTransformer{
				RefreshTracker: tracker,
				Config:         tc.config,
				State:          tc.state,
			}

			if err := tf.Transform(t.Context(), &g); err != nil {
				t.Fatalf("err: %s", err)
			}

			gotNames := verticesByName(g.Vertices())
			for _, name := range tc.wantVertices {
				if !gotNames[name] {
					t.Fatalf("expected %q in graph", name)
				}
			}
			if len(g.Vertices()) != len(tc.wantVertices) {
				t.Fatalf("expected %d vertices, got %d", len(tc.wantVertices), len(g.Vertices()))
			}

			total, kept, removed := tracker.ChangeConeStats()
			if total != tc.wantTotal || kept != tc.wantKept || removed != tc.wantRemoved {
				t.Fatalf("expected stats total=%d kept=%d removed=%d, got total=%d kept=%d removed=%d", tc.wantTotal, tc.wantKept, tc.wantRemoved, total, kept, removed)
			}
		})
	}
}

func TestChangeConeTransformer_rootOutputs(t *testing.T) {
	cases := []struct {
		name        string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "root outputs follow change cone",
			wantPresent: []string{"test_instance.a", "output.a_value"},
			wantAbsent:  []string{"test_instance.b", "output.b_value"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewRefreshTracker()
			tracker.MarkConfigChanged(addrs.RootModule.Resource(addrs.ManagedResourceMode, "test_instance", "a"))

			m := testModuleInline(t, map[string]string{
				"main.tf": `
resource "test_instance" "a" {}
resource "test_instance" "b" {}
`,
			})

			state := states.NewState()
			rootMod := state.EnsureModule(addrs.RootModuleInstance)
			addStateResource(t, rootMod, "a")
			addStateResource(t, rootMod, "b")

			g := Graph{Path: addrs.RootModuleInstance}
			resourceA := &testChangeConeResourceNode{
				name: "test_instance.a",
				addr: addrs.RootModule.Resource(addrs.ManagedResourceMode, "test_instance", "a"),
			}
			resourceB := &testChangeConeResourceNode{
				name: "test_instance.b",
				addr: addrs.RootModule.Resource(addrs.ManagedResourceMode, "test_instance", "b"),
			}
			outputA := &testChangeConeOutputNode{name: "output.a_value", isRoot: true}
			outputB := &testChangeConeOutputNode{name: "output.b_value", isRoot: true}

			g.Add(resourceA)
			g.Add(resourceB)
			g.Add(outputA)
			g.Add(outputB)
			g.Connect(dag.BasicEdge(outputA, resourceA))
			g.Connect(dag.BasicEdge(outputB, resourceB))

			tf := &ChangeConeTransformer{
				RefreshTracker: tracker,
				Config:         m,
				State:          state,
			}

			if err := tf.Transform(t.Context(), &g); err != nil {
				t.Fatalf("err: %s", err)
			}

			gotNames := verticesByName(g.Vertices())
			for _, name := range tc.wantPresent {
				if !gotNames[name] {
					t.Fatalf("expected %q in graph", name)
				}
			}
			for _, name := range tc.wantAbsent {
				if gotNames[name] {
					t.Fatalf("expected %q to be removed", name)
				}
			}
		})
	}
}

type testChangeConeVertex struct {
	name string
}

func (v *testChangeConeVertex) Name() string {
	return v.name
}

type testChangeConeResourceNode struct {
	name string
	addr addrs.ConfigResource
}

func (n *testChangeConeResourceNode) Name() string {
	return n.name
}

func (n *testChangeConeResourceNode) ResourceAddr() addrs.ConfigResource {
	return n.addr
}

var _ GraphNodeConfigResource = (*testChangeConeResourceNode)(nil)

type testChangeConeOutputNode struct {
	name   string
	isRoot bool
}

func (n *testChangeConeOutputNode) Name() string {
	return n.name
}

func (n *testChangeConeOutputNode) temporaryValue(op walkOperation) bool {
	return !n.isRoot
}

var _ graphNodeTemporaryValue = (*testChangeConeOutputNode)(nil)

func verticesByName(vertices []dag.Vertex) map[string]bool {
	result := make(map[string]bool, len(vertices))
	for _, v := range vertices {
		result[dag.VertexName(v)] = true
	}
	return result
}

func addStateResource(t *testing.T, mod *states.Module, name string) {
	t.Helper()
	mod.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: name,
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{"id":"` + name + `"}`),
			Status:    states.ObjectReady,
		},
		mustProviderConfig(`provider["registry.opentofu.org/hashicorp/test"]`),
		addrs.NoKey,
	)
}
