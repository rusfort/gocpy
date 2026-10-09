// Package ast declares the syntax tree of Gocpy.
package ast

import "github.com/rusfort/gocpy/internal/token"

// Node is any AST node.
type Node interface{ Pos() token.Pos }

// Expr is an expression or a type expression.
type Expr interface {
	Node
	exprNode()
}

// Stmt is a statement.
type Stmt interface {
	Node
	stmtNode()
}

// Decl is a top-level (or block-level) declaration.
type Decl interface {
	Node
	declNode()
}

// Annotation is `@name` or `@name(args...)` placed before a declaration,
// struct field or interface method: @inline, @throws(IOError), @overload ...
type Annotation struct {
	At   token.Pos
	Name string
	Args []Expr
}

func (a *Annotation) Pos() token.Pos { return a.At }

// Annotations is a list with helpers.
type Annotations []*Annotation

// Get returns the first annotation with the given name or nil.
func (as Annotations) Get(name string) *Annotation {
	for _, a := range as {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Has reports whether an annotation with the given name exists.
func (as Annotations) Has(name string) bool { return as.Get(name) != nil }

// ---------------------------------------------------------------- File

// File is one .gocpy source file.
type File struct {
	Package token.Pos
	Name    *Ident
	Imports []*ImportSpec
	Decls   []Decl
	// Directives: line -> `//gocpy:` comments on that line (e.g. "ignore:unused").
	Directives map[int][]string
}

func (f *File) Pos() token.Pos { return f.Package }

// ImportSpec: import [alias] "path"
type ImportSpec struct {
	Alias *Ident // may be nil
	Path  *BasicLit
}

func (s *ImportSpec) Pos() token.Pos { return s.Path.ValuePos }

// ---------------------------------------------------------------- Expressions

type (
	Ident struct {
		NamePos token.Pos
		Name    string
	}

	BasicLit struct {
		ValuePos token.Pos
		Kind     token.Kind // INT, FLOAT, CHAR, STRING
		Value    string
	}

	// FString is f"text {expr} text {expr:spec}".
	FString struct {
		StrPos token.Pos
		Parts  []*FStringPart
	}

	// FStringPart is either literal text (Expr == nil) or an interpolation.
	FStringPart struct {
		Text string
		Expr Expr
		Spec string // format spec after ':' e.g. ".2f"
	}

	CompositeLit struct {
		Type   Expr // nil for elided types: {1, 2} inside []T{...}
		Lbrace token.Pos
		Elts   []Expr
	}

	KeyValueExpr struct {
		Key   Expr
		Value Expr
	}

	FuncLit struct {
		Type *FuncType
		Body *BlockStmt
	}

	ParenExpr struct {
		Lparen token.Pos
		X      Expr
	}

	SelectorExpr struct {
		X   Expr
		Sel *Ident
	}

	// IndexExpr covers a[i] and generic instantiation List[int, string].
	IndexExpr struct {
		X       Expr
		Indices []Expr
	}

	SliceExpr struct {
		X              Expr
		Low, High, Max Expr
		Slice3         bool
	}

	// TypeAssertExpr: x.(T); Type == nil means x.(type) in type switches.
	TypeAssertExpr struct {
		X    Expr
		Type Expr
	}

	CallExpr struct {
		Fun      Expr
		Args     []Expr
		Ellipsis bool // f(xs...)
	}

	// StarExpr is *x: pointer type or dereference.
	StarExpr struct {
		Star token.Pos
		X    Expr
	}

	UnaryExpr struct {
		OpPos token.Pos
		Op    token.Kind
		X     Expr
	}

	BinaryExpr struct {
		X     Expr
		OpPos token.Pos
		Op    token.Kind
		Y     Expr
	}

	// PropagateExpr is x? — returns early with the error part of x.
	PropagateExpr struct {
		X        Expr
		Question token.Pos
	}

	// ComprehensionExpr: [Elt for Key, Value in Iter if Cond]
	ComprehensionExpr struct {
		Lbrack token.Pos
		Elt    Expr
		Key    *Ident // may be nil
		Value  *Ident
		Iter   Expr
		Cond   Expr // may be nil
	}

	// IntrinsicExpr: @name(args) used in expression position, e.g. @be_to_le(x).
	IntrinsicExpr struct {
		At   token.Pos
		Name string
		Args []Expr
	}
)

// ---------------------------------------------------------------- Types

type (
	// ArrayType: [N]T, or []T when Len == nil.
	ArrayType struct {
		Lbrack token.Pos
		Len    Expr
		Elt    Expr
	}

	MapType struct {
		Map   token.Pos
		Key   Expr
		Value Expr
	}

	ChanType struct {
		Begin token.Pos
		Value Expr
	}

	// Ellipsis is ...T in a variadic parameter.
	Ellipsis struct {
		EllipsisPos token.Pos
		Elt         Expr
	}

	Field struct {
		Annots Annotations
		Names  []*Ident // nil for embedded fields / unnamed params
		Type   Expr
		Tag    *BasicLit
	}

	FieldList struct {
		Opening token.Pos
		List    []*Field
	}

	FuncType struct {
		Func       token.Pos
		Annots     Annotations // e.g. @throws on function types
		TypeParams *FieldList
		Params     *FieldList
		Results    *FieldList // may be nil
	}

	StructType struct {
		Struct token.Pos
		Fields *FieldList
	}

	// InterfaceType: methods are Fields with one Name and FuncType;
	// type-set elements (int | ~float64) are Fields without Names.
	InterfaceType struct {
		Interface token.Pos
		Methods   *FieldList
	}
)

func (x *Ident) Pos() token.Pos             { return x.NamePos }
func (x *BasicLit) Pos() token.Pos          { return x.ValuePos }
func (x *FString) Pos() token.Pos           { return x.StrPos }
func (x *FStringPart) Pos() token.Pos       { return token.Pos{} }
func (x *CompositeLit) Pos() token.Pos      { return posOr(x.Type, x.Lbrace) }
func (x *KeyValueExpr) Pos() token.Pos      { return x.Key.Pos() }
func (x *FuncLit) Pos() token.Pos           { return x.Type.Func }
func (x *ParenExpr) Pos() token.Pos         { return x.Lparen }
func (x *SelectorExpr) Pos() token.Pos      { return x.X.Pos() }
func (x *IndexExpr) Pos() token.Pos         { return x.X.Pos() }
func (x *SliceExpr) Pos() token.Pos         { return x.X.Pos() }
func (x *TypeAssertExpr) Pos() token.Pos    { return x.X.Pos() }
func (x *CallExpr) Pos() token.Pos          { return x.Fun.Pos() }
func (x *StarExpr) Pos() token.Pos          { return x.Star }
func (x *UnaryExpr) Pos() token.Pos         { return x.OpPos }
func (x *BinaryExpr) Pos() token.Pos        { return x.X.Pos() }
func (x *PropagateExpr) Pos() token.Pos     { return x.X.Pos() }
func (x *ComprehensionExpr) Pos() token.Pos { return x.Lbrack }
func (x *IntrinsicExpr) Pos() token.Pos     { return x.At }
func (x *ArrayType) Pos() token.Pos         { return x.Lbrack }
func (x *MapType) Pos() token.Pos           { return x.Map }
func (x *ChanType) Pos() token.Pos          { return x.Begin }
func (x *Ellipsis) Pos() token.Pos          { return x.EllipsisPos }
func (x *FuncType) Pos() token.Pos          { return x.Func }
func (x *StructType) Pos() token.Pos        { return x.Struct }
func (x *InterfaceType) Pos() token.Pos     { return x.Interface }
func (x *Field) Pos() token.Pos {
	if len(x.Names) > 0 {
		return x.Names[0].Pos()
	}
	return x.Type.Pos()
}
func (x *FieldList) Pos() token.Pos { return x.Opening }

func (*Ident) exprNode()             {}
func (*BasicLit) exprNode()          {}
func (*FString) exprNode()           {}
func (*CompositeLit) exprNode()      {}
func (*KeyValueExpr) exprNode()      {}
func (*FuncLit) exprNode()           {}
func (*ParenExpr) exprNode()         {}
func (*SelectorExpr) exprNode()      {}
func (*IndexExpr) exprNode()         {}
func (*SliceExpr) exprNode()         {}
func (*TypeAssertExpr) exprNode()    {}
func (*CallExpr) exprNode()          {}
func (*StarExpr) exprNode()          {}
func (*UnaryExpr) exprNode()         {}
func (*BinaryExpr) exprNode()        {}
func (*PropagateExpr) exprNode()     {}
func (*ComprehensionExpr) exprNode() {}
func (*IntrinsicExpr) exprNode()     {}
func (*ArrayType) exprNode()         {}
func (*MapType) exprNode()           {}
func (*ChanType) exprNode()          {}
func (*Ellipsis) exprNode()          {}
func (*FuncType) exprNode()          {}
func (*StructType) exprNode()        {}
func (*InterfaceType) exprNode()     {}

func posOr(e Expr, p token.Pos) token.Pos {
	if e != nil {
		return e.Pos()
	}
	return p
}

// ---------------------------------------------------------------- Statements

type (
	BadStmt   struct{ From token.Pos }
	DeclStmt  struct{ Decl Decl }
	EmptyStmt struct {
		Semicolon token.Pos
	}
	ExprStmt struct{ X Expr }

	SendStmt struct {
		Chan  Expr
		Arrow token.Pos
		Value Expr
	}

	IncDecStmt struct {
		X   Expr
		Tok token.Kind // INC or DEC
	}

	AssignStmt struct {
		Lhs    []Expr
		TokPos token.Pos
		Tok    token.Kind // ASSIGN, DEFINE, ADD_ASSIGN, ...
		Rhs    []Expr
	}

	GoStmt struct {
		Go   token.Pos
		Call *CallExpr
	}

	DeferStmt struct {
		Defer token.Pos
		Call  *CallExpr
	}

	ReturnStmt struct {
		Return  token.Pos
		Results []Expr
	}

	BranchStmt struct {
		TokPos token.Pos
		Tok    token.Kind // BREAK, CONTINUE, FALLTHROUGH
		Label  *Ident
	}

	BlockStmt struct {
		Lbrace token.Pos
		List   []Stmt
	}

	IfStmt struct {
		If   token.Pos
		Init Stmt
		Cond Expr
		Body *BlockStmt
		Else Stmt // *IfStmt or *BlockStmt
	}

	// ForStmt: for {}, for cond {}, for init; cond; post {}
	ForStmt struct {
		For  token.Pos
		Init Stmt
		Cond Expr
		Post Stmt
		Body *BlockStmt
	}

	// ForInStmt: for v in xs {}, for k, v in xs {}
	ForInStmt struct {
		For   token.Pos
		Key   *Ident // may be nil
		Value *Ident
		Iter  Expr
		Body  *BlockStmt
	}

	CaseClause struct {
		Case token.Pos
		List []Expr // nil means default
		Body []Stmt
	}

	SwitchStmt struct {
		Switch token.Pos
		Init   Stmt
		Tag    Stmt // ExprStmt or AssignStmt (type switch); may be nil
		Body   []*CaseClause
	}

	CommClause struct {
		Case token.Pos
		Comm Stmt // nil means default
		Body []Stmt
	}

	SelectStmt struct {
		Select token.Pos
		Body   []*CommClause
	}

	ThrowStmt struct {
		Throw token.Pos
		X     Expr
	}

	// CatchClause: catch {}, catch e {}, catch e: T {}
	CatchClause struct {
		Catch token.Pos
		Name  *Ident // may be nil
		Type  Expr   // nil = catch everything
		Body  *BlockStmt
	}

	TryStmt struct {
		Try     token.Pos
		Body    *BlockStmt
		Catches []*CatchClause
		Finally *BlockStmt // may be nil
	}

	// UnsafeStmt: unsafe { ... } — enables malloc/free and pointer arithmetic.
	UnsafeStmt struct {
		Unsafe token.Pos
		Body   *BlockStmt
	}
)

func (s *BadStmt) Pos() token.Pos     { return s.From }
func (s *DeclStmt) Pos() token.Pos    { return s.Decl.Pos() }
func (s *EmptyStmt) Pos() token.Pos   { return s.Semicolon }
func (s *ExprStmt) Pos() token.Pos    { return s.X.Pos() }
func (s *SendStmt) Pos() token.Pos    { return s.Chan.Pos() }
func (s *IncDecStmt) Pos() token.Pos  { return s.X.Pos() }
func (s *AssignStmt) Pos() token.Pos  { return s.Lhs[0].Pos() }
func (s *GoStmt) Pos() token.Pos      { return s.Go }
func (s *DeferStmt) Pos() token.Pos   { return s.Defer }
func (s *ReturnStmt) Pos() token.Pos  { return s.Return }
func (s *BranchStmt) Pos() token.Pos  { return s.TokPos }
func (s *BlockStmt) Pos() token.Pos   { return s.Lbrace }
func (s *IfStmt) Pos() token.Pos      { return s.If }
func (s *ForStmt) Pos() token.Pos     { return s.For }
func (s *ForInStmt) Pos() token.Pos   { return s.For }
func (s *CaseClause) Pos() token.Pos  { return s.Case }
func (s *SwitchStmt) Pos() token.Pos  { return s.Switch }
func (s *CommClause) Pos() token.Pos  { return s.Case }
func (s *SelectStmt) Pos() token.Pos  { return s.Select }
func (s *ThrowStmt) Pos() token.Pos   { return s.Throw }
func (s *CatchClause) Pos() token.Pos { return s.Catch }
func (s *TryStmt) Pos() token.Pos     { return s.Try }
func (s *UnsafeStmt) Pos() token.Pos  { return s.Unsafe }

func (*BadStmt) stmtNode()     {}
func (*DeclStmt) stmtNode()    {}
func (*EmptyStmt) stmtNode()   {}
func (*ExprStmt) stmtNode()    {}
func (*SendStmt) stmtNode()    {}
func (*IncDecStmt) stmtNode()  {}
func (*AssignStmt) stmtNode()  {}
func (*GoStmt) stmtNode()      {}
func (*DeferStmt) stmtNode()   {}
func (*ReturnStmt) stmtNode()  {}
func (*BranchStmt) stmtNode()  {}
func (*BlockStmt) stmtNode()   {}
func (*IfStmt) stmtNode()      {}
func (*ForStmt) stmtNode()     {}
func (*ForInStmt) stmtNode()   {}
func (*CaseClause) stmtNode()  {}
func (*SwitchStmt) stmtNode()  {}
func (*CommClause) stmtNode()  {}
func (*SelectStmt) stmtNode()  {}
func (*ThrowStmt) stmtNode()   {}
func (*CatchClause) stmtNode() {}
func (*TryStmt) stmtNode()     {}
func (*UnsafeStmt) stmtNode()  {}

// ---------------------------------------------------------------- Declarations

type (
	// TypeSpec: Name[TypeParams] Type, or alias Name = Type.
	TypeSpec struct {
		Name       *Ident
		TypeParams *FieldList
		Alias      bool
		Type       Expr
	}

	// ValueSpec: names [type] [= values]
	ValueSpec struct {
		Names  []*Ident
		Type   Expr
		Values []Expr
	}

	// GenDecl is type/var/const, single or grouped with ( ... ).
	GenDecl struct {
		Annots Annotations
		TokPos token.Pos
		Tok    token.Kind // TYPE, VAR, CONST
		Specs  []Node     // *TypeSpec or *ValueSpec
	}

	// FuncDecl is a function, method or operator method.
	FuncDecl struct {
		Annots     Annotations
		Recv       *FieldList // nil for functions
		Name       *Ident     // operator methods use names like "+", "==", "[]"
		IsOperator bool
		Type       *FuncType
		Body       *BlockStmt // nil for external (C/Go) declarations
	}
)

func (d *TypeSpec) Pos() token.Pos  { return d.Name.Pos() }
func (d *ValueSpec) Pos() token.Pos { return d.Names[0].Pos() }
func (d *GenDecl) Pos() token.Pos   { return d.TokPos }
func (d *FuncDecl) Pos() token.Pos  { return d.Type.Func }

func (*GenDecl) declNode()  {}
func (*FuncDecl) declNode() {}
