// Copyright 2020 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shader

import (
	"fmt"
	"go/ast"
	gconstant "go/constant"
	"go/token"
	"strings"

	"github.com/hajimehoshi/ebiten/internal/shaderir"
)

type variable struct {
	name string
	typ  shaderir.Type
}

type constant struct {
	name string
	typ  shaderir.Type
	init ast.Expr
}

type function struct {
	name  string
	block *block

	ir shaderir.Func
}

type compileState struct {
	fs *token.FileSet

	vertexEntry   string
	fragmentEntry string

	ir shaderir.Program

	// uniforms is a collection of uniform variable names.
	uniforms []string

	funcs []function

	global block

	varyingParsed bool

	errs []string
}

func (cs *compileState) findFunction(name string) (int, bool) {
	for i, f := range cs.funcs {
		if f.name == name {
			return i, true
		}
	}
	return 0, false
}

func (cs *compileState) findUniformVariable(name string) (int, bool) {
	for i, u := range cs.uniforms {
		if u == name {
			return i, true
		}
	}
	return 0, false
}

type typ struct {
	name string
	ir   shaderir.Type
}

type block struct {
	types  []typ
	vars   []variable
	consts []constant
	pos    token.Pos
	outer  *block

	ir shaderir.Block
}

func (b *block) findLocalVariable(name string) (int, shaderir.Type, bool) {
	idx := 0
	for outer := b.outer; outer != nil; outer = outer.outer {
		idx += len(outer.vars)
	}
	for i, v := range b.vars {
		if v.name == name {
			return idx + i, v.typ, true
		}
	}
	if b.outer != nil {
		return b.outer.findLocalVariable(name)
	}
	return 0, shaderir.Type{}, false
}

type ParseError struct {
	errs []string
}

func (p *ParseError) Error() string {
	return strings.Join(p.errs, "\n")
}

func Compile(fs *token.FileSet, f *ast.File, vertexEntry, fragmentEntry string) (*shaderir.Program, error) {
	s := &compileState{
		fs:            fs,
		vertexEntry:   vertexEntry,
		fragmentEntry: fragmentEntry,
	}
	s.parse(f)

	if len(s.errs) > 0 {
		return nil, &ParseError{s.errs}
	}

	// TODO: Resolve identifiers?
	// TODO: Resolve constants

	// TODO: Make a call graph and reorder the elements.

	return &s.ir, nil
}

func (s *compileState) addError(pos token.Pos, str string) {
	p := s.fs.Position(pos)
	s.errs = append(s.errs, fmt.Sprintf("%s: %s", p, str))
}

func (cs *compileState) parse(f *ast.File) {
	// Parse GenDecl for global variables, and then parse functions.
	for _, d := range f.Decls {
		if _, ok := d.(*ast.FuncDecl); !ok {
			if !cs.parseDecl(&cs.global, d) {
				return
			}
		}
	}

	// Sort the uniform variable so that special variable starting with __ should come first.
	var unames []string
	var utypes []shaderir.Type
	for i, u := range cs.uniforms {
		if strings.HasPrefix(u, "__") {
			unames = append(unames, u)
			utypes = append(utypes, cs.ir.Uniforms[i])
		}
	}
	for i, u := range cs.uniforms {
		if !strings.HasPrefix(u, "__") {
			unames = append(unames, u)
			utypes = append(utypes, cs.ir.Uniforms[i])
		}
	}
	cs.uniforms = unames
	cs.ir.Uniforms = utypes

	// Parse function names so that any other function call the others.
	// The function data is provisional and will be updated soon.
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		n := fd.Name.Name
		if n == cs.vertexEntry {
			continue
		}
		if n == cs.fragmentEntry {
			continue
		}

		inParams, outParams := cs.parseFuncParams(fd)
		var inT, outT []shaderir.Type
		for _, v := range inParams {
			inT = append(inT, v.typ)
		}
		for _, v := range outParams {
			outT = append(outT, v.typ)
		}

		cs.funcs = append(cs.funcs, function{
			name: n,
			ir: shaderir.Func{
				Index:     len(cs.funcs),
				InParams:  inT,
				OutParams: outT,
			},
		})
	}

	// Parse functions.
	for _, d := range f.Decls {
		if _, ok := d.(*ast.FuncDecl); ok {
			if !cs.parseDecl(&cs.global, d) {
				return
			}
		}
	}

	if len(cs.errs) > 0 {
		return
	}

	for _, f := range cs.funcs {
		cs.ir.Funcs = append(cs.ir.Funcs, f.ir)
	}
}

