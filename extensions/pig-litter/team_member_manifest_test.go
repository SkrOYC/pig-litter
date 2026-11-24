package piglitter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeMemberManifestBindsPrivateLaunch(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work", "sessions", "agent"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	launch := "0123456789abcdef0123456789abcdef"
	path := filepath.Join(directory, "member-"+launch+".json")
	manifest := nativeMemberManifest{Version: 1, Launch: launch, Session: storeTestUUID(2), Cwd: filepath.Join(directory, "work"), Model: "fixture/fixed", Tools: []string{"read", "write"}, SessionDirectory: filepath.Join(directory, "sessions"), AgentDirectory: filepath.Join(directory, "agent"), PigHome: filepath.Join(directory, "pig"), UsePiDirs: true, Socket: filepath.Join(directory, "control.sock"), Capability: strings.Repeat("a", 64), Prompt: "Task:\ninspect", MailboxLimit: 2}
	manifest.ResumeFile = filepath.Join(manifest.SessionDirectory, "session_"+string(manifest.Session)+".jsonl")
	write := func(t *testing.T, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil || os.WriteFile(path, data, 0o600) != nil {
			t.Fatal("write manifest", err)
		}
	}
	write(t, manifest)
	loaded, err := readNativeMemberManifest(path)
	if err != nil || loaded.Session != manifest.Session || loaded.Model != manifest.Model || loaded.Cwd != manifest.Cwd || loaded.PigHome != manifest.PigHome || !loaded.UsePiDirs || loaded.ResumeFile != manifest.ResumeFile {
		t.Fatalf("valid manifest = %#v, %v", loaded, err)
	}
	for name, change := range map[string]func(*nativeMemberManifest){
		"duplicate tools": func(value *nativeMemberManifest) { value.Tools = []string{"read", "read"} },
		"foreign socket":  func(value *nativeMemberManifest) { value.Socket = filepath.Join(t.TempDir(), "socket") },
		"invalid model":   func(value *nativeMemberManifest) { value.Model = "fixed" },
		"empty seed":      func(value *nativeMemberManifest) { value.Prompt = " \n" },
		"bad capability":  func(value *nativeMemberManifest) { value.Capability = "not-a-capability" },
		"foreign session": func(value *nativeMemberManifest) {
			value.ResumeFile = filepath.Join(value.SessionDirectory, "other_"+string(storeTestUUID(4))+".jsonl")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := manifest
			change(&changed)
			write(t, changed)
			if _, err := readNativeMemberManifest(path); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
	iterm := manifest
	iterm.Backend = backendITerm
	iterm.Environment = map[string]string{"OPENAI_API_KEY": "fixture-only"}
	iterm.Observer = true
	iterm.ResumeFile = ""
	iterm.Tools = []string{"observer_ls", "observer_read", "observer_report"}
	write(t, iterm)
	loaded, err = readNativeMemberManifest(path)
	if err != nil || !loaded.Observer || loaded.Backend != backendITerm || loaded.Environment["OPENAI_API_KEY"] != "fixture-only" {
		t.Fatalf("private iTerm observer manifest = %#v, %v", loaded, err)
	}
	for _, environment := range []map[string]string{nil, {"PIG_EXT_SOCKET": "other"}, {"ITERM_SESSION_ID": "other"}} {
		iterm.Environment = environment
		write(t, iterm)
		if _, err := readNativeMemberManifest(path); err == nil {
			t.Fatal("invalid iTerm environment was accepted")
		}
	}
	write(t, manifest)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeMemberManifest(path); err == nil {
		t.Fatal("publicly readable manifest was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	duplicate := `{"version":1,"version":1}`
	if err := os.WriteFile(path, []byte(duplicate), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeMemberManifest(path); err == nil {
		t.Fatal("duplicate JSON member was accepted")
	}
}
