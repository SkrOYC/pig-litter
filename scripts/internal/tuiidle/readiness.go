package tuiidle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Readiness struct {
	Command        string    `json:"command"`
	Nonce          string    `json:"nonce"`
	RootID         string    `json:"rootId"`
	PID            int       `json:"pid"`
	StartedAt      time.Time `json:"startedAt"`
	WaitReturnedAt time.Time `json:"waitReturnedAt"`
	ObservedAt     time.Time `json:"observedAt"`
	WaitError      string    `json:"waitError"`
	IdleError      string    `json:"idleError"`
	SessionError   string    `json:"sessionError"`
	Idle           bool      `json:"idle"`
}

func Observe(command, nonce string, wait func() error, idle func() (bool, error), session func() (string, error)) Readiness {
	result := Readiness{Command: command, Nonce: nonce, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := wait(); err != nil {
		result.WaitError = err.Error()
	}
	result.WaitReturnedAt = time.Now().UTC()
	var err error
	result.Idle, err = idle()
	if err != nil {
		result.IdleError = err.Error()
	}
	result.RootID, err = session()
	if err != nil {
		result.SessionError = err.Error()
	}
	result.ObservedAt = time.Now().UTC()
	return result
}

func (r Readiness) Validate(command, nonce, root string, requestedAt time.Time) error {
	if r.Command != command || r.Nonce != nonce || r.RootID != root || root == "" {
		return fmt.Errorf("idle readiness identity mismatch")
	}
	if r.WaitError != "" || r.IdleError != "" || r.SessionError != "" || !r.Idle {
		return fmt.Errorf("idle readiness failed: wait=%q idle=%t idleError=%q session=%q", r.WaitError, r.Idle, r.IdleError, r.SessionError)
	}
	if r.PID <= 0 || requestedAt.IsZero() || r.StartedAt.IsZero() || r.WaitReturnedAt.IsZero() || r.ObservedAt.IsZero() || r.StartedAt.Before(requestedAt) || r.WaitReturnedAt.Before(r.StartedAt) || r.ObservedAt.Before(r.WaitReturnedAt) || r.ObservedAt.After(time.Now()) {
		return fmt.Errorf("idle readiness is stale or incomplete")
	}
	return nil
}

func Decode(data []byte) (Readiness, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Readiness{}, err
	}
	for _, name := range []string{"command", "nonce", "rootId", "pid", "startedAt", "waitReturnedAt", "observedAt", "waitError", "idleError", "sessionError", "idle"} {
		if value, ok := fields[name]; !ok || string(value) == "null" {
			return Readiness{}, fmt.Errorf("idle readiness missing %s", name)
		}
	}
	var result Readiness
	err := json.Unmarshal(data, &result)
	return result, err
}

func Write(directory string, readiness Readiness) error {
	file, err := os.CreateTemp(directory, ".idle-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(readiness); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(directory, readiness.Nonce+".json"))
}
