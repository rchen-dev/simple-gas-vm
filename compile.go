package gasvm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"math"
	"strconv"
	"strings"
)

// CompileError describes an unsupported construct or a type error.
type CompileError struct {
	Line    int
	Message string
}

func (e CompileError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Message) }

const maxSource = 1 << 20

// Compile translates a Go-like decision script to bytecode. The VM's host
// signatures and constants are available to the script.
func (vm *VM) Compile(source string) (program *Program, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if e, ok := recovered.(CompileError); ok {
				err = e
				return
			}
			panic(recovered)
		}
	}()
	if len(source) > maxSource {
		return nil, fmt.Errorf("source exceeds %d bytes", maxSource)
	}
	wrapped, err := wrapSource(source)
	if err != nil {
		return nil, err
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "script", wrapped, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	c := compiler{
		vm: vm, files: files, program: &Program{entries: make(map[string]*entryPoint)},
		hostIDs: make(map[string]int),
	}
	for i, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || fn.Type.TypeParams != nil {
			c.fail(decl.Pos(), "expected an entry declaration")
		}
		name := fn.Name.Name
		if i == 0 {
			name = ""
		} else if !validName(name) || c.program.entries[name] != nil {
			c.fail(fn.Name.Pos(), "invalid or duplicate entry %q", name)
		}
		if len(c.program.entries) >= maxItems {
			c.fail(fn.Pos(), "too many entries")
		}
		c.compileEntry(name, fn)
	}
	c.compactHosts()
	if _, err := c.program.MarshalBinary(); err != nil {
		return nil, err
	}
	return c.program, nil
}

// Use Go's lexer and parser for Go syntax. Only the script envelope and
// top-level entry keyword need translation.
func wrapSource(source string) (string, error) {
	files := token.NewFileSet()
	file := files.AddFile("script", -1, len(source))
	var lex scanner.Scanner
	var scanErr error
	lex.Init(file, []byte(source), func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = CompileError{pos.Line, msg}
		}
	}, 0)
	var adapted strings.Builder
	from, depth := 0, 0
	adapted.WriteString("package script\nfunc __top__() {\n//line script:1\n")
	named := false
	for {
		pos, tok, lit := lex.Scan()
		at := file.Position(pos)
		if scanErr != nil {
			return "", scanErr
		}
		switch tok {
		case token.EOF:
			adapted.WriteString(source[from:])
			if !named {
				adapted.WriteString("\n}")
			}
			return adapted.String(), nil
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
			if depth > MaxNesting {
				return "", CompileError{at.Line, "maximum source nesting exceeded"}
			}
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
			if depth < 0 {
				return "", CompileError{at.Line, "unmatched closing delimiter"}
			}
		case token.FUNC:
			return "", CompileError{at.Line, "script functions are unsupported; use entry for host-callable blocks"}
		case token.IDENT:
			if depth == 0 && lit == "entry" {
				adapted.WriteString(source[from:at.Offset])
				if !named {
					fmt.Fprintf(&adapted, "\n}\n//line script:%d\n", at.Line)
					named = true
				}
				adapted.WriteString("func ")
				from = at.Offset + len(lit)
			}
		}
	}
}

type binding struct {
	slot int
	t    Type
}

type control struct {
	loop      bool
	breaks    []int
	continues []int
}

type compiler struct {
	vm             *VM
	files          *token.FileSet
	program        *Program
	hostIDs        map[string]int
	entry          *entryPoint
	topLevel       bool
	scopes         []map[string]binding
	control        []*control
	statementDepth int
}

func (c *compiler) fail(pos token.Pos, format string, args ...any) {
	panic(CompileError{c.files.Position(pos).Line, fmt.Sprintf(format, args...)})
}

func (c *compiler) scriptType(expr ast.Expr) Type {
	if name, ok := expr.(*ast.Ident); ok {
		switch name.Name {
		case "int", "int32":
			return Int
		case "bool":
			return Bool
		}
	}
	c.fail(expr.Pos(), "expected int, int32, or bool")
	return Void
}

func (c *compiler) compileEntry(name string, fn *ast.FuncDecl) {
	c.topLevel = name == ""
	c.entry = &entryPoint{}
	c.scopes = []map[string]binding{{}}
	for _, field := range fn.Type.Params.List {
		t := c.scriptType(field.Type)
		if len(field.Names) == 0 {
			c.fail(field.Pos(), "entry parameters must have names")
		}
		for _, id := range field.Names {
			c.declare(id, t)
			c.entry.params = append(c.entry.params, t)
		}
	}
	if results := fn.Type.Results; results != nil {
		if len(results.List) != 1 || len(results.List[0].Names) != 0 {
			c.fail(results.Pos(), "expected a single unnamed return type")
		}
		c.entry.result = c.scriptType(results.List[0].Type)
	}
	c.statements(fn.Body.List)
	end := c.emit(OpReturn, 0)
	if c.entry.result != Void && reachable(c.entry.code, 0)[end] {
		c.fail(fn.Body.End(), "entry %q can finish without returning a value", name)
	}
	c.entry.code = optimize(c.entry.code)
	if !verify(c.entry, c.program.hosts, c.topLevel) {
		c.fail(fn.Pos(), "entry exceeds bytecode or operand stack limits")
	}
	c.program.entries[name] = c.entry
}

