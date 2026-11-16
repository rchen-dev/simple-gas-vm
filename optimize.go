package gasvm

func reachable(code []instruction, start int) []bool {
	// The sentinel represents falling through the end of the region.
	seen := make([]bool, len(code)+1)
	queue := []int{start}
	seen[start] = true
	add := func(pc int) {
		if !seen[pc] {
			seen[pc] = true
			queue = append(queue, pc)
		}
	}
	for i := 0; i < len(queue); i++ {
		pc := queue[i]
		if pc == len(code) {
			continue
		}
		switch ins := code[pc]; ins.op {
		case OpReturn:
		case OpJump:
			add(ins.arg)
		case OpJumpFalse, OpJumpTrue, OpAndThen, OpOrElse:
			add(ins.arg)
			add(pc + 1)
		default:
			add(pc + 1)
		}
	}
	return seen
}

func optimize(code []instruction) []instruction {
	// Thread forward jump chains. Leave loop backedges in place.
	destination := make([]int, len(code))
	for pc := len(code) - 1; pc >= 0; pc-- {
		destination[pc] = pc
		if ins := code[pc]; ins.op == OpJump && ins.arg > pc {
			destination[pc] = destination[ins.arg]
		}
	}
	for pc, ins := range code {
		switch ins.op {
		case OpJump, OpJumpFalse, OpJumpTrue, OpAndThen, OpOrElse:
			if ins.arg > pc {
				code[pc].arg = destination[ins.arg]
			}
		}
	}
	seen := reachable(code, 0)
	// Remove unreachable instructions and jumps to the next live instruction.
	next := len(code)
	for pc := len(code) - 1; pc >= 0; pc-- {
		if !seen[pc] {
			continue
		}
		ins := code[pc]
		if ins.op == OpJump && ins.arg == next {
			seen[pc] = false
		} else {
			next = pc
		}
	}
	index := make([]int, len(code))
	kept := 0
	for pc := range code {
		index[pc] = kept
		if seen[pc] {
			kept++
		}
	}
	out := make([]instruction, 0, kept)
	for pc, ins := range code {
		if !seen[pc] {
			continue
		}
		switch ins.op {
		case OpJump, OpJumpFalse, OpJumpTrue, OpAndThen, OpOrElse:
			ins.arg = index[ins.arg]
		}
		out = append(out, ins)
	}
	return out
}
