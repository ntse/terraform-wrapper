package main

import (
	"fmt"
	"os"

	"terraform-wrapper/cmd/terraform-wrapper/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
