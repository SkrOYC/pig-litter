package tuiidle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadinessWaitsForActualCompletionBeforePublishing(t *testing.T) {
	directory := t.TempDir()
	requestedAt := time.Now().UTC()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		result := Observe("fixture-idle-test", "nonce", func() error {
			close(entered)
			<-release
			return nil
		}, func() (bool, error) { return true, nil }, func() (string, error) { return "root", nil })
		done <- Write(directory, result)
	}()
	<-entered
	path := filepath.Join(directory, "nonce.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending wait published readiness: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate("fixture-idle-test", "nonce", "root", requestedAt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private readiness mode=%v, error=%v", info, err)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 || files[0].Name() != "nonce.json" {
		t.Fatalf("published files=%v, error=%v", files, err)
	}
}

func TestReadinessRejectsErrorsIdentityAndStaleResponses(t *testing.T) {
	requestedAt := time.Now().Add(-time.Second)
	valid := Readiness{Command: "fixture-idle-test", Nonce: "nonce", RootID: "root", PID: 42, StartedAt: requestedAt.Add(time.Millisecond), WaitReturnedAt: requestedAt.Add(2 * time.Millisecond), ObservedAt: requestedAt.Add(3 * time.Millisecond), Idle: true}
	if err := valid.Validate("fixture-idle-test", "nonce", "root", requestedAt); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Readiness){
		"command":       func(r *Readiness) { r.Command = "other" },
		"nonce":         func(r *Readiness) { r.Nonce = "stale" },
		"root":          func(r *Readiness) { r.RootID = "other-root" },
		"wait error":    func(r *Readiness) { r.WaitError = "cancelled" },
		"idle error":    func(r *Readiness) { r.IdleError = "transport failed" },
		"session error": func(r *Readiness) { r.SessionError = "session unavailable" },
		"not idle":      func(r *Readiness) { r.Idle = false },
		"pid":           func(r *Readiness) { r.PID = 0 },
		"missing start": func(r *Readiness) { r.StartedAt = time.Time{} },
		"stale start":   func(r *Readiness) { r.StartedAt = requestedAt.Add(-time.Millisecond) },
		"wait order":    func(r *Readiness) { r.WaitReturnedAt = r.StartedAt.Add(-time.Millisecond) },
		"ready order":   func(r *Readiness) { r.ObservedAt = r.WaitReturnedAt.Add(-time.Millisecond) },
		"future":        func(r *Readiness) { r.ObservedAt = time.Now().Add(time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			change(&bad)
			if err := bad.Validate("fixture-idle-test", "nonce", "root", requestedAt); err == nil {
				t.Fatalf("accepted invalid readiness: %#v", bad)
			}
		})
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		t.Run("missing "+key, func(t *testing.T) {
			var incomplete map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &incomplete); err != nil {
				t.Fatal(err)
			}
			delete(incomplete, key)
			data, _ := json.Marshal(incomplete)
			if _, err := Decode(data); err == nil {
				t.Fatalf("accepted missing %s", key)
			}
		})
		t.Run("null "+key, func(t *testing.T) {
			var incomplete map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &incomplete); err != nil {
				t.Fatal(err)
			}
			incomplete[key] = json.RawMessage("null")
			data, _ := json.Marshal(incomplete)
			if _, err := Decode(data); err == nil {
				t.Fatalf("accepted null %s", key)
			}
		})
	}
	for _, malformed := range []string{`{"idle":`, `null`, `[]`, `{"idle":"true"}`} {
		if _, err := Decode([]byte(malformed)); err == nil {
			t.Fatalf("accepted malformed readiness %q", malformed)
		}
	}
}

func TestObserveRecordsEveryPublicCallFailure(t *testing.T) {
	var order []string
	result := Observe("fixture-idle-test", "nonce", func() error {
		order = append(order, "wait")
		return errors.New("wait failed")
	}, func() (bool, error) {
		order = append(order, "idle")
		return false, errors.New("idle failed")
	}, func() (string, error) {
		order = append(order, "session")
		return "", errors.New("session failed")
	})
	if len(order) != 3 || order[0] != "wait" || order[1] != "idle" || order[2] != "session" || result.WaitError != "wait failed" || result.IdleError != "idle failed" || result.SessionError != "session failed" || result.Idle {
		t.Fatalf("public observations=%#v, order=%v", result, order)
	}
}
