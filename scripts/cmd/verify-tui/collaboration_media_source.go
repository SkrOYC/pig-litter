package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

type collaborationSourceFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

type collaborationSourceSelection struct {
	Source          string                    `json:"source"`
	Fixture         string                    `json:"fixture"`
	SHA256          string                    `json:"sha256"`
	Algorithm       string                    `json:"algorithm"`
	Files           []collaborationSourceFile `json:"files"`
	Excluded        []string                  `json:"excluded"`
	GoExecutable    string                    `json:"go_executable"`
	GoSHA256        string                    `json:"go_sha256"`
	PigSource       string                    `json:"pig_source,omitempty"`
	ManifestSHA256  string                    `json:"manifest_sha256,omitempty"`
	WorkspaceSHA256 string                    `json:"workspace_sha256,omitempty"`
}

func collaborationSnapshotSource(source, destination string) ([]collaborationSourceFile, string, error) {
	var files []collaborationSourceFile
	var total int64
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.IsDir() && (relative == "dist" || relative == ".git") {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source fixture rejects symbolic link %q", relative)
		}
		if entry.IsDir() {
			if destination != "" {
				return os.Mkdir(filepath.Join(destination, relative), 0o700)
			}
			return nil
		}
		before, err := entry.Info()
		if err != nil {
			return err
		}
		if !before.Mode().IsRegular() || before.Size() > 8<<20 || len(files) >= 10000 {
			return fmt.Errorf("source fixture has unsupported file %q", relative)
		}
		total += before.Size()
		if total > 64<<20 {
			return errors.New("source fixture exceeds 64 MiB")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		writer := io.Writer(hash)
		var output *os.File
		if destination != "" {
			output, err = os.OpenFile(filepath.Join(destination, relative), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600|before.Mode().Perm()&0o111)
			if err != nil {
				_ = input.Close()
				return err
			}
			writer = io.MultiWriter(hash, output)
		}
		copied, copyErr := io.Copy(writer, io.LimitReader(input, before.Size()+1))
		closeErr := input.Close()
		if output != nil {
			closeErr = errors.Join(closeErr, output.Chmod(before.Mode().Perm()))
			closeErr = errors.Join(closeErr, output.Close())
		}
		after, statErr := os.Stat(path)
		if err := errors.Join(copyErr, closeErr, statErr); err != nil {
			return err
		}
		if !os.SameFile(before, after) || copied != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
			return fmt.Errorf("source file changed while copied: %q", relative)
		}
		files = append(files, collaborationSourceFile{Path: filepath.ToSlash(relative), Size: copied, Mode: uint32(before.Mode().Perm()), SHA256: hex.EncodeToString(hash.Sum(nil))})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", errors.New("source fixture is empty")
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(encoded)
	return files, hex.EncodeToString(digest[:]), nil
}

