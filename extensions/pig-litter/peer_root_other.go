//go:build !linux && !darwin

package piglitter

import (
	"context"
	"encoding/json"
	"errors"
)

type localRuntime struct{}

func localOptIn() (bool, bool, error) { return false, false, nil }
func (o *owner) startLocal(bool) error {
	return errors.New("local sessions are unsupported on this platform")
}
func (o *owner) stopLocal() error                     { return nil }
func (o *owner) setLocalBusy(bool) error              { return nil }
func (o *owner) localDiscovery(string) map[string]any { return map[string]any{"state": "unsupported"} }
func (o *owner) localTerminalList() string {
	return "Independent local sessions are unsupported on this platform."
}
func (o *owner) localOperation(context.Context, string, json.RawMessage) (map[string]any, error) {
	return map[string]any{"state": "unsupported"}, nil
}
func (o *owner) addressedSend(context.Context, json.RawMessage) (map[string]any, error) {
	return map[string]any{"state": "unsupported"}, nil
}
