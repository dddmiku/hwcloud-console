package main

import (
	"os"

	"hwcloud/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args))
}