func collaborationPrepareSource(ctx context.Context, source, workspace, scratch, evidence string, environment []string) (collaborationSourceSelection, []string, error) {
	physical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		return collaborationSourceSelection{}, nil, errors.New("source selection requires Go on PATH")
	}
	goPath, err = filepath.Abs(goPath)
	if err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	fixture := filepath.Join(workspace, "extensions", "pig-litter")
	if err := os.MkdirAll(fixture, 0o700); err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	files, digest, err := collaborationSnapshotSource(physical, fixture)
	if err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	_, sourceDigest, err := collaborationSnapshotSource(physical, "")
	if err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	if digest != sourceDigest {
		return collaborationSourceSelection{}, nil, errors.New("source tree changed during fixture creation")
	}
	_, fixtureDigest, err := collaborationSnapshotSource(fixture, "")
	if err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	if digest != fixtureDigest {
		return collaborationSourceSelection{}, nil, errors.New("source fixture does not match its original tree")
	}
	if err := collaborationValidateSourceModule(fixture); err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	for _, name := range []string{"source-cache", "source-modules", "source-gopath", "source-tmp"} {
		if err := os.Mkdir(filepath.Join(scratch, name), 0o700); err != nil {
			return collaborationSourceSelection{}, nil, err
		}
	}
	selection := collaborationSourceSelection{Source: physical, Fixture: fixture, SHA256: digest, Algorithm: "sha256-json-file-list-v1", Files: files, Excluded: []string{".git", "dist"}}
	checkout := filepath.Dir(filepath.Dir(physical))
	manifest := filepath.Join(checkout, "pig-source.json")
	if filepath.Base(physical) == "pig-litter" && filepath.Base(filepath.Dir(physical)) == "extensions" {
		if _, err := os.Stat(manifest); err == nil {
			devenv, err := exec.LookPath("devenv")
			if err != nil {
				return collaborationSourceSelection{}, nil, errors.New("source selection requires devenv for the declared PiG source")
			}
			script := `set -eu
export GOPATH="$3/source-gopath" GOCACHE="$3/source-cache" GOMODCACHE="$3/source-modules" GOTMPDIR="$3/source-tmp" GOFLAGS=
unset PIG_LITTER_WORKSPACE_BRIDGE
bash "$DEVENV_ROOT/scripts/prepare-pig-workspace.sh" "$PIG_LITTER_SOURCE_DIR" "$1" "$2" >/dev/null
printf '%s\n%s\n' "$(command -v go)" "$PIG_LITTER_SOURCE_DIR"`
			command := exec.CommandContext(ctx, devenv, "shell", "--", "bash", "-c", script, "collaboration-source", workspace, fixture, scratch)
			command.Dir = checkout
			command.Env = append(append([]string{}, environment...), "PATH="+os.Getenv("PATH"), "GOENV=off", "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off")
			output, err := command.Output()
			if err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return collaborationSourceSelection{}, nil, fmt.Errorf("prepare target source workspace: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
				}
				return collaborationSourceSelection{}, nil, fmt.Errorf("prepare target source workspace: %w", err)
			}
			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
			if len(lines) != 2 || !filepath.IsAbs(lines[0]) || !filepath.IsAbs(lines[1]) {
				return collaborationSourceSelection{}, nil, errors.New("target source workspace did not identify Go and PiG source")
			}
			goPath, selection.PigSource = lines[0], lines[1]
			selection.ManifestSHA256, err = fileSHA256(manifest)
			if err != nil {
				return collaborationSourceSelection{}, nil, err
			}
			workData, err := os.ReadFile(filepath.Join(workspace, "go.work"))
			if err != nil {
				return collaborationSourceSelection{}, nil, err
			}
			workHash := sha256.Sum256(workData)
			selection.WorkspaceSHA256 = hex.EncodeToString(workHash[:])
			if err := os.WriteFile(filepath.Join(evidence, "source-workspace.go.work"), workData, 0o600); err != nil {
				return collaborationSourceSelection{}, nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return collaborationSourceSelection{}, nil, err
		}
	}
	selection.GoExecutable = goPath
	selection.GoSHA256, err = fileSHA256(goPath)
	if err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	if err := writeCaptureJSON(filepath.Join(evidence, "source-selection.json"), selection); err != nil {
		return collaborationSourceSelection{}, nil, err
	}
	for i, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			environment[i] = "PATH=" + filepath.Dir(goPath) + string(os.PathListSeparator) + strings.TrimPrefix(entry, "PATH=")
		}
	}
	goWork := "off"
	goFlags := "-mod=readonly -modcacherw -p=2"
	if selection.PigSource != "" {
		goWork = filepath.Join(workspace, "go.work")
		goFlags = "-modcacherw -p=2"
		environment = append(environment, "PIG_SOURCE_ROOT="+selection.PigSource, "PIG_SDK_GO_ROOT="+filepath.Join(selection.PigSource, "extensions", "sdk"))
	}
	environment = append(environment,
		"GOCACHE="+filepath.Join(scratch, "source-cache"), "GOMODCACHE="+filepath.Join(scratch, "source-modules"),
		"GOPATH="+filepath.Join(scratch, "source-gopath"), "GOTMPDIR="+filepath.Join(scratch, "source-tmp"),
		"GOENV=off", "GOTELEMETRY=off", "GOWORK="+goWork, "GOTOOLCHAIN=local", "GOFLAGS="+goFlags)
	return selection, environment, nil
}

func collaborationValidateSourceModule(fixture string) error {
	data, err := os.ReadFile(filepath.Join(fixture, "go.mod"))
	if err != nil {
		return err
	}
	module, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return err
	}
	for _, replacement := range module.Replace {
		if replacement.New.Version == "" {
			return errors.New("source fixture does not permit local module replacements")
		}
	}
	return nil
}
