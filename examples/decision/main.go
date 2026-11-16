package main

import (
	"context"
	"fmt"
	"log"

	"github.com/rchen-dev/simple-gas-vm"
)

const script = `
for gas_left() > 16 {
	if see_friend() {
		wave()
		return
	}
	turn(left)
}

entry should_wave(distance int) bool {
	switch distance {
	case 0, 1:
		return see_friend()
	default:
		return false
	}
}
`

type entity struct {
	scout bool
	turns int
	waves int
}

func makeVM(caller *entity) *gasvm.VM {
	vm, err := gasvm.New(gasvm.Config{
		Caller:    caller,
		Constants: map[string]gasvm.Value{"left": gasvm.IntValue(-1)},
		Functions: map[string]gasvm.HostFunc{
			"see_friend": {
				Result: gasvm.Bool,
				Quote: func(ctx gasvm.CallContext, _ []gasvm.Value) (uint32, error) {
					if ctx.VM.Caller().(*entity).scout {
						return 2, nil
					}
					return 7, nil
				},
				Run: func(ctx gasvm.CallContext, _ []gasvm.Value) (gasvm.Value, error) {
					return gasvm.BoolValue(ctx.VM.Caller().(*entity).turns > 0), nil
				},
			},
			"wave": {
				Gas: 2,
				Run: func(ctx gasvm.CallContext, _ []gasvm.Value) (gasvm.Value, error) {
					ctx.VM.Caller().(*entity).waves++
					return gasvm.Value{}, nil
				},
			},
			"turn": {
				Params: []gasvm.Type{gasvm.Int},
				Gas:    2,
				Run: func(ctx gasvm.CallContext, args []gasvm.Value) (gasvm.Value, error) {
					direction, _ := args[0].Int32()
					if direction != -1 {
						return gasvm.Value{}, fmt.Errorf("unknown direction %d", direction)
					}
					ctx.VM.Caller().(*entity).turns++
					return gasvm.Value{}, nil
				},
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	return vm
}

func main() {
	ctx := context.Background()
	scout := &entity{scout: true}
	vm := makeVM(scout)
	program, err := vm.Compile(script)
	if err != nil {
		log.Fatal(err)
	}
	data, err := program.MarshalBinary()
	if err != nil {
		log.Fatal(err)
	}
	loaded, err := gasvm.Load(data)
	if err != nil {
		log.Fatal(err)
	}
	regular := &entity{}
	for _, run := range []struct {
		name   string
		vm     *gasvm.VM
		caller *entity
	}{{"scout", vm, scout}, {"regular", makeVM(regular), regular}} {
		result, err := run.vm.Run(ctx, loaded, "", 50)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s used %d gas and waved %d time(s)\n", run.name, result.GasUsed, run.caller.waves)
	}
	result, err := vm.Run(ctx, loaded, "should_wave", 20, gasvm.IntValue(1))
	if err != nil {
		log.Fatal(err)
	}
	shouldWave, _ := result.Value.Bool()
	fmt.Printf("named entry returned %t\n", shouldWave)
	_, err = vm.Run(ctx, loaded, "should_wave", 7, gasvm.IntValue(1))
	fmt.Printf("limited budget: %v\n", err)
}
