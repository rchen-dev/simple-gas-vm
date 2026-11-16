# Compiler optimizations

Gas is charged on the final bytecode. The compiler preserves the order of evaluated calls and possible arithmetic faults. Short-circuited expressions and eliminated instructions spend no gas. This can increase the value a later `gas_left()` sees. Pin the compiler version and opcode schedule if exact gas budgets are part of your rules.

| Implemented optimization | Effect |
| --- | --- |
| Fold integer, boolean, comparison, and bitwise constant expressions, including Go-supplied constants | Emit one literal in place of an expression. Keep faulting operations on paths that can execute. |
| Simplify constant boolean operands and constant guards | Drop unreachable right-hand expressions and branches. Keep the left-side evaluation in `x && true` and `x \|\| false`. |
| Use short-circuit branches, invert direct negated guards, and jump directly on matching switch cases | Skip unnecessary calls and avoid materializing extra booleans or branch instructions. |
| Resolve forward jump chains, remove jumps to the next instruction, and omit unreachable code | Reduce dispatches and bytecode length in decision trees and switches. |
| Remove unused host references and encode operands with variable-length integers | Reduce stored bytecode size, including small literals and nearby jump targets. |

Two candidates for a later pass are propagation across local assignments and dispatch tables for dense integer switches. Measure them against real scripts. Propagation has to respect reassignment and control-flow joins. A dispatch table has to preserve source-order evaluation of dynamic cases, so only a suitable set of constant cases can be grouped. Caller-dependent functions and `gas_left()` are runtime observations. Reusing or moving their results would require stronger rules about when those observations are valid.
