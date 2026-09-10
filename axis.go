package main

import (
	"github.com/0x7d8/wings/cmd"
)

func main() {
	// math/rand is auto-seeded since Go 1.20; no manual Seed needed.
	cmd.Execute()
}
