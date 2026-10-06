package main

import (
	"fmt"
	"os"

	piglitter "github.com/SkrOYC/pig-litter/extensions/pig-litter"
)

func main() {
	if err := piglitter.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
