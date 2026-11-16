package gasvm

import (
	"go/ast"
	"go/token"
)

func (c *compiler) statements(nodes []ast.Stmt) {
	for _, node := range nodes {
		c.statement(node)
	}
}

func (c *compiler) statement(node ast.Stmt) {
	c.statementDepth++
	defer func() { c.statementDepth-- }()
	if c.statementDepth > MaxNesting {
		c.fail(node.Pos(), "maximum statement nesting exceeded")
	}
	switch n := node.(type) {
	case *ast.EmptyStmt:
	case *ast.BlockStmt:
		defer c.scope()()
		c.statements(n.List)
	case *ast.AssignStmt:
		c.assign(n)
	case *ast.IncDecStmt:
		b := c.assignedLocal(n.X)
		c.need(n.Pos(), b.t, Int)
		c.emit(OpLoad, b.slot)
		c.literal(IntValue(1))
		op := OpAdd
		if n.Tok == token.DEC {
			op = OpSub
		}
		c.emit(op, 0)
		c.emit(OpStore, b.slot)
	case *ast.ExprStmt:
		if _, ok := n.X.(*ast.CallExpr); !ok {
			c.fail(n.Pos(), "expected a host call as a statement")
		}
		if c.expr(n.X, 0).t != Void {
			c.emit(OpPop, 0)
		}
	case *ast.IfStmt:
		c.ifStmt(n)
	case *ast.ForStmt:
		c.forStmt(n)
	case *ast.SwitchStmt:
		c.switchStmt(n)
	case *ast.BranchStmt:
		c.branch(n)
	case *ast.ReturnStmt:
		c.ret(n)
	default:
		c.fail(node.Pos(), "unsupported statement")
	}
}

func (c *compiler) assignedLocal(node ast.Expr) binding {
	if id, ok := node.(*ast.Ident); ok {
		if b, found := c.lookup(id.Name); found {
			return b
		}
	}
	c.fail(node.Pos(), "assignment requires a declared local variable")
	return binding{}
}

var compoundOps = map[token.Token]Opcode{
	token.ADD_ASSIGN: OpAdd, token.SUB_ASSIGN: OpSub, token.MUL_ASSIGN: OpMul,
	token.QUO_ASSIGN: OpDiv, token.REM_ASSIGN: OpMod, token.AND_ASSIGN: OpBitAnd,
	token.OR_ASSIGN: OpBitOr, token.XOR_ASSIGN: OpBitXor, token.AND_NOT_ASSIGN: OpBitClear,
	token.SHL_ASSIGN: OpShiftLeft, token.SHR_ASSIGN: OpShiftRight,
}

func (c *compiler) assign(n *ast.AssignStmt) {
	if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
		c.fail(n.Pos(), "assignment requires one local and one value")
	}
	lhs, rhs := n.Lhs[0], n.Rhs[0]
	if n.Tok == token.DEFINE {
		id, ok := lhs.(*ast.Ident)
		if !ok {
			c.fail(lhs.Pos(), "expected a local name")
		}
		t := c.expr(rhs, 0).t
		if t == Void {
			c.fail(rhs.Pos(), "expected a value")
		}
		b := c.declare(id, t)
		c.emit(OpStore, b.slot)
		return
	}
	b := c.assignedLocal(lhs)
	if n.Tok == token.ASSIGN {
		c.need(rhs.Pos(), c.expr(rhs, 0).t, b.t)
	} else {
		c.need(lhs.Pos(), b.t, Int)
		c.emit(OpLoad, b.slot)
		c.need(rhs.Pos(), c.expr(rhs, 0).t, Int)
		c.emit(compoundOps[n.Tok], 0)
	}
	c.emit(OpStore, b.slot)
}

func (c *compiler) ret(n *ast.ReturnStmt) {
	if len(n.Results) > 1 {
		c.fail(n.Pos(), "return accepts at most one value")
	}
	if len(n.Results) == 0 {
		c.need(n.Pos(), Void, c.entry.result)
	} else {
		t := c.expr(n.Results[0], 0).t
		if !isValueType(t) {
			c.fail(n.Pos(), "expected a return value")
		}
		if !c.topLevel {
			c.need(n.Pos(), t, c.entry.result)
		}
	}
	c.emit(OpReturn, len(n.Results))
}

// conditionBranch consumes a boolean expression or emits direct control
// flow for a constant guard. -1 means the branch is never taken.
func (c *compiler) conditionBranch(node ast.Expr, onTrue bool) int {
	start := c.pc()
	// A direct negation can invert the branch instead of computing a value.
	if unary, ok := node.(*ast.UnaryExpr); ok && unary.Op == token.NOT {
		node, onTrue = unary.X, !onTrue
	}
	expr := c.expr(node, 0)
	c.need(node.Pos(), expr.t, Bool)
	if truth, known := expr.constant.Bool(); known {
		c.entry.code = c.entry.code[:start]
		if truth != onTrue {
			return -1
		}
		return c.emit(OpJump, 0)
	}
	if onTrue {
		return c.emit(OpJumpTrue, 0)
	}
	return c.emit(OpJumpFalse, 0)
}

