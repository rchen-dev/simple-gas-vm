# Simple Gas VM

An importable Go library for compiling and running small, gas-metered decision scripts. The host supplies domain-specific constants and functions, controls the budget, and owns any external state.

```sh
go get github.com/rchen-dev/simple-gas-vm
go run ./examples/decision
```

The [runnable example](examples/decision/main.go) registers `see_friend`, `wave`, and `turn`, then compiles this script and runs it from saved bytecode. A scout pays less for `see_friend` than a regular entity.

```go
for gas_left() > 16 {
    if see_friend() {
        wave()
        return
    }
    turn(left)
}
```

## Go API

- Construct a VM with `gasvm.New(gasvm.Config{...})`. Register constants as `map[string]gasvm.Value` and helpers as `map[string]gasvm.HostFunc`. Set `Config.Caller` to the entity running the script. Host callbacks can access it through `call.VM.Caller()`.
- Compile source with `vm.Compile(source)`. Save a program with `program.MarshalBinary()` and load it with `gasvm.Load(data)`. Bytecode includes a format version, embedded constant values, and the names and signatures of the host calls it uses. A compatible VM can use different callers, callback implementations, and gas costs with the same program.
- Execute with `vm.Run(ctx, program, "", budget)`. Pass an entry name and typed arguments to invoke a named entry. `Result` reports both the return value and gas spent, including gas spent before an error.
- Cancel the context or set a deadline to stop a run between instructions. Host callbacks can stop their own work by observing `CallContext.Context`.
- Share a compiled program across VM instances. Concurrent runs have independent locals, stacks, and budgets. Synchronize shared state in host callbacks as needed.

## Script language

- Values are signed 32-bit `int` (also spelled `int32`) and `bool`. Integer arithmetic wraps, right shifts are arithmetic, and division or remainder by zero and negative shifts fail the run.
- Scripts support scoped locals with `:=`, assignment, compound assignment, `++` and `--`, and Go operator precedence. Logical `&&` and `||` short-circuit. Locals live for one run.
- Control flow includes `if`/`else`, condition-only and three-clause `for` loops, `switch` with multiple expressions per case and optional `default`, unlabeled `break` and `continue`, and `return`. A switch evaluates its tag once and tests cases in order, exiting after the first match.
- The top-level script may return a value or finish without one. Put named entries after top-level statements. For example, `entry decide(square int) bool { return threats(square) == 0 }` defines a host-callable entry with one argument and one result. Entries may also omit a return type. Script calls are limited to Go-registered helpers and `gas_left()`.
- Declare script constants in Go through `Config.Constants`. They are available as bare identifiers, such as `left` in the example.

## Metering and limits

Every opcode defaults to one gas. Set `Config.Costs` to override individual opcodes, including setting a cost to zero. `gas_left()` has zero additional helper cost and reports the budget after paying for its own opcode. The run budget is at most `gasvm.MaxGas` so its value fits an `int32`.

A registered helper adds its fixed `Gas` cost or the amount returned by `Quote`. The VM first charges the call opcode, then quotes using the caller and arguments, then charges the full helper cost before invoking `Run`. If the helper charge exceeds the remaining budget, its action never starts. Quotes should be cheap and read-only. The host owns side effects already performed by callbacks when a later instruction fails.

The compiler rejects unconditional or constant-true loops with no reachable `break` or `return`. For state-dependent conditions or exits it relies on runtime gas. The compiler caps nesting at 64. When a Go helper starts a nested run, pass its `CallContext.Context` to enforce the 64-run depth limit. A custom schedule that allows 65,536 consecutive instructions without charging gas halts with `ErrNoGasProgress`.

See [compiler optimizations](docs/optimizations.md) for details on bytecode size and how optimization affects gas.
