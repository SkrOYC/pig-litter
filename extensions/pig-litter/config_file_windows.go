package piglitter

import "os"

func openConfig(path string) (*os.File, error) { return os.Open(path) }
