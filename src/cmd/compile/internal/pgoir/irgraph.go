// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A note on line numbers: when working with line numbers, we always use the
// binary-visible relative line number. i.e., the line number as adjusted by
// //line directives (ctxt.InnermostPos(ir.Node.Pos()).RelLine()). Use
// NodeLineOffset to compute line offsets.
//
// If you are thinking, "wait, doesn't that just make things more complex than
// using the real line number?", then you are 100% correct. Unfortunately,
// pprof profiles generated by the runtime always contain line numbers as
// adjusted by //line directives (because that is what we put in pclntab). Thus
// for the best behavior when attempting to match the source with the profile
// it makes sense to use the same line number space.
//
// Some of the effects of this to keep in mind:
//
//  - For files without //line directives there is no impact, as RelLine() ==
//    Line().
//  - For functions entirely covered by the same //line directive (i.e., a
//    directive before the function definition and no directives within the
//    function), there should also be no impact, as line offsets within the
//    function should be the same as the real line offsets.
//  - Functions containing //line directives may be impacted. As fake line
//    numbers need not be monotonic, we may compute negative line offsets. We
//    should accept these and attempt to use them for best-effort matching, as
//    these offsets should still match if the source is unchanged, and may
//    continue to match with changed source depending on the impact of the
//    changes on fake line numbers.
//  - Functions containing //line directives may also contain duplicate lines,
//    making it ambiguous which call the profile is referencing. This is a
//    similar problem to multiple calls on a single real line, as we don't
//    currently track column numbers.
//
// Long term it would be best to extend pprof profiles to include real line
// numbers. Until then, we have to live with these complexities. Luckily,
// //line directives that change line numbers in strange ways should be rare,
// and failing PGO matching on these files is not too big of a loss.

// Package pgoir assosciates a PGO profile with the IR of the current package
// compilation.
package pgoir

import (
	"bufio"
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/pgo"
	"fmt"
	"os"
)

// IRGraph is a call graph with nodes pointing to IRs of functions and edges
// carrying weights and callsite information.
//
// Nodes for indirect calls may have missing IR (IRNode.AST == nil) if the node
// is not visible from this package (e.g., not in the transitive deps). Keeping
// these nodes allows determining the hottest edge from a call even if that
// callee is not available.
//
// TODO(prattmic): Consider merging this data structure with Graph. This is
// effectively a copy of Graph aggregated to line number and pointing to IR.
type IRGraph struct {
	// Nodes of the graph. Each node represents a function, keyed by linker
	// symbol name.
	IRNodes map[string]*IRNode
}

// IRNode represents a node (function) in the IRGraph.
type IRNode struct {
	// Pointer to the IR of the Function represented by this node.
	AST *ir.Func
	// Linker symbol name of the Function represented by this node.
	// Populated only if AST == nil.
	LinkerSymbolName string

	// Set of out-edges in the callgraph. The map uniquely identifies each
	// edge based on the callsite and callee, for fast lookup.
	OutEdges map[pgo.NamedCallEdge]*IREdge
}

// Name returns the symbol name of this function.
func (i *IRNode) Name() string {
	if i.AST != nil {
		return ir.LinkFuncName(i.AST)
	}
	return i.LinkerSymbolName
}

// IREdge represents a call edge in the IRGraph with source, destination,
// weight, callsite, and line number information.
type IREdge struct {
	// Source and destination of the edge in IRNode.
	Src, Dst       *IRNode
	Weight         int64
	CallSiteOffset int // Line offset from function start line.
}

// CallSiteInfo captures call-site information and its caller/callee.
type CallSiteInfo struct {
	LineOffset int // Line offset from function start line.
	Caller     *ir.Func
	Callee     *ir.Func
}

// Profile contains the processed PGO profile and weighted call graph used for
// PGO optimizations.
type Profile struct {
	// Profile is the base data from the raw profile, without IR attribution.
	*pgo.Profile

	// WeightedCG represents the IRGraph built from profile, which we will
	// update as part of inlining.
	WeightedCG *IRGraph
}