func (c *compiler) emit(op Opcode, arg int) int {
	code := c.entry.code
	if len(code) >= maxCode {
		c.fail(token.NoPos, "entry exceeds %d instructions", maxCode)
	}
	c.entry.code = append(code, instruction{op, arg})
	return len(code)
}

func (c *compiler) pc() int { return len(c.entry.code) }

func (c *compiler) patch(at, target int) {
	if at >= 0 {
		c.entry.code[at].arg = target
	}
}

func (c *compiler) scope() func() {
	c.scopes = append(c.scopes, make(map[string]binding))
	return func() { c.scopes = c.scopes[:len(c.scopes)-1] }
}

func (c *compiler) lookup(name string) (binding, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if b, ok := c.scopes[i][name]; ok {
			return b, true
		}
	}
	return binding{}, false
}

func (c *compiler) local(pos token.Pos, t Type) binding {
	n := len(c.entry.locals)
	if n >= maxItems {
		c.fail(pos, "too many local slots")
	}
	c.entry.locals = append(c.entry.locals, t)
	return binding{n, t}
}

func (c *compiler) declare(id *ast.Ident, t Type) binding {
	scope := c.scopes[len(c.scopes)-1]
	if _, ok := scope[id.Name]; ok || !validName(id.Name) {
		c.fail(id.Pos(), "invalid or duplicate local %q", id.Name)
	}
	b := c.local(id.Pos(), t)
	scope[id.Name] = b
	return b
}

func (c *compiler) compactHosts() {
	used := make([]bool, len(c.program.hosts))
	for _, e := range c.program.entries {
		for _, ins := range e.code {
			if ins.op == OpCall {
				used[ins.arg] = true
			}
		}
	}
	var retained []hostRef
	indices := make([]int, len(used))
	for i, host := range c.program.hosts {
		if used[i] {
			indices[i] = len(retained)
			retained = append(retained, host)
		}
	}
	for _, e := range c.program.entries {
		for i, ins := range e.code {
			if ins.op == OpCall {
				e.code[i].arg = indices[ins.arg]
			}
		}
	}
	c.program.hosts = retained
}

var binaryOps = map[token.Token]Opcode{
	token.ADD: OpAdd, token.SUB: OpSub, token.MUL: OpMul, token.QUO: OpDiv, token.REM: OpMod,
	token.AND: OpBitAnd, token.OR: OpBitOr, token.XOR: OpBitXor, token.AND_NOT: OpBitClear,
	token.SHL: OpShiftLeft, token.SHR: OpShiftRight,
	token.EQL: OpEq, token.NEQ: OpNe, token.LSS: OpLt, token.LEQ: OpLe, token.GTR: OpGt, token.GEQ: OpGe,
}

type expression struct {
	t        Type
	constant Value // Void means the result depends on runtime evaluation
}

func (c *compiler) literal(v Value) expression {
	op := OpPushInt
	if v.t == Bool {
		op = OpPushBool
	}
	c.emit(op, int(v.n))
	return expression{v.t, v}
}

func (c *compiler) integer(lit *ast.BasicLit, negative bool) expression {
	n, err := strconv.ParseUint(lit.Value, 0, 32)
	limit := uint64(math.MaxInt32)
	if negative {
		limit++
	}
	if err != nil || n > limit {
		c.fail(lit.Pos(), "integer literal is outside the signed 32-bit range")
	}
	if negative {
		return c.literal(IntValue(int32(-int64(n))))
	}
	return c.literal(IntValue(int32(n)))
}

func (c *compiler) need(pos token.Pos, got, want Type) {
	if got != want {
		c.fail(pos, "expected %s, got %s", want, got)
	}
}