func (cs *compileState) parseDecl(b *block, d ast.Decl) bool {
	switch d := d.(type) {
	case *ast.GenDecl:
		switch d.Tok {
		case token.TYPE:
			// TODO: Parse other types
			for _, s := range d.Specs {
				s := s.(*ast.TypeSpec)
				t := cs.parseType(s.Type)
				b.types = append(b.types, typ{
					name: s.Name.Name,
					ir:   t,
				})
			}
		case token.CONST:
			for _, s := range d.Specs {
				s := s.(*ast.ValueSpec)
				cs := cs.parseConstant(s)
				b.consts = append(b.consts, cs...)
			}
		case token.VAR:
			for _, s := range d.Specs {
				s := s.(*ast.ValueSpec)
				vs, inits, stmts, ok := cs.parseVariable(b, s)
				if !ok {
					return false
				}
				b.ir.Stmts = append(b.ir.Stmts, stmts...)
				if b == &cs.global {
					// TODO: Should rhs be ignored?
					for i, v := range vs {
						if !strings.HasPrefix(v.name, "__") {
							if v.name[0] < 'A' || 'Z' < v.name[0] {
								cs.addError(s.Names[i].Pos(), fmt.Sprintf("global variables must be exposed: %s", v.name))
							}
						}
						cs.uniforms = append(cs.uniforms, v.name)
						cs.ir.Uniforms = append(cs.ir.Uniforms, v.typ)
					}
					continue
				}

				base := len(b.vars)
				b.vars = append(b.vars, vs...)

				if len(inits) > 0 {
					for i := range vs {
						b.ir.Stmts = append(b.ir.Stmts, shaderir.Stmt{
							Type: shaderir.Assign,
							Exprs: []shaderir.Expr{
								{
									Type:  shaderir.LocalVariable,
									Index: base + i,
								},
								inits[i],
							},
						})
					}
				}
			}
		case token.IMPORT:
			cs.addError(d.Pos(), "import is forbidden")
		default:
			cs.addError(d.Pos(), "unexpected token")
		}
	case *ast.FuncDecl:
		f, ok := cs.parseFunc(b, d)
		if !ok {
			return false
		}
		if b != &cs.global {
			cs.addError(d.Pos(), "non-global function is not implemented")
			return false
		}
		switch d.Name.Name {
		case cs.vertexEntry:
			cs.ir.VertexFunc.Block = f.ir.Block
		case cs.fragmentEntry:
			cs.ir.FragmentFunc.Block = f.ir.Block
		default:
			// The function is already registered for their names.
			for i := range cs.funcs {
				if cs.funcs[i].name == d.Name.Name {
					// Index is already determined by the provisional parsing.
					f.ir.Index = cs.funcs[i].ir.Index
					cs.funcs[i] = f
					break
				}
			}
		}
	default:
		cs.addError(d.Pos(), "unexpected decl")
		return false
	}

	return true
}

// functionReturnTypes returns the original returning value types, if the given expression is call.
//
// Note that parseExpr returns the returning types for IR, not the original function.
func (cs *compileState) functionReturnTypes(block *block, expr ast.Expr) ([]shaderir.Type, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil, false
	}

	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil, false
	}

	for _, f := range cs.funcs {
		if f.name == ident.Name {
			// TODO: Is it correct to combine out-params and return param?
			ts := f.ir.OutParams
			if f.ir.Return.Main != shaderir.None {
				ts = append(ts, f.ir.Return)
			}
			return ts, true
		}
	}
	return nil, false
}

