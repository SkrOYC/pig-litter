package piglitter

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

type nativeMemberManifest struct {
	Version          int               `json:"version"`
	Backend          backendRequest    `json:"backend,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	Launch           string            `json:"launch"`
	Session          ChildID           `json:"session"`
	Cwd              string            `json:"cwd"`
	Model            string            `json:"model"`
	Tools            []string          `json:"tools"`
	SessionDirectory string            `json:"sessionDirectory"`
	ResumeFile       string            `json:"resumeFile,omitempty"`
	AgentDirectory   string            `json:"agentDirectory"`
	PigHome          string            `json:"pigHome,omitempty"`
	UsePiDirs        bool              `json:"usePiDirs,omitempty"`
	Socket           string            `json:"socket"`
	Capability       string            `json:"capability"`
	Prompt           string            `json:"prompt"`
	PeerWake         bool              `json:"peerWake,omitempty"`
	Instructions     string            `json:"instructions"`
	MailboxLimit     int               `json:"mailboxLimit"`
	ProjectTrusted   bool              `json:"projectTrusted"`
	Observer         bool              `json:"observer,omitempty"`
}

func readNativeMemberManifest(path string) (nativeMemberManifest, error) {
	var manifest nativeMemberManifest
	if !filepath.IsAbs(path) {
		return manifest, errors.New("native member manifest path must be absolute")
	}
	directory := filepath.Dir(path)
	canonical, err := canonicalTeamDirectory(directory)
	if err != nil || canonical != directory {
		return manifest, errors.New("native member manifest directory is not canonical")
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return manifest, errors.New("native member manifest directory must be private")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1024*1024 {
		return manifest, errors.New("native member manifest is not a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return manifest, errors.New("native member manifest changed during startup")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 || checkUniqueJSON(data) != nil {
		return manifest, errors.New("native member manifest is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nativeMemberManifest{}, errors.New("native member manifest is invalid")
	}
	if err := manifest.validate(directory, filepath.Base(path)); err != nil {
		return nativeMemberManifest{}, err
	}
	return manifest, nil
}

func (manifest nativeMemberManifest) validate(directory, name string) error {
	if manifest.Backend != "" && !isPaneBackend(manifest.Backend) || manifest.Version != 1 || !tmuxLaunchNonce.MatchString(manifest.Launch) || name != "member-"+manifest.Launch+".json" || !childUUID.MatchString(string(manifest.Session)) || !exactModel.MatchString(manifest.Model) || manifest.MailboxLimit < 1 || manifest.MailboxLimit > 1024 || len(manifest.Prompt) > 32768 || len(manifest.Instructions) > 32768 {
		return errors.New("native member manifest has an invalid identity or input")
	}
	if manifest.Backend == backendITerm && manifest.Environment == nil || manifest.Backend != backendITerm && manifest.Environment != nil {
		return errors.New("native member environment is invalid")
	}
	size := 0
	for name, value := range manifest.Environment {
		size += len(name) + len(value)
		if size > 512*1024 || !nativeRuntimeVariable(name) || strings.ContainsRune(value, 0) {
			return errors.New("native member environment is invalid")
		}
	}
	if manifest.PeerWake && (manifest.ResumeFile == "" || manifest.Prompt != "") || !manifest.PeerWake && strings.TrimSpace(manifest.Prompt) == "" {
		return errors.New("native member manifest has an invalid startup input")
	}
	for _, path := range []string{manifest.Cwd, manifest.SessionDirectory, manifest.AgentDirectory} {
		if canonical, err := canonicalTeamDirectory(path); err != nil || canonical != path {
			return errors.New("native member directory is not canonical")
		}
	}
	if manifest.PigHome != "" && !filepath.IsAbs(manifest.PigHome) {
		return errors.New("native member configuration root must be absolute")
	}
	if manifest.ResumeFile != "" && (!filepath.IsAbs(manifest.ResumeFile) || filepath.Clean(manifest.ResumeFile) != manifest.ResumeFile || filepath.Dir(manifest.ResumeFile) != manifest.SessionDirectory || !strings.HasSuffix(filepath.Base(manifest.ResumeFile), "_"+string(manifest.Session)+".jsonl")) {
		return errors.New("native member resume file does not match its session")
	}
	if !filepath.IsAbs(manifest.Socket) || filepath.Dir(manifest.Socket) != directory {
		return errors.New("native member control socket is outside its private directory")
	}
	capability, err := hex.DecodeString(manifest.Capability)
	if err != nil || len(capability) != 32 {
		return errors.New("native member capability is invalid")
	}
	if len(manifest.Tools) > 1024 || !slices.IsSorted(manifest.Tools) {
		return errors.New("native member tool ceiling is invalid")
	}
	if manifest.Observer && (manifest.PeerWake || manifest.ResumeFile != "" || !slices.Contains(manifest.Tools, "observer_report") || slices.ContainsFunc(manifest.Tools, func(name string) bool {
		return name != "observer_report" && name != "observer_read" && name != "observer_ls"
	})) {
		return errors.New("native observer tool ceiling is invalid")
	}
	for index, name := range manifest.Tools {
		if name == "" || len(name) > 256 || index > 0 && manifest.Tools[index-1] == name || strings.ContainsRune(name, ',') || strings.IndexFunc(name, unicode.IsSpace) >= 0 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return errors.New("native member tool ceiling is invalid")
		}
	}
	return nil
}