// New generates a profile-graph from the profile or pre-processed profile.
func New(profileFile string) (*Profile, error) {
	f, err := os.Open(profileFile)
	if err != nil {
		return nil, fmt.Errorf("error opening profile: %w", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	isSerialized, err := pgo.IsSerialized(r)
	if err != nil {
		return nil, fmt.Errorf("error processing profile header: %w", err)
	}

	var base *pgo.Profile
	if isSerialized {
		base, err = pgo.FromSerialized(r)
		if err != nil {
			return nil, fmt.Errorf("error processing serialized PGO profile: %w", err)
		}
	} else {
		base, err = pgo.FromPProf(r)
		if err != nil {
			return nil, fmt.Errorf("error processing pprof PGO profile: %w", err)
		}
	}

	if base.TotalWeight == 0 {
		return nil, nil // accept but ignore profile with no samples.
	}

	// Create package-level call graph with weights from profile and IR.
	wg := createIRGraph(base.NamedEdgeMap)

	return &Profile{
		Profile:    base,
		WeightedCG: wg,
	}, nil
}

// initializeIRGraph builds the IRGraph by visiting all the ir.Func in decl list
// of a package.
func createIRGraph(namedEdgeMap pgo.NamedEdgeMap) *IRGraph {
	g := &IRGraph{
		IRNodes: make(map[string]*IRNode),
	}

	// Bottomup walk over the function to create IRGraph.
	ir.VisitFuncsBottomUp(typecheck.Target.Funcs, func(list []*ir.Func, recursive bool) {
		for _, fn := range list {
			visitIR(fn, namedEdgeMap, g)
		}
	})

	// Add additional edges for indirect calls. This must be done second so
	// that IRNodes is fully populated (see the dummy node TODO in
	// addIndirectEdges).
	//
	// TODO(prattmic): visitIR above populates the graph via direct calls
	// discovered via the IR. addIndirectEdges populates the graph via
	// calls discovered via the profile. This combination of opposite
	// approaches is a bit awkward, particularly because direct calls are
	// discoverable via the profile as well. Unify these into a single
	// approach.
	addIndirectEdges(g, namedEdgeMap)

	return g
}

// visitIR traverses the body of each ir.Func adds edges to g from ir.Func to
// any called function in the body.
func visitIR(fn *ir.Func, namedEdgeMap pgo.NamedEdgeMap, g *IRGraph) {
	name := ir.LinkFuncName(fn)
	node, ok := g.IRNodes[name]
	if !ok {
		node = &IRNode{
			AST: fn,
		}
		g.IRNodes[name] = node
	}

	// Recursively walk over the body of the function to create IRGraph edges.
	createIRGraphEdge(fn, node, name, namedEdgeMap, g)
}

// createIRGraphEdge traverses the nodes in the body of ir.Func and adds edges
// between the callernode which points to the ir.Func and the nodes in the
// body.
func createIRGraphEdge(fn *ir.Func, callernode *IRNode, name string, namedEdgeMap pgo.NamedEdgeMap, g *IRGraph) {
	ir.VisitList(fn.Body, func(n ir.Node) {
		switch n.Op() {
		case ir.OCALLFUNC:
			call := n.(*ir.CallExpr)
			// Find the callee function from the call site and add the edge.
			callee := DirectCallee(call.Fun)
			if callee != nil {
				addIREdge(callernode, name, n, callee, namedEdgeMap, g)
			}
		case ir.OCALLMETH:
			call := n.(*ir.CallExpr)
			// Find the callee method from the call site and add the edge.
			callee := ir.MethodExprName(call.Fun).Func
			addIREdge(callernode, name, n, callee, namedEdgeMap, g)
		}
	})
}

// NodeLineOffset returns the line offset of n in fn.
func NodeLineOffset(n ir.Node, fn *ir.Func) int {
	// See "A note on line numbers" at the top of the file.
	line := int(base.Ctxt.InnermostPos(n.Pos()).RelLine())
	startLine := int(base.Ctxt.InnermostPos(fn.Pos()).RelLine())
	return line - startLine
}

// addIREdge adds an edge between caller and new node that points to `callee`
// based on the profile-graph and NodeMap.
func addIREdge(callerNode *IRNode, callerName string, call ir.Node, callee *ir.Func, namedEdgeMap pgo.NamedEdgeMap, g *IRGraph) {
	calleeName := ir.LinkFuncName(callee)
	calleeNode, ok := g.IRNodes[calleeName]
	if !ok {
		calleeNode = &IRNode{
			AST: callee,
		}
		g.IRNodes[calleeName] = calleeNode
	}

	namedEdge := pgo.NamedCallEdge{
		CallerName:     callerName,
		CalleeName:     calleeName,
		CallSiteOffset: NodeLineOffset(call, callerNode.AST),
	}

	// Add edge in the IRGraph from caller to callee.
	edge := &IREdge{
		Src:            callerNode,
		Dst:            calleeNode,
		Weight:         namedEdgeMap.Weight[namedEdge],
		CallSiteOffset: namedEdge.CallSiteOffset,
	}

	if callerNode.OutEdges == nil {
		callerNode.OutEdges = make(map[pgo.NamedCallEdge]*IREdge)
	}
	callerNode.OutEdges[namedEdge] = edge
}

// LookupFunc looks up a function or method in export data. It is expected to
// be overridden by package noder, to break a dependency cycle.
var LookupFunc = func(fullName string) (*ir.Func, error) {
	base.Fatalf("pgo.LookupMethodFunc not overridden")
	panic("unreachable")
}

// addIndirectEdges adds indirect call edges found in the profile to the graph,
// to be used for devirtualization.
//
// N.B. despite the name, addIndirectEdges will add any edges discovered via
// the profile. We don't know for sure that they are indirect, but assume they
// are since direct calls would already be added. (e.g., direct calls that have
// been deleted from source since the profile was taken would be added here).
//
// TODO(prattmic): Devirtualization runs before inlining, so we can't devirtualize
// calls inside inlined call bodies. If we did add that, we'd need edges from
// inlined bodies as well.
func addIndirectEdges(g *IRGraph, namedEdgeMap pgo.NamedEdgeMap) {
	// g.IRNodes is populated with the set of functions in the local
	// package build by VisitIR. We want to filter for local functions
	// below, but we also add unknown callees to IRNodes as we go. So make
	// an initial copy of IRNodes to recall just the local functions.
	localNodes := make(map[string]*IRNode, len(g.IRNodes))
	for k, v := range g.IRNodes {
		localNodes[k] = v
	}

	// N.B. We must consider edges in a stable order because export data
	// lookup order (LookupMethodFunc, below) can impact the export data of
	// this package, which must be stable across different invocations for
	// reproducibility.
	//
	// The weight ordering of ByWeight is irrelevant, it just happens to be
	// an ordered list of edges that is already available.
	for _, key := range namedEdgeMap.ByWeight {
		weight := namedEdgeMap.Weight[key]
		// All callers in the local package build were added to IRNodes
		// in VisitIR. If a caller isn't in the local package build we
		// can skip adding edges, since we won't be devirtualizing in
		// them anyway. This keeps the graph smaller.
		callerNode, ok := localNodes[key.CallerName]
		if !ok {
			continue
		}

		// Already handled this edge?
		if _, ok := callerNode.OutEdges[key]; ok {
			continue
		}

		calleeNode, ok := g.IRNodes[key.CalleeName]
		if !ok {
			// IR is missing for this callee. VisitIR populates
			// IRNodes with all functions discovered via local
			// package function declarations and calls. This
			// function may still be available from export data of
			// a transitive dependency.
			//
			// TODO(prattmic): Parameterized types/functions are
			// not supported.
			//
			// TODO(prattmic): This eager lookup during graph load
			// is simple, but wasteful. We are likely to load many
			// functions that we never need. We could delay load
			// until we actually need the method in
			// devirtualization. Instantiation of generic functions
			// will likely need to be done at the devirtualization
			// site, if at all.
			fn, err := LookupFunc(key.CalleeName)
			if err == nil {
				if base.Debug.PGODebug >= 3 {
					fmt.Printf("addIndirectEdges: %s found in export data\n", key.CalleeName)
				}
				calleeNode = &IRNode{AST: fn}

				// N.B. we could call createIRGraphEdge to add
				// direct calls in this newly-imported
				// function's body to the graph. Similarly, we
				// could add to this function's queue to add
				// indirect calls. However, those would be
				// useless given the visit order of inlining,
				// and the ordering of PGO devirtualization and
				// inlining. This function can only be used as
				// an inlined body. We will never do PGO
				// devirtualization inside an inlined call. Nor
				// will we perform inlining inside an inlined
				// call.
			} else {
				// Still not found. Most likely this is because
				// the callee isn't in the transitive deps of
				// this package.
				//
				// Record this call anyway. If this is the hottest,
				// then we want to skip devirtualization rather than
				// devirtualizing to the second most common callee.
				if base.Debug.PGODebug >= 3 {
					fmt.Printf("addIndirectEdges: %s not found in export data: %v\n", key.CalleeName, err)
				}
				calleeNode = &IRNode{LinkerSymbolName: key.CalleeName}
			}

			// Add dummy node back to IRNodes. We don't need this
			// directly, but PrintWeightedCallGraphDOT uses these
			// to print nodes.
			g.IRNodes[key.CalleeName] = calleeNode
		}
		edge := &IREdge{
			Src:            callerNode,
			Dst:            calleeNode,
			Weight:         weight,
			CallSiteOffset: key.CallSiteOffset,
		}

		if callerNode.OutEdges == nil {
			callerNode.OutEdges = make(map[pgo.NamedCallEdge]*IREdge)
		}
		callerNode.OutEdges[key] = edge
	}
}

// PrintWeightedCallGraphDOT prints IRGraph in DOT format.
func (p *Profile) PrintWeightedCallGraphDOT(edgeThreshold float64) {
	fmt.Printf("\ndigraph G {\n")
	fmt.Printf("forcelabels=true;\n")

	// List of functions in this package.
	funcs := make(map[string]struct{})
	ir.VisitFuncsBottomUp(typecheck.Target.Funcs, func(list []*ir.Func, recursive bool) {
		for _, f := range list {
			name := ir.LinkFuncName(f)
			funcs[name] = struct{}{}
		}
	})

	// Determine nodes of DOT.
	//
	// Note that ir.Func may be nil for functions not visible from this
	// package.
	nodes := make(map[string]*ir.Func)
	for name := range funcs {
		if n, ok := p.WeightedCG.IRNodes[name]; ok {
			for _, e := range n.OutEdges {
				if _, ok := nodes[e.Src.Name()]; !ok {
					nodes[e.Src.Name()] = e.Src.AST
				}
				if _, ok := nodes[e.Dst.Name()]; !ok {
					nodes[e.Dst.Name()] = e.Dst.AST
				}
			}
			if _, ok := nodes[n.Name()]; !ok {
				nodes[n.Name()] = n.AST
			}
		}
	}

	// Print nodes.
	for name, ast := range nodes {
		if _, ok := p.WeightedCG.IRNodes[name]; ok {
			style := "solid"
			if ast == nil {
				style = "dashed"
			}

			if ast != nil && ast.Inl != nil {
				fmt.Printf("\"%v\" [color=black, style=%s, label=\"%v,inl_cost=%d\"];\n", name, style, name, ast.Inl.Cost)
			} else {
				fmt.Printf("\"%v\" [color=black, style=%s, label=\"%v\"];\n", name, style, name)
			}
		}
	}
	// Print edges.
	ir.VisitFuncsBottomUp(typecheck.Target.Funcs, func(list []*ir.Func, recursive bool) {
		for _, f := range list {
			name := ir.LinkFuncName(f)
			if n, ok := p.WeightedCG.IRNodes[name]; ok {
				for _, e := range n.OutEdges {
					style := "solid"
					if e.Dst.AST == nil {
						style = "dashed"
					}
					color := "black"
					edgepercent := pgo.WeightInPercentage(e.Weight, p.TotalWeight)
					if edgepercent > edgeThreshold {
						color = "red"
					}

					fmt.Printf("edge [color=%s, style=%s];\n", color, style)
					fmt.Printf("\"%v\" -> \"%v\" [label=\"%.2f\"];\n", n.Name(), e.Dst.Name(), edgepercent)
				}
			}
		}
	})
	fmt.Printf("}\n")
}

// DirectCallee takes a function-typed expression and returns the underlying
// function that it refers to if statically known. Otherwise, it returns nil.
//
// Equivalent to inline.inlCallee without calling CanInline on closures.
func DirectCallee(fn ir.Node) *ir.Func {
	fn = ir.StaticValue(fn)
	switch fn.Op() {
	case ir.OMETHEXPR:
		fn := fn.(*ir.SelectorExpr)
		n := ir.MethodExprName(fn)
		// Check that receiver type matches fn.X.
		// TODO(mdempsky): Handle implicit dereference
		// of pointer receiver argument?
		if n == nil || !types.Identical(n.Type().Recv().Type, fn.X.Type()) {
			return nil
		}
		return n.Func
	case ir.ONAME:
		fn := fn.(*ir.Name)
		if fn.Class == ir.PFUNC {
			return fn.Func
		}
	case ir.OCLOSURE:
		fn := fn.(*ir.ClosureExpr)
		c := fn.Func
		return c
	}
	return nil
}