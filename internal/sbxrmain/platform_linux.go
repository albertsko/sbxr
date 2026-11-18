package sbxrmain

import (
	"fmt"
	"os"

	"github.com/albertsko/sbxr/internal/userns"
	"github.com/albertsko/sbxr/internal/vm"
)

// platformMain finishes entering a user namespace and runs the hidden user namespace check.
func platformMain(args []string) (int, bool) {
	err := userns.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbxr: user namespace: %v\n", err)

		return vm.ExitSetup, true
	}

	if len(args) > 0 && args[0] == userns.CheckArg {
		return userns.CheckInside(), true
	}

	return 0, false
}
