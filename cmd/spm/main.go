package main

import (
	"fmt"
	"os"

	"github.com/eyvant/spm/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "spm: %v\n", err)
		os.Exit(1)
	}
}
