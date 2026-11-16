// Package gasvm compiles and runs gas-metered decision scripts.
package gasvm

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"math"
)

type Type uint8

const (
	Void Type = iota
	Int
	Bool
)

func (t Type) String() string {
	switch t {
	case Void:
		return "void"
	case Int:
		return "int"
	case Bool:
		return "bool"
	default:
		return "invalid"
	}
}

// Value holds a signed 32-bit integer, a boolean, or a void result.
type Value struct {
	t Type
	n int32
}

func IntValue(n int32) Value { return Value{Int, n} }
func BoolValue(b bool) Value {
	if b {
		return Value{Bool, 1}
	}
	return Value{Bool, 0}
}
func (v Value) Type() Type           { return v.t }
func (v Value) Int32() (int32, bool) { return v.n, v.t == Int }
func (v Value) Bool() (bool, bool)   { return v.n != 0, v.t == Bool }

// CallContext provides the calling VM, its entity, and the active gas budget.
// Pass Context to nested Run calls so the run-depth limit applies.
type CallContext struct {
	Context context.Context
	VM      *VM
	GasLeft uint32
}

type HostFunc struct {
	Params []Type
	Result Type
	Gas    uint32
	// Quote, when set, replaces Gas. It should do bounded, read-only work.
	// Run is called only after the entire quoted charge has been paid.
	Quote func(CallContext, []Value) (uint32, error)
	Run   func(CallContext, []Value) (Value, error)
}

type Config struct {
	Caller    any
	Constants map[string]Value
	Functions map[string]HostFunc
	// Costs overrides the default cost of one gas per instruction.
	// Zero is allowed. MaxFreeInstructions bounds execution without gas usage.
	Costs map[Opcode]uint32
}

type VM struct {
	caller    any
	constants map[string]Value
	functions map[string]HostFunc
	costs     [opcodeCount]uint32
}

func (vm *VM) Caller() any { return vm.caller }

func New(cfg Config) (*VM, error) {
	vm := &VM{
		caller: cfg.Caller, constants: make(map[string]Value, len(cfg.Constants)),
		functions: make(map[string]HostFunc, len(cfg.Functions)),
	}
	for op := range vm.costs {
		vm.costs[op] = 1
	}
	for op, gas := range cfg.Costs {
		if op >= opcodeCount {
			return nil, fmt.Errorf("invalid opcode %d in gas schedule", op)
		}
		vm.costs[op] = gas
	}
	for name, value := range cfg.Constants {
		if !validName(name) || !isValueType(value.t) {
			return nil, fmt.Errorf("invalid constant %q", name)
		}
		vm.constants[name] = value
	}
	for name, fn := range cfg.Functions {
		if !validName(name) || fn.Run == nil || !validSignature(fn.Params, fn.Result) {
			return nil, fmt.Errorf("invalid host function %q", name)
		}
		if _, ok := vm.constants[name]; ok {
			return nil, fmt.Errorf("duplicate host name %q", name)
		}
		fn.Params = append([]Type(nil), fn.Params...)
		vm.functions[name] = fn
	}
	return vm, nil
}

func validName(name string) bool {
	if len(name) > maxItems {
		return false
	}
	switch name {
	case "_", "entry", "int", "int32", "bool", "true", "false", "gas_left":
		return false
	default:
		return token.IsIdentifier(name)
	}
}

func isValueType(t Type) bool { return t == Int || t == Bool }

func validSignature(params []Type, result Type) bool {
	if len(params) > maxItems || result > Bool {
		return false
	}
	for _, t := range params {
		if !isValueType(t) {
			return false
		}
	}
	return true
}

const (
	MaxNesting          = 64
	MaxRunDepth         = 64
	MaxGas              = math.MaxInt32
	MaxFreeInstructions = 65536
)

var (
	ErrOutOfGas        = errors.New("out of gas")
	ErrDivisionByZero  = errors.New("division by zero")
	ErrNegativeShift   = errors.New("negative shift count")
	ErrRunDepth        = errors.New("maximum run depth exceeded")
	ErrNoGasProgress   = errors.New("too many instructions without spending gas")
	ErrInvalidBytecode = errors.New("invalid bytecode")
)

type Result struct {
	Value   Value
	GasUsed uint32
}

type runDepthKey struct{}

