// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"log"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/zclconf/go-cty/cty"
)

// AttributeChangeDetector detects changes in resource attributes by comparing
// configuration values with state values, for attributes that can be evaluated
// without requiring a refresh operation.
type AttributeChangeDetector struct {
	config    *configs.Config
	state     *states.State
	variables map[string]cty.Value
	locals    map[string]cty.Value
}

// NewAttributeChangeDetector creates a detector that can identify attribute
// changes without needing to refresh resource state.
func NewAttributeChangeDetector(
	config *configs.Config,
	state *states.State,
	variables map[string]cty.Value,
) *AttributeChangeDetector {
	return &AttributeChangeDetector{
		config:    config,
		state:     state,
		variables: variables,
		locals:    make(map[string]cty.Value),
	}
}

// DetectChangedResources analyzes all resources in the configuration and returns
// those that have detectable attribute changes (changes in attributes that don't
// require refresh to evaluate).
func (d *AttributeChangeDetector) DetectChangedResources() []addrs.AbsResource {
	if d.config == nil || d.state == nil {
		return nil
	}

	changed := make([]addrs.AbsResource, 0)

	// First, evaluate locals that don't depend on resources
	d.evaluateStaticLocals()

	// Check each resource in config
	d.walkConfig(d.config, addrs.RootModuleInstance, &changed)

	return changed
}

// walkConfig recursively walks the configuration tree checking for changes.
func (d *AttributeChangeDetector) walkConfig(
	cfg *configs.Config,
	moduleAddr addrs.ModuleInstance,
	changed *[]addrs.AbsResource,
) {
	if cfg.Module == nil {
		return
	}

	// Check managed resources
	for name, res := range cfg.Module.ManagedResources {
		addr := addrs.AbsResource{
			Module: moduleAddr,
			Resource: addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: res.Type,
				Name: name,
			},
		}

		if d.hasDetectableChanges(res, addr) {
			*changed = append(*changed, addr)
		}
	}

	// Recurse into child modules
	for name, child := range cfg.Children {
		childAddr := moduleAddr.Child(name, addrs.NoKey)
		d.walkConfig(child, childAddr, changed)
	}
}

// hasDetectableChanges checks if a resource has changes in attributes that
// we can evaluate without refresh.
func (d *AttributeChangeDetector) hasDetectableChanges(
	res *configs.Resource,
	addr addrs.AbsResource,
) bool {
	// Get the resource from state
	stateRes := d.getResourceFromState(addr)
	if stateRes == nil {
		// Resource not in state - it's new
		log.Printf("[DEBUG] Velocity: %s is new (not in state)", addr)
		return true
	}

	// Get any instance to compare (for non-counted resources)
	var stateInstance *states.ResourceInstanceObjectSrc
	for _, inst := range stateRes.Instances {
		if inst.Current != nil {
			stateInstance = inst.Current
			break
		}
	}
	if stateInstance == nil {
		// No current instance - needs refresh anyway
		return true
	}

	// Analyze the resource configuration body
	if res.Config == nil {
		return false
	}

	// Check for changes in evaluable attributes
	attrs, _ := res.Config.JustAttributes()
	for attrName, attr := range attrs {
		analysis := AnalyzeExpression(attr.Expr)

		if analysis.CanEvaluateStatically() {
			// We can evaluate this attribute without refresh
			configValue, diags := d.evaluateExpression(attr.Expr)
			if diags.HasErrors() {
				continue // Can't evaluate, skip
			}

			// Get the state value for this attribute
			stateValue := d.getStateAttributeValue(stateInstance, attrName)
			if stateValue == cty.NilVal {
				continue // Can't compare, skip
			}

			// Compare values
			if !configValue.RawEquals(stateValue) {
				log.Printf("[DEBUG] Velocity: %s.%s changed (config=%v, state=%v)",
					addr, attrName, configValue.GoString(), stateValue.GoString())
				return true
			}
		}
	}

	return false
}

// getResourceFromState retrieves a resource from the state by address.
func (d *AttributeChangeDetector) getResourceFromState(addr addrs.AbsResource) *states.Resource {
	if d.state == nil {
		return nil
	}

	ms := d.state.Module(addr.Module)
	if ms == nil {
		return nil
	}

	return ms.Resource(addr.Resource)
}

