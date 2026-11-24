package main

import (
	"fmt"
	"os"

	piglitter "github.com/SkrOYC/pig-litter/extensions/pig-litter"
)

func main() {
	var err error
	if len(os.Args) == 3 && os.Args[1] == "--pig-litter-member" {
		err = piglitter.RunNativeMember(os.Args[2])
	} else {
		err = piglitter.Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
