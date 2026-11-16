package gasvm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Opcode identifies an instruction for gas schedule overrides.
type Opcode uint8

const (
	OpPushInt Opcode = iota
	OpPushBool
	OpLoad
	OpStore
	OpPop
	OpJump
	OpJumpFalse
	OpAndThen
	OpOrElse
	OpGasLeft
	OpCall
	OpReturn
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpMod
	OpBitAnd
	OpBitOr
	OpBitXor
	OpBitClear
	OpShiftLeft
	OpShiftRight
	OpNeg
	OpBitNot
	OpNot
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpJumpTrue
	opcodeCount
)

func operator(op Opcode) (Type, bool) {
	switch op {
	case OpNeg, OpBitNot:
		return Int, true
	case OpNot:
		return Bool, true
	case OpEq, OpNe:
		return Void, false // operands may have either type, but must match
	default:
		return Int, false
	}
}

func operatorResult(op Opcode) Type {
	if op >= OpNot {
		return Bool
	}
	return Int
}

func evaluate(op Opcode, a, b Value) (Value, error) {
	x, y := a.n, b.n
	switch op {
	case OpAdd:
		return IntValue(x + y), nil
	case OpSub:
		return IntValue(x - y), nil
	case OpMul:
		return IntValue(x * y), nil
	case OpDiv, OpMod:
		if y == 0 {
			return Value{}, ErrDivisionByZero
		}
		if op == OpDiv {
			return IntValue(x / y), nil
		}
		return IntValue(x % y), nil
	case OpBitAnd:
		return IntValue(x & y), nil
	case OpBitOr:
		return IntValue(x | y), nil
	case OpBitXor:
		return IntValue(x ^ y), nil
	case OpBitClear:
		return IntValue(x &^ y), nil
	case OpShiftLeft, OpShiftRight:
		if y < 0 {
			return Value{}, ErrNegativeShift
		}
		if op == OpShiftLeft {
			return IntValue(x << uint(y)), nil
		}
		return IntValue(x >> uint(y)), nil
	case OpNeg:
		return IntValue(-x), nil
	case OpBitNot:
		return IntValue(^x), nil
	case OpNot:
		return BoolValue(y == 0), nil
	case OpEq:
		return BoolValue(x == y), nil
	case OpNe:
		return BoolValue(x != y), nil
	case OpLt:
		return BoolValue(x < y), nil
	case OpLe:
		return BoolValue(x <= y), nil
	case OpGt:
		return BoolValue(x > y), nil
	case OpGe:
		return BoolValue(x >= y), nil
	default:
		return Value{}, ErrInvalidBytecode
	}
}

const (
	bytecodeHeader = "SGVM\x01"
	maxItems       = 4096
	maxStack       = 256
	maxCode        = 65536
	maxBytecode    = 4 << 20
)

type instruction struct {
	op  Opcode
	arg int
}

type hostRef struct {
	name   string
	params []Type
	result Type
}

type entryPoint struct {
	params, locals []Type
	result         Type
	code           []instruction
	stackSize      int
}

// Program is immutable compiled bytecode. It can be reused across VM instances.
type Program struct {
	entries map[string]*entryPoint
	hosts   []hostRef
}

