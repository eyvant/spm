// Command spm-scheduler is the spm scheduler runtime. spm starts it; it is not meant
// to be run by hand.
package main

import (
	"fmt"
	"os"

	"github.com/eyvant/spm/internal/runtime/scheduler"
)

func main() {
	if err := scheduler.Main(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "spm-scheduler: %v\n", err)
		os.Exit(1)
	}
}