func (s *compileState) parseVariable(block *block, vs *ast.ValueSpec) ([]variable, []shaderir.Expr, []shaderir.Stmt, bool) {
	if len(vs.Names) != len(vs.Values) && len(vs.Values) != 1 && len(vs.Values) != 0 {
		s.addError(vs.Pos(), fmt.Sprintf("the numbers of lhs and rhs don't match"))
		return nil, nil, nil, false
	}

	var declt shaderir.Type
	if vs.Type != nil {
		declt = s.parseType(vs.Type)
	}

	var (
		vars  []variable
		inits []shaderir.Expr
		stmts []shaderir.Stmt
	)

	// These variables are used only in multiple-value context.
	var inittypes []shaderir.Type
	var initexprs []shaderir.Expr

	for i, n := range vs.Names {
		t := declt
		switch {
		case len(vs.Values) == 0:
			// No initialization

		case len(vs.Names) == len(vs.Values):
			// Single-value context

			init := vs.Values[i]

			es, origts, ss, ok := s.parseExpr(block, init)
			if !ok {
				return nil, nil, nil, false
			}

			if t.Main == shaderir.None {
				ts, ok := s.functionReturnTypes(block, init)
				if !ok {
					ts = origts
				}
				if len(ts) > 1 {
					s.addError(vs.Pos(), fmt.Sprintf("the numbers of lhs and rhs don't match"))
				}
				t = ts[0]
			}

			if es[0].Type == shaderir.NumberExpr {
				switch t.Main {
				case shaderir.Int:
					es[0].ConstType = shaderir.ConstTypeInt
				case shaderir.Float:
					es[0].ConstType = shaderir.ConstTypeFloat
				}
			}

			inits = append(inits, es...)
			stmts = append(stmts, ss...)

		default:
			// Multiple-value context

			if i == 0 {
				init := vs.Values[0]

				var ss []shaderir.Stmt
				var ok bool
				initexprs, inittypes, ss, ok = s.parseExpr(block, init)
				if !ok {
					return nil, nil, nil, false
				}
				stmts = append(stmts, ss...)

				if t.Main == shaderir.None {
					ts, ok := s.functionReturnTypes(block, init)
					if ok {
						inittypes = ts
					}
					if len(ts) != len(vs.Names) {
						s.addError(vs.Pos(), fmt.Sprintf("the numbers of lhs and rhs don't match"))
						continue
					}
				}
			}
			if len(inittypes) > 0 {
				t = inittypes[i]
			}

			// Add the same initexprs for each variable.
			inits = append(inits, initexprs...)
		}

		name := n.Name
		vars = append(vars, variable{
			name: name,
			typ:  t,
		})
	}

	return vars, inits, stmts, true
}

func (s *compileState) parseConstant(vs *ast.ValueSpec) []constant {
	var t shaderir.Type
	if vs.Type != nil {
		t = s.parseType(vs.Type)
	}

	var cs []constant
	for i, n := range vs.Names {
		cs = append(cs, constant{
			name: n.Name,
			typ:  t,
			init: vs.Values[i],
		})
	}
	return cs
}

func (cs *compileState) parseFuncParams(d *ast.FuncDecl) (in, out []variable) {
	for _, f := range d.Type.Params.List {
		t := cs.parseType(f.Type)
		for _, n := range f.Names {
			in = append(in, variable{
				name: n.Name,
				typ:  t,
			})
		}
	}

	if d.Type.Results == nil {
		return
	}

	for _, f := range d.Type.Results.List {
		t := cs.parseType(f.Type)
		if len(f.Names) == 0 {
			out = append(out, variable{
				name: "",
				typ:  t,
			})
		} else {
			for _, n := range f.Names {
				out = append(out, variable{
					name: n.Name,
					typ:  t,
				})
			}
		}
	}
	return
}