func (c *compiler) expr(node ast.Expr, depth int) expression {
	if depth >= MaxNesting {
		c.fail(node.Pos(), "maximum expression nesting exceeded")
	}
	start := c.pc()
	next := depth + 1
	switch n := node.(type) {
	case *ast.ParenExpr:
		return c.expr(n.X, next)
	case *ast.BasicLit:
		if n.Kind == token.INT {
			return c.integer(n, false)
		}
	case *ast.Ident:
		if b, ok := c.lookup(n.Name); ok {
			c.emit(OpLoad, b.slot)
			return expression{t: b.t}
		}
		switch n.Name {
		case "true", "false":
			return c.literal(BoolValue(n.Name == "true"))
		}
		if v, ok := c.vm.constants[n.Name]; ok {
			return c.literal(v)
		}
		c.fail(n.Pos(), "unknown variable or constant %q", n.Name)
	case *ast.CallExpr:
		return c.call(n, next)
	case *ast.UnaryExpr:
		switch n.Op {
		case token.SUB:
			if literal, ok := n.X.(*ast.BasicLit); ok && literal.Kind == token.INT {
				return c.integer(literal, true)
			}
		case token.ADD, token.XOR, token.NOT:
		default:
			c.fail(n.Pos(), "unsupported unary operator %s", n.Op)
		}
		a := c.expr(n.X, next)
		op := OpNeg
		switch n.Op {
		case token.ADD:
			c.need(n.Pos(), a.t, Int)
			return a
		case token.XOR:
			op = OpBitNot
		case token.NOT:
			op = OpNot
		}
		want, _ := operator(op)
		c.need(n.Pos(), a.t, want)
		if a.constant.t != Void {
			v, _ := evaluate(op, a.constant, a.constant)
			c.entry.code = c.entry.code[:start]
			return c.literal(v)
		}
		c.emit(op, 0)
		return expression{t: operatorResult(op)}
	case *ast.BinaryExpr:
		left := c.expr(n.X, next)
		if n.Op == token.LAND || n.Op == token.LOR {
			return c.logical(n, left, start, next)
		}
		op, ok := binaryOps[n.Op]
		if !ok {
			c.fail(n.Pos(), "unsupported binary operator %s", n.Op)
		}
		right := c.expr(n.Y, next)
		want, _ := operator(op)
		if want == Void {
			want = left.t
			if !isValueType(want) {
				c.fail(n.X.Pos(), "expected a value")
			}
		}
		c.need(n.X.Pos(), left.t, want)
		c.need(n.Y.Pos(), right.t, want)
		if left.constant.t != Void && right.constant.t != Void {
			if v, err := evaluate(op, left.constant, right.constant); err == nil {
				c.entry.code = c.entry.code[:start]
				return c.literal(v)
			}
		}
		c.emit(op, 0)
		return expression{t: operatorResult(op)}
	}
	c.fail(node.Pos(), "unsupported expression")
	return expression{}
}

func (c *compiler) logical(n *ast.BinaryExpr, left expression, start, depth int) expression {
	c.need(n.X.Pos(), left.t, Bool)
	ors := n.Op == token.LOR
	if truth, known := left.constant.Bool(); known {
		c.entry.code = c.entry.code[:start]
		right := c.expr(n.Y, depth)
		c.need(n.Y.Pos(), right.t, Bool)
		if truth != ors {
			return right
		}
		c.entry.code = c.entry.code[:start]
		return c.literal(left.constant)
	}
	op := OpAndThen
	if ors {
		op = OpOrElse
	}
	jump := c.emit(op, 0)
	right := c.expr(n.Y, depth)
	c.need(n.Y.Pos(), right.t, Bool)
	// x && true and x || false preserve x, including its side effects.
	if truth, known := right.constant.Bool(); known && truth != ors {
		c.entry.code = c.entry.code[:jump]
	} else {
		c.patch(jump, c.pc())
	}
	return expression{t: Bool}
}

func (c *compiler) call(n *ast.CallExpr, depth int) expression {
	id, ok := n.Fun.(*ast.Ident)
	if !ok || n.Ellipsis.IsValid() {
		c.fail(n.Pos(), "expected a direct host function call")
	}
	if _, shadowed := c.lookup(id.Name); shadowed {
		c.fail(id.Pos(), "%q is a local variable", id.Name)
	}
	if id.Name == "gas_left" {
		if len(n.Args) != 0 {
			c.fail(n.Pos(), "gas_left takes no arguments")
		}
		c.emit(OpGasLeft, 0)
		return expression{t: Int}
	}
	fn, ok := c.vm.functions[id.Name]
	if !ok {
		c.fail(id.Pos(), "unknown host function %q", id.Name)
	}
	if len(n.Args) != len(fn.Params) {
		c.fail(n.Pos(), "%s expects %d arguments", id.Name, len(fn.Params))
	}
	for i, arg := range n.Args {
		c.need(arg.Pos(), c.expr(arg, depth).t, fn.Params[i])
	}
	index, exists := c.hostIDs[id.Name]
	if !exists {
		index = len(c.program.hosts)
		if index >= maxItems {
			c.fail(id.Pos(), "too many host functions")
		}
		c.hostIDs[id.Name] = index
		c.program.hosts = append(c.program.hosts, hostRef{id.Name, fn.Params, fn.Result})
	}
	c.emit(OpCall, index)
	return expression{t: fn.Result}
}
