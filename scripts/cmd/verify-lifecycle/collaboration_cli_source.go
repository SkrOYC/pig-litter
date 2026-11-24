package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"golang.org/x/mod/modfile"
)

type collaborationCLISourceFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

type collaborationCLISourceSelection struct {
	Source          string                       `json:"source"`
	Fixture         string                       `json:"fixture"`
	SHA256          string                       `json:"sha256"`
	Algorithm       string                       `json:"algorithm"`
	Files           []collaborationCLISourceFile `json:"files"`
	Excluded        []string                     `json:"excluded"`
	GoExecutable    string                       `json:"go_executable"`
	GoSHA256        string                       `json:"go_sha256"`
	PigSource       string                       `json:"pig_source,omitempty"`
	ManifestSHA256  string                       `json:"manifest_sha256,omitempty"`
	WorkspaceSHA256 string                       `json:"workspace_sha256,omitempty"`
}

func (runner *collaborationCLIRunner) validateSourceSelection(target collaborationCLITarget, evidence string, data []byte) error {
	var selection, receipt collaborationCLISourceSelection
	if err := json.Unmarshal(data, &selection); err != nil {
		return fmt.Errorf("decode source selection: %w", err)
	}
	receiptData, err := os.ReadFile(filepath.Join(evidence, "source-selection.json"))
	if err != nil || json.Unmarshal(receiptData, &receipt) != nil || !reflect.DeepEqual(selection, receipt) {
		return errors.New("source selection receipt differs from terminal result")
	}
	source, err := filepath.EvalSymlinks(filepath.Join(target.Checkout, "extensions", "pig-litter"))
	if err != nil || selection.Source != source || !filepath.IsAbs(selection.Fixture) || selection.Source == selection.Fixture || selection.Algorithm != "sha256-json-file-list-v1" || !reflect.DeepEqual(selection.Excluded, []string{".git", "dist"}) || len(selection.Files) == 0 {
		return errors.New("source selection does not identify the target checkout")
	}
	goDigest, err := collaborationCLIDigest(selection.GoExecutable)
	if err != nil || goDigest != selection.GoSHA256 {
		return errors.New("source selection does not identify its Go executable")
	}
	manifest := filepath.Join(target.Checkout, "pig-source.json")
	if _, err := os.Stat(manifest); err == nil {
		manifestDigest, err := collaborationCLIDigest(manifest)
		if err != nil || manifestDigest != selection.ManifestSHA256 || !filepath.IsAbs(selection.PigSource) {
			return errors.New("source selection does not identify the target's declared PiG source")
		}
		workspaceData, err := os.ReadFile(filepath.Join(evidence, "source-workspace.go.work"))
		if err != nil || collaborationTestDataDigest(workspaceData) != selection.WorkspaceSHA256 {
			return errors.New("source workspace differs from its receipt")
		}
		work, err := modfile.ParseWork("go.work", workspaceData, nil)
		if err != nil || len(work.Use) != 3 {
			return errors.New("source workspace has an invalid module closure")
		}
		want := map[string]bool{selection.Fixture: true, selection.PigSource: true, filepath.Join(selection.PigSource, "extensions", "sdk"): true}
		for _, use := range work.Use {
			if !want[use.Path] {
				return errors.New("source workspace does not use the target fixture and both PiG modules")
			}
			delete(want, use.Path)
		}
		if len(want) != 0 {
			return errors.New("source workspace has duplicate or missing modules")
		}
	} else if !errors.Is(err, os.ErrNotExist) || selection.PigSource != "" || selection.ManifestSHA256 != "" || selection.WorkspaceSHA256 != "" {
		return errors.New("source workspace has no target manifest")
	}
	encoded, err := json.Marshal(selection.Files)
	if err != nil || collaborationTestDataDigest(encoded) != selection.SHA256 {
		return errors.New("source file manifest digest is invalid")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	seen := make(map[string]bool, len(selection.Files))
	previous := ""
	for _, entry := range selection.Files {
		if entry.Path <= previous || !filepath.IsLocal(filepath.FromSlash(entry.Path)) || seen[entry.Path] {
			return errors.New("source file manifest has invalid or unordered paths")
		}
		previous, seen[entry.Path] = entry.Path, true
		file, err := collaborationOpenFile(root, filepath.FromSlash(entry.Path))
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		hash := sha256.New()
		size, readErr := io.Copy(hash, io.LimitReader(file, 8<<20+1))
		closeErr := file.Close()
		if err := errors.Join(statErr, readErr, closeErr); err != nil {
			return err
		}
		if size != entry.Size || uint32(info.Mode().Perm()) != entry.Mode || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("source file %q differs from its selection receipt", entry.Path)
		}
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		if entry.IsDir() && (relative == ".git" || relative == "dist") {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !seen[filepath.ToSlash(relative)] || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("unrecorded source file %q", relative)
		}
		return nil
	})
}
