package piglitter

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

func childDirectory(base, requested string) (string, error) {
	path := requested
	if path == "" {
		path = base
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", reject("rejected", fmt.Sprintf("child cwd is unavailable: %v", err))
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", reject("rejected", "child cwd must be an existing directory")
	}
	return path, nil
}

func childToolArgs(name, cwd string, args map[string]any) map[string]any {
	args = maps.Clone(args)
	if args == nil {
		return nil
	}
	if name == "bash" {
		if command, ok := args["command"].(string); ok {
			args["command"] = "cd -- '" + strings.ReplaceAll(cwd, "'", "'\\''") + "' && (\n" + command + "\n)"
		}
		return args
	}
	value, present := args["path"]
	path, ok := value.(string)
	if name == "ls" && (!present || ok && path == "") {
		args["path"] = cwd
	} else if ok && path != "" && !filepath.IsAbs(path) && path != "~" && !strings.HasPrefix(path, "~/") {
		args["path"] = filepath.Join(cwd, path)
	}
	return args
}
