// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package velocity

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/lang"
)

// ExpressionType categorizes HCL expressions by their evaluation requirements.
type ExpressionType int

const (
	// ExprTypeStatic - can be evaluated without any external state
	// Examples: literals, pure functions with literal args
	ExprTypeStatic ExpressionType = iota

	// ExprTypeVariables - requires only input variables
	// Examples: var.foo, var.bar
	ExprTypeVariables

	// ExprTypeLocals - requires local values (may depend on variables)
	// Examples: local.name, local.prefix
	ExprTypeLocals

	// ExprTypeResource - requires resource state (needs refresh)
	// Examples: aws_vpc.main.id, module.foo.output
	ExprTypeResource

	// ExprTypeData - requires data source (needs refresh)
	// Examples: data.aws_ami.latest.id
	ExprTypeData
)

// ExpressionAnalysis contains the result of analyzing an HCL expression.
type ExpressionAnalysis struct {
	// Type is the highest-dependency type found in the expression
	Type ExpressionType

	// References contains all parsed references in the expression
	References []*addrs.Reference

	// IsEvaluableWithoutRefresh indicates if we can evaluate this
	// expression using only variables and locals (no resource state)
	IsEvaluableWithoutRefresh bool

	// ResourceRefs contains references to managed resources
	ResourceRefs []addrs.AbsResource

	// DataRefs contains references to data sources
	DataRefs []addrs.AbsResource

	// VariableRefs contains references to input variables
	VariableRefs []string

	// LocalRefs contains references to local values
	LocalRefs []string
}

// AnalyzeExpression examines an HCL expression and categorizes its dependencies.
// This is used to determine if an expression can be evaluated before refresh.
func AnalyzeExpression(expr hcl.Expression) *ExpressionAnalysis {
	if expr == nil {
		return &ExpressionAnalysis{
			Type:                      ExprTypeStatic,
			IsEvaluableWithoutRefresh: true,
		}
	}

	analysis := &ExpressionAnalysis{
		Type:                      ExprTypeStatic,
		IsEvaluableWithoutRefresh: true,
	}

	// Get all references in the expression
	refs, _ := lang.ReferencesInExpr(addrs.ParseRef, expr)
	analysis.References = refs

	// Analyze each reference
	for _, ref := range refs {
		switch subject := ref.Subject.(type) {
		case addrs.InputVariable:
			analysis.VariableRefs = append(analysis.VariableRefs, subject.Name)
			if analysis.Type < ExprTypeVariables {
				analysis.Type = ExprTypeVariables
			}

		case addrs.LocalValue:
			analysis.LocalRefs = append(analysis.LocalRefs, subject.Name)
			if analysis.Type < ExprTypeLocals {
				analysis.Type = ExprTypeLocals
			}

		case addrs.Resource:
			absRes := addrs.AbsResource{
				Module:   addrs.RootModuleInstance,
				Resource: subject,
			}
			if subject.Mode == addrs.DataResourceMode {
				analysis.DataRefs = append(analysis.DataRefs, absRes)
				analysis.Type = ExprTypeData
			} else {
				analysis.ResourceRefs = append(analysis.ResourceRefs, absRes)
				analysis.Type = ExprTypeResource
			}
			analysis.IsEvaluableWithoutRefresh = false

		case addrs.ResourceInstance:
			absRes := addrs.AbsResource{
				Module:   addrs.RootModuleInstance,
				Resource: subject.Resource,
			}
			if subject.Resource.Mode == addrs.DataResourceMode {
				analysis.DataRefs = append(analysis.DataRefs, absRes)
				analysis.Type = ExprTypeData
			} else {
				analysis.ResourceRefs = append(analysis.ResourceRefs, absRes)
				analysis.Type = ExprTypeResource
			}
			analysis.IsEvaluableWithoutRefresh = false

		case addrs.ModuleCallOutput:
			// Module outputs depend on resource state
			analysis.Type = ExprTypeResource
			analysis.IsEvaluableWithoutRefresh = false

		case addrs.ModuleCallInstance:
			// Module instances depend on resource state
			analysis.Type = ExprTypeResource
			analysis.IsEvaluableWithoutRefresh = false

		case addrs.PathAttr, addrs.TerraformAttr:
			// path.* and terraform.* are static
			// Keep current type

		case addrs.CountAttr, addrs.ForEachAttr:
			// count.* and each.* depend on context but not refresh
			// Keep as evaluable

		default:
			// Unknown reference type - be conservative
			analysis.IsEvaluableWithoutRefresh = false
		}
	}

	return analysis
}

// CanEvaluateStatically returns true if the expression can be fully evaluated
// using only the provided variables and locals, without needing resource state.
func (a *ExpressionAnalysis) CanEvaluateStatically() bool {
	return a.IsEvaluableWithoutRefresh
}

// HasResourceDependencies returns true if the expression references any
// managed resources or data sources.
func (a *ExpressionAnalysis) HasResourceDependencies() bool {
	return len(a.ResourceRefs) > 0 || len(a.DataRefs) > 0
}