// Run executes the selected entry point. The empty name selects the top-level
// script. GasUsed includes only successful charges, even when the run fails.
// The host owns any external effects produced by its callbacks.
func (vm *VM) Run(ctx context.Context, p *Program, entryName string, budget uint32, args ...Value) (Result, error) {
	var result Result
	if ctx == nil {
		ctx = context.Background()
	}
	depth, _ := ctx.Value(runDepthKey{}).(int)
	if depth >= MaxRunDepth {
		return result, ErrRunDepth
	}
	ctx = context.WithValue(ctx, runDepthKey{}, depth+1)
	if budget > MaxGas {
		return result, fmt.Errorf("gas budget exceeds %d", MaxGas)
	}
	if p == nil {
		return result, ErrInvalidBytecode
	}
	entry, ok := p.entries[entryName]
	if !ok {
		return result, fmt.Errorf("unknown entry %q", entryName)
	}
	if !matches(args, entry.params) {
		return result, fmt.Errorf("arguments do not match entry %q", entryName)
	}
	hosts := make([]HostFunc, len(p.hosts))
	for i, ref := range p.hosts {
		fn, ok := vm.functions[ref.name]
		if !ok || fn.Result != ref.result || !sameTypes(fn.Params, ref.params) {
			return result, fmt.Errorf("host function %q has no matching registration", ref.name)
		}
		hosts[i] = fn
	}
	locals := make([]Value, len(entry.locals))
	for i, t := range entry.locals {
		locals[i].t = t
	}
	copy(locals, args)
	stack := make([]Value, 0, entry.stackSize)
	charge := func(cost uint32) bool {
		if cost > budget-result.GasUsed {
			return false
		}
		result.GasUsed += cost
		return true
	}
	pop := func() Value {
		top := len(stack) - 1
		v := stack[top]
		stack = stack[:top]
		return v
	}
	var lastGas uint32
	free := 0
	for pc := 0; ; {
		if result.GasUsed != lastGas {
			lastGas, free = result.GasUsed, 0
		}
		if free == MaxFreeInstructions {
			return result, ErrNoGasProgress
		}
		free++
		ins := entry.code[pc]
		if !charge(vm.costs[ins.op]) {
			return result, ErrOutOfGas
		}
		pc++
		switch ins.op {
		case OpPushInt:
			stack = append(stack, IntValue(int32(ins.arg)))
		case OpPushBool:
			stack = append(stack, BoolValue(ins.arg != 0))
		case OpLoad:
			stack = append(stack, locals[ins.arg])
		case OpStore:
			locals[ins.arg] = pop()
		case OpPop:
			pop()
		case OpJump:
			pc = ins.arg
		case OpJumpFalse, OpJumpTrue:
			if (pop().n != 0) == (ins.op == OpJumpTrue) {
				pc = ins.arg
			}
		case OpAndThen, OpOrElse:
			truth := stack[len(stack)-1].n != 0
			if truth == (ins.op == OpOrElse) {
				pc = ins.arg
			} else {
				pop()
			}
		case OpGasLeft:
			stack = append(stack, IntValue(int32(budget-result.GasUsed)))
		case OpCall:
			fn := hosts[ins.arg]
			n := len(stack) - len(fn.Params)
			callArgs := append([]Value(nil), stack[n:]...)
			stack = stack[:n]
			call := CallContext{ctx, vm, budget - result.GasUsed}
			extra := fn.Gas
			if fn.Quote != nil {
				var err error
				extra, err = fn.Quote(call, append([]Value(nil), callArgs...))
				if err != nil {
					return result, fmt.Errorf("quoting %s: %w", p.hosts[ins.arg].name, err)
				}
			}
			if !charge(extra) {
				return result, ErrOutOfGas
			}
			call.GasLeft = budget - result.GasUsed
			value, err := fn.Run(call, callArgs)
			if err != nil {
				return result, fmt.Errorf("calling %s: %w", p.hosts[ins.arg].name, err)
			}
			if value.t != fn.Result {
				return result, fmt.Errorf("%s returned %s, expected %s", p.hosts[ins.arg].name, value.t, fn.Result)
			}
			if value.t != Void {
				stack = append(stack, value)
			}
		case OpReturn:
			if ins.arg != 0 {
				result.Value = pop()
			}
			return result, nil
		default:
			_, unary := operator(ins.op)
			b := pop()
			a := b
			if !unary {
				a = pop()
			}
			v, err := evaluate(ins.op, a, b)
			if err != nil {
				return result, err
			}
			stack = append(stack, v)
		}
	}
}

func matches(values []Value, types []Type) bool {
	if len(values) != len(types) {
		return false
	}
	for i, v := range values {
		if v.t != types[i] {
			return false
		}
	}
	return true
}

func sameTypes(a, b []Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
