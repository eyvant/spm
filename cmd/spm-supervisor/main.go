// Command spm-supervisor is the spm supervisor runtime. spm starts it; it is not meant
// to be run by hand.
package main

import (
	"fmt"
	"os"

	"github.com/eyvant/spm/internal/runtime/supervisor"
)

func main() {
	if err := supervisor.Main(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "spm-supervisor: %v\n", err)
		os.Exit(1)
	}
}