func (p *Program) MarshalBinary() ([]byte, error) {
	if p == nil || len(p.entries) == 0 {
		return nil, ErrInvalidBytecode
	}
	data := []byte(bytecodeHeader)
	writeCount := func(n int) { data = binary.AppendUvarint(data, uint64(n)) }
	writeName := func(name string) {
		writeCount(len(name))
		data = append(data, name...)
	}
	writeTypes := func(types []Type) {
		writeCount(len(types))
		for _, t := range types {
			data = append(data, byte(t))
		}
	}
	writeCount(len(p.hosts))
	for _, host := range p.hosts {
		writeName(host.name)
		writeTypes(host.params)
		data = append(data, byte(host.result))
	}
	names := make([]string, 0, len(p.entries))
	for name := range p.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	writeCount(len(names))
	for _, name := range names {
		entry := p.entries[name]
		writeName(name)
		writeTypes(entry.params)
		writeTypes(entry.locals)
		data = append(data, byte(entry.result))
		writeCount(len(entry.code))
		for pc, ins := range entry.code {
			data = append(data, byte(ins.op))
			switch ins.op {
			case OpPushInt:
				data = binary.AppendVarint(data, int64(ins.arg))
			case OpJump, OpJumpFalse, OpJumpTrue, OpAndThen, OpOrElse:
				data = binary.AppendVarint(data, int64(ins.arg-pc))
			case OpLoad, OpStore, OpCall:
				writeCount(ins.arg)
			case OpPushBool, OpReturn:
				data = append(data, byte(ins.arg))
			}
		}
	}
	if len(data) > maxBytecode {
		return nil, fmt.Errorf("bytecode exceeds %d bytes", maxBytecode)
	}
	return data, nil
}

type decoder struct {
	*bytes.Reader
	bad bool
}

func (d *decoder) octet() byte {
	b, err := d.ReadByte()
	d.bad = d.bad || err != nil
	return b
}

func (d *decoder) count(max int) int {
	n, err := binary.ReadUvarint(d)
	if err != nil || n > uint64(max) {
		d.bad = true
		return 0
	}
	return int(n)
}

func (d *decoder) signed(minimum, maximum int64) int {
	n, err := binary.ReadVarint(d)
	if err != nil || n < minimum || n > maximum {
		d.bad = true
		return 0
	}
	return int(n)
}

func (d *decoder) name() string {
	n := d.count(maxItems)
	if n > d.Len() {
		d.bad = true
		return ""
	}
	b := make([]byte, n)
	_, _ = d.Read(b)
	return string(b)
}

func (d *decoder) types() []Type {
	n := d.count(maxItems)
	if n > d.Len() {
		d.bad = true
		return nil
	}
	types := make([]Type, n)
	for i := range types {
		types[i] = Type(d.octet())
	}
	return types
}

// Load decodes and verifies the bytecode produced by MarshalBinary.
func Load(data []byte) (*Program, error) {
	if len(data) > maxBytecode || !bytes.HasPrefix(data, []byte(bytecodeHeader)) {
		return nil, ErrInvalidBytecode
	}
	d := decoder{Reader: bytes.NewReader(data[len(bytecodeHeader):])}
	p := &Program{entries: make(map[string]*entryPoint)}
	seen := make(map[string]bool)
	for n := d.count(maxItems); n > 0 && !d.bad; n-- {
		ref := hostRef{d.name(), d.types(), Type(d.octet())}
		if !validName(ref.name) || seen[ref.name] || !validSignature(ref.params, ref.result) {
			return nil, ErrInvalidBytecode
		}
		seen[ref.name] = true
		p.hosts = append(p.hosts, ref)
	}
	for n := d.count(maxItems); n > 0 && !d.bad; n-- {
		name := d.name()
		e := &entryPoint{params: d.types(), locals: d.types(), result: Type(d.octet())}
		if (name != "" && !validName(name)) || p.entries[name] != nil {
			return nil, ErrInvalidBytecode
		}
		count := d.count(maxCode)
		if count > d.Len() {
			return nil, ErrInvalidBytecode
		}
		e.code = make([]instruction, count)
		for pc := range e.code {
			ins := instruction{op: Opcode(d.octet())}
			switch ins.op {
			case OpPushInt:
				ins.arg = d.signed(math.MinInt32, math.MaxInt32)
			case OpJump, OpJumpFalse, OpJumpTrue, OpAndThen, OpOrElse:
				ins.arg = pc + d.signed(-maxCode, maxCode)
			case OpLoad, OpStore, OpCall:
				ins.arg = d.count(maxItems)
			case OpPushBool, OpReturn:
				ins.arg = int(d.octet())
			}
			e.code[pc] = ins
		}
		if d.bad || !verify(e, p.hosts, name == "") {
			return nil, fmt.Errorf("%w in entry %q", ErrInvalidBytecode, name)
		}
		p.entries[name] = e
	}
	if d.bad || d.Len() != 0 || len(p.entries) == 0 {
		return nil, ErrInvalidBytecode
	}
	return p, nil
}

