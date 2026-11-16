package gasvm

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func newVM(t *testing.T, cfg Config) *VM {
	t.Helper()
	vm, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return vm
}

func compileScript(t *testing.T, vm *VM, source string) *Program {
	t.Helper()
	p, err := vm.Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecisionAndCallerPricing(t *testing.T) {
	type entity struct {
		turns, waves int
		scout        bool
	}
	config := Config{
		Constants: map[string]Value{"left": IntValue(-1)},
		Functions: map[string]HostFunc{
			"see_friend": {
				Result: Bool,
				Quote: func(call CallContext, _ []Value) (uint32, error) {
					if call.VM.Caller().(*entity).scout {
						return 2, nil
					}
					return 9, nil
				},
				Run: func(call CallContext, _ []Value) (Value, error) {
					return BoolValue(call.VM.Caller().(*entity).turns > 0), nil
				},
			},
			"wave": {
				Gas: 2,
				Run: func(call CallContext, _ []Value) (Value, error) {
					call.VM.Caller().(*entity).waves++
					return Value{}, nil
				},
			},
			"turn": {
				Params: []Type{Int},
				Gas:    2,
				Run: func(call CallContext, args []Value) (Value, error) {
					if v, _ := args[0].Int32(); v != -1 {
						t.Fatalf("turn argument = %d", v)
					}
					call.VM.Caller().(*entity).turns++
					return Value{}, nil
				},
			},
		},
	}
	const source = `
for gas_left() > 16 {
	if see_friend() {
		wave()
		return
	}
	turn(left)
}
`
	config.Caller = &entity{scout: true}
	scout := newVM(t, config)
	program := compileScript(t, scout, source)
	var scoutGas uint32
	for _, isScout := range []bool{true, false} {
		caller := &entity{scout: isScout}
		config.Caller = caller
		vm := newVM(t, config)
		result, err := vm.Run(context.Background(), program, "", 50)
		if err != nil || caller.turns != 1 || caller.waves != 1 || result.Value.Type() != Void {
			t.Fatalf("run = %+v, %v, caller = %+v", result, err, caller)
		}
		if isScout {
			scoutGas = result.GasUsed
		} else if result.GasUsed-scoutGas != 14 {
			t.Fatalf("gas = %d, scout = %d", result.GasUsed, scoutGas)
		}
	}
}

func TestHostPrechargeAndGasLeft(t *testing.T) {
	called := false
	var quotedWith uint32
	vm := newVM(t, Config{
		Costs: map[Opcode]uint32{OpCall: 2},
		Functions: map[string]HostFunc{
			"pay": {
				Quote: func(call CallContext, _ []Value) (uint32, error) {
					quotedWith = call.GasLeft
					return 4, nil
				},
				Run: func(call CallContext, _ []Value) (Value, error) {
					called = true
					if call.GasLeft != 2 {
						t.Fatalf("host entered with %d gas", call.GasLeft)
					}
					return Value{}, nil
				},
			},
		},
	})
	p := compileScript(t, vm, "pay()\nreturn gas_left()")
	res, err := vm.Run(nil, p, "", 5)
	if !errors.Is(err, ErrOutOfGas) || res.GasUsed != 2 || called || quotedWith != 3 {
		t.Fatalf("failed run = %+v, %v, called = %v, quote gas = %d", res, err, called, quotedWith)
	}
	res, err = vm.Run(nil, p, "", 8)
	if n, _ := res.Value.Int32(); err != nil || !called || n != 1 || res.GasUsed != 8 {
		t.Fatalf("successful run = %+v, %v, called = %v", res, err, called)
	}
}

func TestBytecodeRoundTripAndControlFlow(t *testing.T) {
	vm := newVM(t, Config{})
	p := compileScript(t, vm, `
entry score(start int, enabled bool) int {
	value := start
	for i := 0; i < 5; i++ {
		switch i {
		default:
			value += 5
		case 0, 2:
			continue
		case 1:
			value = value ^ 3
			break
		}
		if !enabled {
			break
		}
	}
	switch {
	case value > 10:
		return value &^ 2
	default:
		return value << 1
	}
}
`)
	data, err := p.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := loaded.MarshalBinary()
	if err != nil || !bytes.Equal(data, roundTrip) {
		t.Fatalf("unstable round trip: %v", err)
	}
	for _, tc := range []struct {
		enabled bool
		want    int32
	}{{true, 13}, {false, 10}} {
		res, err := vm.Run(nil, loaded, "score", 200, IntValue(6), BoolValue(tc.enabled))
		if got, ok := res.Value.Int32(); err != nil || !ok || got != tc.want {
			t.Fatalf("score(%v) = %d, %v", tc.enabled, got, err)
		}
	}
	for i := range data {
		if _, err := Load(data[:i]); !errors.Is(err, ErrInvalidBytecode) {
			t.Fatalf("truncated at %d: %v", i, err)
		}
	}
}

func TestOptimizationsAndShortCircuit(t *testing.T) {
	fn := HostFunc{Result: Bool, Run: func(CallContext, []Value) (Value, error) {
		t.Fatal("short-circuited call ran")
		return Value{}, nil
	}}
	vm := newVM(t, Config{Functions: map[string]HostFunc{"expensive": fn}})
	p := compileScript(t, vm, `
if false && expensive() { return 0 }
switch 2 * 3 {
case 4: return 0
case 6: return (4 + 2) * 2
default: return 0
}
entry condition(value bool) bool {
	return value || expensive()
}
`)
	res, err := vm.Run(nil, p, "", 2)
	if n, _ := res.Value.Int32(); err != nil || n != 12 || res.GasUsed != 2 {
		t.Fatalf("optimized run = %+v, %v", res, err)
	}
	res, err = vm.Run(nil, p, "condition", 3, BoolValue(true))
	if n, _ := res.Value.Bool(); err != nil || !n || res.GasUsed != 3 {
		t.Fatalf("short-circuited run = %+v, %v", res, err)
	}
	if _, err := newVM(t, Config{}).Run(nil, p, "condition", 3, BoolValue(true)); err == nil {
		t.Fatal("missing host signature was accepted")
	}
	dead := compileScript(t, vm, "return false && expensive()")
	data, _ := dead.MarshalBinary()
	loaded, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newVM(t, Config{}).Run(nil, loaded, "", 2); err != nil {
		t.Fatalf("dead host dependency was retained: %v", err)
	}
}

func TestFaultsAndLimits(t *testing.T) {
	vm := newVM(t, Config{})
	for _, source := range []string{
		"for {}",
		"for 4 > 2 { if false { break } }",
		"for true { switch { default: break } }",
		"return " + strings.Repeat("!", MaxNesting) + "true",
		"entry f(x int) int { if x > 0 { return x } }",
		"entry f() { f() }",
		"const left = -1",
	} {
		if _, err := vm.Compile(source); err == nil {
			t.Errorf("compiled invalid source %q", source)
		}
	}
	for _, tc := range []struct {
		source string
		err    error
	}{
		{"return 4 / 0", ErrDivisionByZero},
		{"return 4 << -1", ErrNegativeShift},
		{"for gas_left() > 0 {}", ErrOutOfGas},
	} {
		p := compileScript(t, vm, tc.source)
		if _, err := vm.Run(nil, p, "", 12); !errors.Is(err, tc.err) {
			t.Errorf("%q returned %v, want %v", tc.source, err, tc.err)
		}
	}
	zero := newVM(t, Config{Costs: map[Opcode]uint32{
		OpGasLeft: 0, OpPushInt: 0, OpGt: 0, OpJumpFalse: 0, OpJump: 0,
	}})
	loop := compileScript(t, zero, "for gas_left() > 0 {}")
	if res, err := zero.Run(nil, loop, "", 1); !errors.Is(err, ErrNoGasProgress) || res.GasUsed != 0 {
		t.Fatalf("free loop = %+v, %v", res, err)
	}
}

func TestHostReentryDepth(t *testing.T) {
	var program *Program
	calls := 0
	vm := newVM(t, Config{Functions: map[string]HostFunc{
		"again": {Run: func(call CallContext, _ []Value) (Value, error) {
			calls++
			_, err := call.VM.Run(call.Context, program, "", 3)
			return Value{}, err
		}},
	}})
	program = compileScript(t, vm, "again()")
	_, err := vm.Run(nil, program, "", 3)
	if !errors.Is(err, ErrRunDepth) || calls != MaxRunDepth {
		t.Fatalf("ran %d calls, error = %v", calls, err)
	}
}
