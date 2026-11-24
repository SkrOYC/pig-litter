package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--pig-litter-member" {
		os.Exit(2)
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
	if err := os.WriteFile(filepath.Join(filepath.Dir(os.Args[2]), "stubborn-ready"), []byte("ready"), 0o600); err != nil {
		os.Exit(1)
	}
	for {
		time.Sleep(time.Hour)
	}
}
