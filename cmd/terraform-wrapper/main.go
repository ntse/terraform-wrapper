package main

import (
	"log"

	"terraform-wrapper/cmd/terraform-wrapper/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		log.Fatalf("error: %v", err)
	}
}