func (cs *compileState) parseFunc(block *block, d *ast.FuncDecl) (function, bool) {
	if d.Name == nil {
		cs.addError(d.Pos(), "function must have a name")
		return function{}, false
	}
	if d.Body == nil {
		cs.addError(d.Pos(), "function must have a body")
		return function{}, false
	}

	inParams, outParams := cs.parseFuncParams(d)

	checkVaryings := func(vs []variable) {
		if len(cs.ir.Varyings) != len(vs) {
			cs.addError(d.Pos(), fmt.Sprintf("the number of vertex entry point's returning values and the number of framgent entry point's params must be the same"))
			return
		}
		for i, t := range cs.ir.Varyings {
			if t.Main != vs[i].typ.Main {
				cs.addError(d.Pos(), fmt.Sprintf("vertex entry point's returning value types and framgent entry point's param types must match"))
			}
		}
	}

	if block == &cs.global {
		switch d.Name.Name {
		case cs.vertexEntry:
			for _, v := range inParams {
				cs.ir.Attributes = append(cs.ir.Attributes, v.typ)
			}

			// The first out-param is treated as gl_Position in GLSL.
			if len(outParams) == 0 {
				cs.addError(d.Pos(), fmt.Sprintf("vertex entry point must have at least one returning vec4 value for a position"))
				return function{}, false
			}
			if outParams[0].typ.Main != shaderir.Vec4 {
				cs.addError(d.Pos(), fmt.Sprintf("vertex entry point must have at least one returning vec4 value for a position"))
				return function{}, false
			}

			if cs.varyingParsed {
				checkVaryings(outParams[1:])
			} else {
				for _, v := range outParams[1:] {
					// TODO: Check that these params are not arrays or structs
					cs.ir.Varyings = append(cs.ir.Varyings, v.typ)
				}
			}
			cs.varyingParsed = true
		case cs.fragmentEntry:
			if len(inParams) == 0 {
				cs.addError(d.Pos(), fmt.Sprintf("fragment entry point must have at least one vec4 parameter for a position"))
				return function{}, false
			}
			if inParams[0].typ.Main != shaderir.Vec4 {
				cs.addError(d.Pos(), fmt.Sprintf("fragment entry point must have at least one vec4 parameter for a position"))
				return function{}, false
			}

			if len(outParams) != 1 {
				cs.addError(d.Pos(), fmt.Sprintf("fragment entry point must have one returning vec4 value for a color"))
				return function{}, false
			}
			if outParams[0].typ.Main != shaderir.Vec4 {
				cs.addError(d.Pos(), fmt.Sprintf("fragment entry point must have one returning vec4 value for a color"))
				return function{}, false
			}

			if cs.varyingParsed {
				checkVaryings(inParams[1:])
			} else {
				for _, v := range inParams[1:] {
					cs.ir.Varyings = append(cs.ir.Varyings, v.typ)
				}
			}
			cs.varyingParsed = true
		}
	}

	b, ok := cs.parseBlock(block, d.Body, inParams, outParams)
	if !ok {
		return function{}, false
	}

	var inT, outT []shaderir.Type
	for _, v := range inParams {
		inT = append(inT, v.typ)
	}
	for _, v := range outParams {
		outT = append(outT, v.typ)
	}

	return function{
		name:  d.Name.Name,
		block: b,
		ir: shaderir.Func{
			InParams:  inT,
			OutParams: outT,
			Block:     b.ir,
		},
	}, true
}

func (cs *compileState) parseBlock(outer *block, b *ast.BlockStmt, inParams, outParams []variable) (*block, bool) {
	vars := make([]variable, 0, len(inParams)+len(outParams))
	vars = append(vars, inParams...)
	vars = append(vars, outParams...)
	block := &block{
		vars:  vars,
		outer: outer,
	}
	defer func() {
		for _, v := range block.vars[len(inParams)+len(outParams):] {
			block.ir.LocalVars = append(block.ir.LocalVars, v.typ)
		}
	}()

	for _, l := range b.List {
		switch l := l.(type) {
		case *ast.AssignStmt:
			switch l.Tok {
			case token.DEFINE:
				if len(l.Lhs) != len(l.Rhs) && len(l.Rhs) != 1 {
					cs.addError(l.Pos(), fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
					return nil, false
				}

				if !cs.assign(block, l.Pos(), l.Lhs, l.Rhs, true) {
					return nil, false
				}
			case token.ASSIGN:
				// TODO: What about the statement `a,b = b,a?`
				if len(l.Lhs) != len(l.Rhs) && len(l.Rhs) != 1 {
					cs.addError(l.Pos(), fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
					return nil, false
				}
				if !cs.assign(block, l.Pos(), l.Lhs, l.Rhs, false) {
					return nil, false
				}
			case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN, token.REM_ASSIGN:
				var op shaderir.Op
				switch l.Tok {
				case token.ADD_ASSIGN:
					op = shaderir.Add
				case token.SUB_ASSIGN:
					op = shaderir.Sub
				case token.MUL_ASSIGN:
					op = shaderir.Mul
				case token.QUO_ASSIGN:
					op = shaderir.Div
				case token.REM_ASSIGN:
					op = shaderir.ModOp
				}
				rhs, _, stmts, ok := cs.parseExpr(block, l.Rhs[0])
				if !ok {
					return nil, false
				}
				block.ir.Stmts = append(block.ir.Stmts, stmts...)
				lhs, _, stmts, ok := cs.parseExpr(block, l.Lhs[0])
				if !ok {
					return nil, false
				}
				block.ir.Stmts = append(block.ir.Stmts, stmts...)
				block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
					Type: shaderir.Assign,
					Exprs: []shaderir.Expr{
						lhs[0],
						{
							Type: shaderir.Binary,
							Op:   op,
							Exprs: []shaderir.Expr{
								lhs[0],
								rhs[0],
							},
						},
					},
				})
			default:
				cs.addError(l.Pos(), fmt.Sprintf("unexpected token: %s", l.Tok))
			}
		case *ast.BlockStmt:
			b, ok := cs.parseBlock(block, l, nil, nil)
			if !ok {
				return nil, false
			}
			block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
				Type: shaderir.BlockStmt,
				Blocks: []shaderir.Block{
					b.ir,
				},
			})
		case *ast.DeclStmt:
			if !cs.parseDecl(block, l.Decl) {
				return nil, false
			}
		case *ast.ReturnStmt:
			for i, r := range l.Results {
				exprs, _, stmts, ok := cs.parseExpr(block, r)
				if !ok {
					return nil, false
				}
				block.ir.Stmts = append(block.ir.Stmts, stmts...)
				if len(exprs) == 0 {
					continue
				}
				if len(exprs) > 1 {
					cs.addError(r.Pos(), "multiple-context with return is not implemented yet")
					continue
				}
				block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
					Type: shaderir.Assign,
					Exprs: []shaderir.Expr{
						{
							Type:  shaderir.LocalVariable,
							Index: len(inParams) + i,
						},
						exprs[0],
					},
				})
			}
			block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
				Type: shaderir.Return,
			})
		case *ast.ExprStmt:
			exprs, _, stmts, ok := cs.parseExpr(block, l.X)
			if !ok {
				return nil, false
			}
			block.ir.Stmts = append(block.ir.Stmts, stmts...)
			for _, expr := range exprs {
				if expr.Type != shaderir.Call {
					continue
				}
				block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
					Type:  shaderir.ExprStmt,
					Exprs: []shaderir.Expr{expr},
				})
			}
		default:
			cs.addError(l.Pos(), fmt.Sprintf("unexpected statement: %#v", l))
			return nil, false
		}
	}

	return block, true
}