func verify(e *entryPoint, hosts []hostRef, topLevel bool) bool {
	if len(e.code) == 0 || len(e.code) > maxCode || len(e.locals) > maxItems ||
		!validSignature(e.params, e.result) || !validSignature(e.locals, Void) ||
		len(e.locals) < len(e.params) || !sameTypes(e.params, e.locals[:len(e.params)]) ||
		(topLevel && (len(e.params) != 0 || e.result != Void)) {
		return false
	}
	for pc, ins := range e.code {
		switch ins.op {
		case OpJump, OpJumpFalse, OpJumpTrue, OpAndThen, OpOrElse:
			if ins.arg < 0 || ins.arg >= len(e.code) || (ins.op != OpJump && ins.arg <= pc) {
				return false
			}
		case OpLoad, OpStore:
			if ins.arg < 0 || ins.arg >= len(e.locals) {
				return false
			}
		case OpCall:
			if ins.arg < 0 || ins.arg >= len(hosts) {
				return false
			}
		case OpPushBool, OpReturn:
			if ins.arg < 0 || ins.arg > 1 {
				return false
			}
		default:
			if ins.op >= opcodeCount {
				return false
			}
		}
	}
	states := make([][]Type, len(e.code))
	queue := make([]int, 0, len(e.code))
	add := func(pc int, stack []Type) bool {
		if pc >= len(states) || len(stack) > maxStack {
			return false
		}
		e.stackSize = max(e.stackSize, len(stack))
		if states[pc] != nil {
			return sameTypes(states[pc], stack)
		}
		states[pc] = append(make([]Type, 0, len(stack)), stack...)
		queue = append(queue, pc)
		return true
	}
	add(0, nil)
	for i := 0; i < len(queue); i++ {
		pc := queue[i]
		stack := states[pc][:len(states[pc]):len(states[pc])]
		pop := func(want Type) bool {
			if len(stack) == 0 || (want != Void && stack[len(stack)-1] != want) {
				return false
			}
			stack = stack[: len(stack)-1 : len(stack)-1]
			return true
		}
		ins := e.code[pc]
		switch ins.op {
		case OpPushInt, OpGasLeft:
			stack = append(stack, Int)
		case OpPushBool:
			stack = append(stack, Bool)
		case OpLoad:
			stack = append(stack, e.locals[ins.arg])
		case OpStore, OpPop:
			want := Void
			if ins.op == OpStore {
				want = e.locals[ins.arg]
			}
			if !pop(want) {
				return false
			}
		case OpJump:
			if !add(ins.arg, stack) {
				return false
			}
			continue
		case OpJumpFalse, OpJumpTrue:
			if !pop(Bool) || !add(ins.arg, stack) {
				return false
			}
		case OpAndThen, OpOrElse:
			if !add(ins.arg, stack) || !pop(Bool) {
				return false
			}
		case OpCall:
			h := hosts[ins.arg]
			for j := len(h.params) - 1; j >= 0; j-- {
				if !pop(h.params[j]) {
					return false
				}
			}
			if h.result != Void {
				stack = append(stack, h.result)
			}
		case OpReturn:
			if len(stack) != ins.arg ||
				(!topLevel && ((e.result == Void) != (ins.arg == 0) || (ins.arg == 1 && stack[0] != e.result))) {
				return false
			}
			continue
		default:
			want, unary := operator(ins.op)
			if want == Void && len(stack) >= 1 {
				want = stack[len(stack)-1]
			}
			if !pop(want) || (!unary && !pop(want)) {
				return false
			}
			stack = append(stack, operatorResult(ins.op))
		}
		if !add(pc+1, stack) {
			return false
		}
	}
	return true
}