// getStateAttributeValue extracts an attribute value from the state instance.
func (d *AttributeChangeDetector) getStateAttributeValue(
	instance *states.ResourceInstanceObjectSrc,
	attrName string,
) cty.Value {
	if instance == nil || instance.AttrsJSON == nil {
		return cty.NilVal
	}

	// Decode the JSON attributes
	// Note: This is a simplified approach - in production we'd use the schema
	attrs, err := instance.Decode(cty.DynamicPseudoType)
	if err != nil {
		return cty.NilVal
	}

	if attrs.Value.Type().HasAttribute(attrName) {
		return attrs.Value.GetAttr(attrName)
	}

	return cty.NilVal
}

// evaluateExpression evaluates an HCL expression using only variables and locals.
func (d *AttributeChangeDetector) evaluateExpression(expr hcl.Expression) (cty.Value, hcl.Diagnostics) {
	ctx := d.buildEvalContext()
	return expr.Value(ctx)
}

// buildEvalContext creates an evaluation context with variables and locals.
func (d *AttributeChangeDetector) buildEvalContext() *hcl.EvalContext {
	vars := make(map[string]cty.Value)

	// Add input variables
	if len(d.variables) > 0 {
		vars["var"] = cty.ObjectVal(d.variables)
	} else {
		vars["var"] = cty.EmptyObjectVal
	}

	// Add locals
	if len(d.locals) > 0 {
		vars["local"] = cty.ObjectVal(d.locals)
	} else {
		vars["local"] = cty.EmptyObjectVal
	}

	// Add path attributes (static)
	vars["path"] = cty.ObjectVal(map[string]cty.Value{
		"module": cty.StringVal("."),
		"root":   cty.StringVal("."),
		"cwd":    cty.StringVal("."),
	})

	// Add terraform attributes (static)
	vars["terraform"] = cty.ObjectVal(map[string]cty.Value{
		"workspace": cty.StringVal("default"),
	})

	return &hcl.EvalContext{
		Variables: vars,
	}
}

// evaluateStaticLocals evaluates local values that don't depend on resources.
func (d *AttributeChangeDetector) evaluateStaticLocals() {
	if d.config == nil || d.config.Module == nil {
		return
	}

	// Iterate over locals and evaluate those that are static
	for name, local := range d.config.Module.Locals {
		analysis := AnalyzeExpression(local.Expr)
		if analysis.CanEvaluateStatically() {
			value, diags := d.evaluateExpression(local.Expr)
			if !diags.HasErrors() {
				d.locals[name] = value
			}
		}
	}
}

// ResourceChangeInfo contains detailed information about detected changes.
type ResourceChangeInfo struct {
	Address           addrs.AbsResource
	IsNew             bool
	IsRemoved         bool
	ChangedAttributes []string
	Reason            string
}

// DetectChangesDetailed returns detailed information about each changed resource.
func (d *AttributeChangeDetector) DetectChangesDetailed() []ResourceChangeInfo {
	if d.config == nil {
		return nil
	}

	var changes []ResourceChangeInfo

	// Evaluate static locals first
	d.evaluateStaticLocals()

	// Track resources in config
	configResources := make(map[string]bool)

	// Check each resource in config
	d.walkConfigDetailed(d.config, addrs.RootModuleInstance, &changes, configResources)

	// Check for removed resources (in state but not in config)
	if d.state != nil {
		for _, ms := range d.state.Modules {
			for _, rs := range ms.Resources {
				key := rs.Addr.String()
				if !configResources[key] {
					changes = append(changes, ResourceChangeInfo{
						Address:   rs.Addr,
						IsRemoved: true,
						Reason:    "resource removed from configuration",
					})
				}
			}
		}
	}

	return changes
}