func (cs *compileState) assign(block *block, pos token.Pos, lhs, rhs []ast.Expr, define bool) bool {
	var rhsExprs []shaderir.Expr
	var rhsTypes []shaderir.Type

	for i, e := range lhs {
		if len(lhs) == len(rhs) {
			// Prase RHS first for the order of the statements.
			r, origts, stmts, ok := cs.parseExpr(block, rhs[i])
			if !ok {
				return false
			}
			block.ir.Stmts = append(block.ir.Stmts, stmts...)

			if define {
				v := variable{
					name: e.(*ast.Ident).Name,
				}
				ts, ok := cs.functionReturnTypes(block, rhs[i])
				if !ok {
					ts = origts
				}
				if len(ts) > 1 {
					cs.addError(pos, fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
					return false
				}
				if len(ts) == 1 {
					v.typ = ts[0]
				}
				block.vars = append(block.vars, v)
			}

			if len(r) > 1 {
				cs.addError(pos, fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
				return false
			}

			l, _, stmts, ok := cs.parseExpr(block, lhs[i])
			if !ok {
				return false
			}
			block.ir.Stmts = append(block.ir.Stmts, stmts...)

			if r[0].Type == shaderir.NumberExpr {
				switch block.vars[l[0].Index].typ.Main {
				case shaderir.Int:
					r[0].ConstType = shaderir.ConstTypeInt
				case shaderir.Float:
					r[0].ConstType = shaderir.ConstTypeFloat
				}
			}

			block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
				Type:  shaderir.Assign,
				Exprs: []shaderir.Expr{l[0], r[0]},
			})
		} else {
			if i == 0 {
				var stmts []shaderir.Stmt
				var ok bool
				rhsExprs, rhsTypes, stmts, ok = cs.parseExpr(block, rhs[0])
				if !ok {
					return false
				}
				if len(rhsExprs) != len(lhs) {
					cs.addError(pos, fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
				}
				block.ir.Stmts = append(block.ir.Stmts, stmts...)
			}

			if define {
				v := variable{
					name: e.(*ast.Ident).Name,
				}
				v.typ = rhsTypes[i]
				block.vars = append(block.vars, v)
			}

			l, _, stmts, ok := cs.parseExpr(block, lhs[i])
			if !ok {
				return false
			}
			block.ir.Stmts = append(block.ir.Stmts, stmts...)

			block.ir.Stmts = append(block.ir.Stmts, shaderir.Stmt{
				Type:  shaderir.Assign,
				Exprs: []shaderir.Expr{l[0], rhsExprs[i]},
			})
		}
	}
	return true
}

func (cs *compileState) parseExpr(block *block, expr ast.Expr) ([]shaderir.Expr, []shaderir.Type, []shaderir.Stmt, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		switch e.Kind {
		case token.INT:
			return []shaderir.Expr{
				{
					Type:  shaderir.NumberExpr,
					Const: gconstant.MakeFromLiteral(e.Value, e.Kind, 0),
				},
			}, []shaderir.Type{{Main: shaderir.Int}}, nil, true
		case token.FLOAT:
			return []shaderir.Expr{
				{
					Type:  shaderir.NumberExpr,
					Const: gconstant.MakeFromLiteral(e.Value, e.Kind, 0),
				},
			}, []shaderir.Type{{Main: shaderir.Float}}, nil, true
		default:
			cs.addError(e.Pos(), fmt.Sprintf("literal not implemented: %#v", e))
		}
	case *ast.BinaryExpr:
		var stmts []shaderir.Stmt

		// Prase LHS first for the order of the statements.
		lhs, ts, ss, ok := cs.parseExpr(block, e.X)
		if !ok {
			return nil, nil, nil, false
		}
		if len(lhs) != 1 {
			cs.addError(e.Pos(), fmt.Sprintf("multiple-value context is not available at a binary operator: %s", e.X))
			return nil, nil, nil, false
		}
		stmts = append(stmts, ss...)
		lhst := ts[0]

		rhs, ts, ss, ok := cs.parseExpr(block, e.Y)
		if !ok {
			return nil, nil, nil, false
		}
		if len(rhs) != 1 {
			cs.addError(e.Pos(), fmt.Sprintf("multiple-value context is not available at a binary operator: %s", e.Y))
			return nil, nil, nil, false
		}
		stmts = append(stmts, ss...)
		rhst := ts[0]

		if lhs[0].Type == shaderir.NumberExpr && rhs[0].Type == shaderir.NumberExpr {
			op := e.Op
			// https://golang.org/pkg/go/constant/#BinaryOp
			// "To force integer division of Int operands, use op == token.QUO_ASSIGN instead of
			// token.QUO; the result is guaranteed to be Int in this case."
			if op == token.QUO && lhs[0].Const.Kind() == gconstant.Int && rhs[0].Const.Kind() == gconstant.Int {
				op = token.QUO_ASSIGN
			}
			v := gconstant.BinaryOp(lhs[0].Const, op, rhs[0].Const)
			t := shaderir.Type{Main: shaderir.Int}
			if v.Kind() == gconstant.Float {
				t = shaderir.Type{Main: shaderir.Float}
			}
			return []shaderir.Expr{
				{
					Type:  shaderir.NumberExpr,
					Const: v,
				},
			}, []shaderir.Type{t}, stmts, true
		}

		var t shaderir.Type
		if lhst.Equal(&rhst) {
			t = lhst
		} else if lhst.Main == shaderir.Float || lhst.Main == shaderir.Int {
			switch rhst.Main {
			case shaderir.Int:
				t = lhst
			case shaderir.Float, shaderir.Vec2, shaderir.Vec3, shaderir.Vec4, shaderir.Mat2, shaderir.Mat3, shaderir.Mat4:
				t = rhst
			default:
				cs.addError(e.Pos(), fmt.Sprintf("types don't match: %s %s %s", lhst.String(), e.Op, rhst.String()))
				return nil, nil, nil, false
			}
		} else if rhst.Main == shaderir.Float || rhst.Main == shaderir.Int {
			switch lhst.Main {
			case shaderir.Int:
				t = rhst
			case shaderir.Float, shaderir.Vec2, shaderir.Vec3, shaderir.Vec4, shaderir.Mat2, shaderir.Mat3, shaderir.Mat4:
				t = lhst
			default:
				cs.addError(e.Pos(), fmt.Sprintf("types don't match: %s %s %s", lhst.String(), e.Op, rhst.String()))
				return nil, nil, nil, false
			}
		}

		op, ok := shaderir.OpFromToken(e.Op)
		if !ok {
			cs.addError(e.Pos(), fmt.Sprintf("unexpected operator: %s", e.Op))
			return nil, nil, nil, false
		}

		return []shaderir.Expr{
			{
				Type:  shaderir.Binary,
				Op:    op,
				Exprs: []shaderir.Expr{lhs[0], rhs[0]},
			},
		}, []shaderir.Type{t}, stmts, true
	case *ast.CallExpr:
		var (
			callee shaderir.Expr
			args   []shaderir.Expr
			argts  []shaderir.Type
			stmts  []shaderir.Stmt
		)

		// Parse the argument first for the order of the statements.
		for _, a := range e.Args {
			es, ts, ss, ok := cs.parseExpr(block, a)
			if !ok {
				return nil, nil, nil, false
			}
			if len(es) > 1 && len(e.Args) > 1 {
				cs.addError(e.Pos(), fmt.Sprintf("single-value context and multiple-value context cannot be mixed: %s", e.Fun))
				return nil, nil, nil, false
			}
			args = append(args, es...)
			argts = append(argts, ts...)
			stmts = append(stmts, ss...)
		}

		// TODO: When len(ss) is not 0?
		es, _, ss, ok := cs.parseExpr(block, e.Fun)
		if !ok {
			return nil, nil, nil, false
		}
		if len(es) != 1 {
			cs.addError(e.Pos(), fmt.Sprintf("multiple-value context is not available at a callee: %s", e.Fun))
			return nil, nil, nil, false
		}
		callee = es[0]
		stmts = append(stmts, ss...)

		// For built-in functions, we can call this in this position. Return an expression for the function
		// call.
		if callee.Type == shaderir.BuiltinFuncExpr {
			var t shaderir.Type
			switch callee.BuiltinFunc {
			case shaderir.Vec2F:
				t = shaderir.Type{Main: shaderir.Vec2}
			case shaderir.Vec3F:
				t = shaderir.Type{Main: shaderir.Vec3}
			case shaderir.Vec4F:
				t = shaderir.Type{Main: shaderir.Vec4}
			case shaderir.Mat2F:
				t = shaderir.Type{Main: shaderir.Mat2}
			case shaderir.Mat3F:
				t = shaderir.Type{Main: shaderir.Mat3}
			case shaderir.Mat4F:
				t = shaderir.Type{Main: shaderir.Mat4}
			case shaderir.Step:
				t = argts[1]
			case shaderir.Smoothstep:
				t = argts[2]
			case shaderir.Length, shaderir.Distance, shaderir.Dot:
				t = shaderir.Type{Main: shaderir.Float}
			case shaderir.Cross:
				t = shaderir.Type{Main: shaderir.Vec3}
			case shaderir.Texture2DF:
				t = shaderir.Type{Main: shaderir.Vec4}
			default:
				t = argts[0]
			}
			return []shaderir.Expr{
				{
					Type:  shaderir.Call,
					Exprs: append([]shaderir.Expr{callee}, args...),
				},
			}, []shaderir.Type{t}, stmts, true
		}

		if callee.Type != shaderir.FunctionExpr {
			cs.addError(e.Pos(), fmt.Sprintf("function callee must be a funciton name but %s", e.Fun))
			return nil, nil, nil, false
		}

		f := cs.funcs[callee.Index]

		var outParams []int
		for _, p := range f.ir.OutParams {
			idx := len(block.vars)
			block.vars = append(block.vars, variable{
				typ: p,
			})
			args = append(args, shaderir.Expr{
				Type:  shaderir.LocalVariable,
				Index: idx,
			})
			outParams = append(outParams, idx)
		}

		if t := f.ir.Return; t.Main != shaderir.None {
			if len(outParams) != 0 {
				cs.addError(e.Pos(), fmt.Sprintf("a function returning value cannot have out-params so far: %s", e.Fun))
				return nil, nil, nil, false
			}

			idx := len(block.vars)
			block.vars = append(block.vars, variable{
				typ: t,
			})

			// Calling the function should be done eariler to treat out-params correctly.
			stmts = append(stmts, shaderir.Stmt{
				Type: shaderir.Assign,
				Exprs: []shaderir.Expr{
					{
						Type:  shaderir.LocalVariable,
						Index: idx,
					},
					{
						Type:  shaderir.Call,
						Exprs: append([]shaderir.Expr{callee}, args...),
					},
				},
			})

			// The actual expression here is just a local variable that includes the result of the
			// function call.
			return []shaderir.Expr{
				{
					Type:  shaderir.LocalVariable,
					Index: idx,
				},
			}, []shaderir.Type{t}, stmts, true
		}

		// Even if the function doesn't return anything, calling the function should be done eariler to keep
		// the evaluation order.
		stmts = append(stmts, shaderir.Stmt{
			Type: shaderir.ExprStmt,
			Exprs: []shaderir.Expr{
				{
					Type:  shaderir.Call,
					Exprs: append([]shaderir.Expr{callee}, args...),
				},
			},
		})

		if len(outParams) == 0 {
			// TODO: Is this an error?
		}

		var exprs []shaderir.Expr
		for _, p := range outParams {
			exprs = append(exprs, shaderir.Expr{
				Type:  shaderir.LocalVariable,
				Index: p,
			})
		}
		return exprs, f.ir.OutParams, stmts, true

	case *ast.Ident:
		if i, t, ok := block.findLocalVariable(e.Name); ok {
			return []shaderir.Expr{
				{
					Type:  shaderir.LocalVariable,
					Index: i,
				},
			}, []shaderir.Type{t}, nil, true
		}
		if i, ok := cs.findFunction(e.Name); ok {
			return []shaderir.Expr{
				{
					Type:  shaderir.FunctionExpr,
					Index: i,
				},
			}, nil, nil, true
		}
		if i, ok := cs.findUniformVariable(e.Name); ok {
			return []shaderir.Expr{
				{
					Type:  shaderir.UniformVariable,
					Index: i,
				},
			}, []shaderir.Type{cs.ir.Uniforms[i]}, nil, true
		}
		if f, ok := shaderir.ParseBuiltinFunc(e.Name); ok {
			return []shaderir.Expr{
				{
					Type:        shaderir.BuiltinFuncExpr,
					BuiltinFunc: f,
				},
			}, nil, nil, true
		}
		cs.addError(e.Pos(), fmt.Sprintf("unexpected identifier: %s", e.Name))

	case *ast.ParenExpr:
		return cs.parseExpr(block, e.X)

	case *ast.SelectorExpr:
		exprs, _, stmts, ok := cs.parseExpr(block, e.X)
		if !ok {
			return nil, nil, nil, false
		}
		if len(exprs) != 1 {
			cs.addError(e.Pos(), fmt.Sprintf("multiple-value context is not available at a selector: %s", e.X))
			return nil, nil, nil, false
		}
		var t shaderir.Type
		switch len(e.Sel.Name) {
		case 1:
			t.Main = shaderir.Float
		case 2:
			t.Main = shaderir.Vec2
		case 3:
			t.Main = shaderir.Vec3
		case 4:
			t.Main = shaderir.Vec4
		default:
			cs.addError(e.Pos(), fmt.Sprintf("unexpected swizzling: %s", e.Sel.Name))
			return nil, nil, nil, false
		}
		return []shaderir.Expr{
			{
				Type: shaderir.FieldSelector,
				Exprs: []shaderir.Expr{
					exprs[0],
					{
						Type:      shaderir.SwizzlingExpr,
						Swizzling: e.Sel.Name,
					},
				},
			},
		}, []shaderir.Type{t}, stmts, true

	case *ast.UnaryExpr:
		exprs, t, stmts, ok := cs.parseExpr(block, e.X)
		if !ok {
			return nil, nil, nil, false
		}
		if len(exprs) != 1 {
			cs.addError(e.Pos(), fmt.Sprintf("multiple-value context is not available at a unary operator: %s", e.X))
			return nil, nil, nil, false
		}

		if exprs[0].Type == shaderir.NumberExpr {
			v := gconstant.UnaryOp(e.Op, exprs[0].Const, 0)
			t := shaderir.Type{Main: shaderir.Int}
			if v.Kind() == gconstant.Float {
				t = shaderir.Type{Main: shaderir.Float}
			}
			return []shaderir.Expr{
				{
					Type:  shaderir.NumberExpr,
					Const: v,
				},
			}, []shaderir.Type{t}, stmts, true
		}

		var op shaderir.Op
		switch e.Op {
		case token.ADD:
			op = shaderir.Add
		case token.SUB:
			op = shaderir.Sub
		case token.NOT:
			op = shaderir.NotOp
		default:
			cs.addError(e.Pos(), fmt.Sprintf("unexpected operator: %s", e.Op))
			return nil, nil, nil, false
		}
		return []shaderir.Expr{
			{
				Type:  shaderir.Unary,
				Op:    op,
				Exprs: exprs,
			},
		}, t, stmts, true
	default:
		cs.addError(e.Pos(), fmt.Sprintf("expression not implemented: %#v", e))
	}
	return nil, nil, nil, false
}
