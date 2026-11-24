package main

import (
	"fmt"
	"os"

	piglitter "github.com/SkrOYC/pig-litter/extensions/pig-litter"
)

func main() {
	var err error
	if len(os.Args) == 2 && os.Args[1] == "--pig-litter-member-check" {
		if !piglitter.NativeMemberAvailable() {
			fmt.Fprintln(os.Stderr, "native teammate support is unavailable in this build")
			os.Exit(1)
		}
		fmt.Println("pig-litter-native-member-v1")
		return
	} else if len(os.Args) == 3 && os.Args[1] == "--pig-litter-member" {
		err = piglitter.RunNativeMember(os.Args[2])
	} else {
		err = piglitter.Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