// walkConfigDetailed recursively walks config collecting detailed change info.
func (d *AttributeChangeDetector) walkConfigDetailed(
	cfg *configs.Config,
	moduleAddr addrs.ModuleInstance,
	changes *[]ResourceChangeInfo,
	configResources map[string]bool,
) {
	if cfg.Module == nil {
		return
	}

	for name, res := range cfg.Module.ManagedResources {
		addr := addrs.AbsResource{
			Module: moduleAddr,
			Resource: addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: res.Type,
				Name: name,
			},
		}
		configResources[addr.String()] = true

		info := d.detectDetailedChanges(res, addr)
		if info != nil {
			*changes = append(*changes, *info)
		}
	}

	for childName, child := range cfg.Children {
		childAddr := moduleAddr.Child(childName, addrs.NoKey)
		d.walkConfigDetailed(child, childAddr, changes, configResources)
	}
}

// detectDetailedChanges checks a single resource and returns detailed change info.
func (d *AttributeChangeDetector) detectDetailedChanges(
	res *configs.Resource,
	addr addrs.AbsResource,
) *ResourceChangeInfo {
	stateRes := d.getResourceFromState(addr)
	if stateRes == nil {
		return &ResourceChangeInfo{
			Address: addr,
			IsNew:   true,
			Reason:  "new resource in configuration",
		}
	}

	var stateInstance *states.ResourceInstanceObjectSrc
	for _, inst := range stateRes.Instances {
		if inst.Current != nil {
			stateInstance = inst.Current
			break
		}
	}
	if stateInstance == nil {
		return &ResourceChangeInfo{
			Address: addr,
			IsNew:   true,
			Reason:  "no current instance in state",
		}
	}

	if res.Config == nil {
		return nil
	}

	var changedAttrs []string
	attrs, _ := res.Config.JustAttributes()
	for attrName, attr := range attrs {
		analysis := AnalyzeExpression(attr.Expr)
		if analysis.CanEvaluateStatically() {
			configValue, diags := d.evaluateExpression(attr.Expr)
			if diags.HasErrors() {
				continue
			}

			stateValue := d.getStateAttributeValue(stateInstance, attrName)
			if stateValue == cty.NilVal {
				continue
			}

			if !configValue.RawEquals(stateValue) {
				changedAttrs = append(changedAttrs, attrName)
			}
		}
	}

	if len(changedAttrs) > 0 {
		return &ResourceChangeInfo{
			Address:           addr,
			ChangedAttributes: changedAttrs,
			Reason:            "attribute values changed",
		}
	}

	return nil
}

// AnalyzeResourceConfig analyzes a resource configuration and returns
// information about which attributes can be evaluated without refresh.
func AnalyzeResourceConfig(res *configs.Resource, schema *configschema.Block) *ResourceAnalysis {
	if res == nil || res.Config == nil {
		return nil
	}

	analysis := &ResourceAnalysis{
		StaticAttributes:  make(map[string]*ExpressionAnalysis),
		DynamicAttributes: make(map[string]*ExpressionAnalysis),
	}

	// Use hcldec to get attribute expressions if we have a schema
	if schema != nil {
		spec := schema.DecoderSpec()
		content, _, _ := hcldec.PartialDecode(res.Config, spec, nil)
		if content != cty.NilVal {
			// Schema-aware decoding succeeded
			// Note: For full implementation, we'd iterate through decoded attributes
		}
	}

	// Fallback: analyze JustAttributes
	attrs, _ := res.Config.JustAttributes()
	for name, attr := range attrs {
		exprAnalysis := AnalyzeExpression(attr.Expr)
		if exprAnalysis.CanEvaluateStatically() {
			analysis.StaticAttributes[name] = exprAnalysis
		} else {
			analysis.DynamicAttributes[name] = exprAnalysis
		}
	}

	return analysis
}

// ResourceAnalysis contains the analysis of a resource's configuration.
type ResourceAnalysis struct {
	// StaticAttributes are attributes that can be evaluated without refresh
	StaticAttributes map[string]*ExpressionAnalysis

	// DynamicAttributes are attributes that require resource state
	DynamicAttributes map[string]*ExpressionAnalysis
}

// CanDetectChanges returns true if the resource has any static attributes
// that can be used to detect changes.
func (a *ResourceAnalysis) CanDetectChanges() bool {
	return len(a.StaticAttributes) > 0
}