func (c *compiler) ifStmt(n *ast.IfStmt) {
	defer c.scope()()
	if n.Init != nil {
		c.statement(n.Init)
	}
	skip := c.conditionBranch(n.Cond, false)
	c.statement(n.Body)
	if n.Else == nil {
		c.patch(skip, c.pc())
		return
	}
	done := c.emit(OpJump, 0)
	c.patch(skip, c.pc())
	c.statement(n.Else)
	c.patch(done, c.pc())
}

func (c *compiler) forStmt(n *ast.ForStmt) {
	defer c.scope()()
	if n.Init != nil {
		c.statement(n.Init)
	}
	head := c.pc()
	exit := -1
	if n.Cond != nil {
		exit = c.conditionBranch(n.Cond, false)
	}
	block := &control{loop: true}
	c.control = append(c.control, block)
	c.statement(n.Body)
	c.control = c.control[:len(c.control)-1]
	for _, jump := range block.continues {
		c.patch(jump, c.pc())
	}
	if n.Post != nil {
		if assign, ok := n.Post.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			c.fail(n.Post.Pos(), "loop post statement cannot declare locals")
		}
		c.statement(n.Post)
	}
	c.emit(OpJump, head)
	end := c.pc()
	c.patch(exit, end)
	for _, jump := range block.breaks {
		c.patch(jump, end)
	}
	if exit == -1 {
		reached := reachable(c.entry.code, head)
		if reached[end] {
			return
		}
		for pc := head; pc < end; pc++ {
			if reached[pc] && c.entry.code[pc].op == OpReturn {
				return
			}
		}
		c.fail(n.Pos(), "unconditional loop has no reachable break or return")
	}
}

func (c *compiler) branch(n *ast.BranchStmt) {
	if n.Label != nil || (n.Tok != token.BREAK && n.Tok != token.CONTINUE) {
		c.fail(n.Pos(), "only unlabeled break and continue are supported")
	}
	for i := len(c.control) - 1; i >= 0; i-- {
		block := c.control[i]
		if n.Tok == token.BREAK {
			block.breaks = append(block.breaks, c.emit(OpJump, 0))
			return
		}
		if block.loop {
			block.continues = append(block.continues, c.emit(OpJump, 0))
			return
		}
	}
	c.fail(n.Pos(), "%s outside a compatible control block", n.Tok)
}

func (c *compiler) switchStmt(n *ast.SwitchStmt) {
	defer c.scope()()
	if n.Init != nil {
		c.statement(n.Init)
	}
	tag := expression{Bool, BoolValue(true)}
	var slot int
	if n.Tag != nil {
		start := c.pc()
		tag = c.expr(n.Tag, 0)
		if tag.t == Void {
			c.fail(n.Tag.Pos(), "switch requires a value")
		}
		if tag.constant.t != Void {
			c.entry.code = c.entry.code[:start]
		} else {
			slot = c.local(n.Tag.Pos(), tag.t).slot
			c.emit(OpStore, slot)
		}
	}
	type clause struct {
		node  *ast.CaseClause
		jumps []int
	}
	var clauses []clause
	defaultIndex := -1
	for _, stmt := range n.Body.List {
		cs := stmt.(*ast.CaseClause)
		current := clause{node: cs}
		if len(cs.List) == 0 {
			if defaultIndex != -1 {
				c.fail(cs.Pos(), "duplicate default clause")
			}
			defaultIndex = len(clauses)
		}
		for _, condition := range cs.List {
			if n.Tag == nil {
				if jump := c.conditionBranch(condition, true); jump >= 0 {
					current.jumps = append(current.jumps, jump)
				}
				continue
			}
			start := c.pc()
			if tag.constant.t == Void {
				c.emit(OpLoad, slot)
			} else {
				c.literal(tag.constant)
			}
			test := c.expr(condition, 0)
			c.need(condition.Pos(), test.t, tag.t)
			if tag.constant.t != Void && test.constant.t != Void {
				c.entry.code = c.entry.code[:start]
				if tag.constant == test.constant {
					current.jumps = append(current.jumps, c.emit(OpJump, 0))
				}
			} else {
				c.emit(OpEq, 0)
				current.jumps = append(current.jumps, c.emit(OpJumpTrue, 0))
			}
		}
		clauses = append(clauses, current)
	}
	noMatch := c.emit(OpJump, 0)
	block := &control{}
	c.control = append(c.control, block)
	for i, cs := range clauses {
		if i == defaultIndex {
			c.patch(noMatch, c.pc())
		}
		for _, jump := range cs.jumps {
			c.patch(jump, c.pc())
		}
		closeScope := c.scope()
		c.statements(cs.node.Body)
		closeScope()
		block.breaks = append(block.breaks, c.emit(OpJump, 0))
	}
	c.control = c.control[:len(c.control)-1]
	if defaultIndex == -1 {
		c.patch(noMatch, c.pc())
	}
	for _, jump := range block.breaks {
		c.patch(jump, c.pc())
	}
}
