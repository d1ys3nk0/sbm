package main

import (
	"fmt"
	"os"

	"github.com/d1ys3nk0/sbm/internal/controller"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("sbm %s (%s)\n", version, commit)
		return
	}
	if err := controller.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
