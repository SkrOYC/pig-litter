package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type previewNoticeEntry struct {
	Type       string          `json:"type"`
	CustomType string          `json:"customType"`
	ID         string          `json:"id"`
	Content    string          `json:"content"`
	Details    json.RawMessage `json:"details"`
}

type previewTurnNotice struct {
	Member        previewMemberRef `json:"member"`
	Execution     string           `json:"execution"`
	Kind          string           `json:"kind"`
	Report        string           `json:"report"`
	Error         string           `json:"error"`
	Delivery      string           `json:"delivery"`
	NativeEntryID string           `json:"nativeEntryId"`
}

type previewControlNotice struct {
	Kind          string          `json:"kind"`
	Request       json.RawMessage `json:"requestId"`
	Text          string          `json:"text"`
	Delivery      string          `json:"delivery"`
	NativeEntryID string          `json:"nativeEntryId"`
}

func (p *panelProof) nativeNoticeEntries() ([]previewNoticeEntry, error) {
	data, err := os.ReadFile(p.rootPath)
	if err != nil {
		return nil, err
	}
	var entries []previewNoticeEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var entry previewNoticeEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, err
		}
		if entry.Type == "custom_message" {
			entries = append(entries, entry)
		}
	}
	return entries, scanner.Err()
}

func (p *panelProof) noticeLedger(member previewMember) ([]byte, error) {
	return os.ReadFile(filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, fmt.Sprintf("plan-%s-%d.json", member.Ref.ID, member.Ref.Generation)))
}

func (p *panelProof) waitRenderedNotices(expected, forbidden []string) error {
	return waitFor(p.ctx, "rendered notice output", stageTimeout, func() (bool, error) {
		if err := p.rawActive(); err != nil {
			return false, err
		}
		data, err := os.ReadFile(filepath.Join(p.evidence, "pty.raw"))
		if err != nil {
			return false, err
		}
		for _, text := range forbidden {
			if bytes.Contains(data, []byte(text)) {
				return false, fmt.Errorf("raw notice JSON appeared in terminal output")
			}
		}
		for _, text := range expected {
			if !bytes.Contains(data, []byte(text)) {
				return false, nil
			}
		}
		return true, nil
	})
}

func (p *panelProof) captureTurnNotices(members []previewMember) error {
	ledgers := map[string][]byte{}
	if err := waitFor(p.ctx, "native turn notice receipts", stageTimeout, func() (bool, error) {
		entries, err := p.nativeNoticeEntries()
		if err != nil {
			return false, nil
		}
		for _, member := range members {
			data, err := p.noticeLedger(member)
			var ledger struct {
				Lifecycle struct {
					Turns []previewTurnNotice `json:"turns"`
				} `json:"lifecycle"`
			}
			if err != nil || json.Unmarshal(data, &ledger) != nil || len(ledger.Lifecycle.Turns) != 1 {
				return false, nil
			}
			notice := ledger.Lifecycle.Turns[0]
			if notice.Member != member.Ref || notice.Kind != "available" || notice.Report != "TEAM_REPORT_"+strings.ToUpper(member.Name) || notice.Delivery != "native_observed" || notice.NativeEntryID == "" {
				return false, nil
			}
			matched := false
			for _, entry := range entries {
				if entry.CustomType != "pig_litter_team_turn" || entry.ID != notice.NativeEntryID {
					continue
				}
				var payload previewTurnNotice
				var details struct {
					Notice previewTurnNotice `json:"notice"`
				}
				if json.Unmarshal([]byte(entry.Content), &payload) != nil || json.Unmarshal(entry.Details, &details) != nil || !reflect.DeepEqual(payload, details.Notice) || payload.Member != notice.Member || payload.Execution != notice.Execution || payload.Kind != notice.Kind || payload.Report != notice.Report {
					return false, fmt.Errorf("turn notice content did not match its native receipt")
				}
				matched = true
			}
			if !matched {
				return false, nil
			}
			ledgers[member.Name] = data
		}
		return true, nil
	}); err != nil {
		return err
	}
	if _, err := p.frame("team-readable-turn-notices"); err != nil {
		return err
	}
	var expected []string
	for _, member := range members {
		label := "Teammate " + member.Name + " | available"
		expected = append(expected, label, "TEAM_REPORT_"+strings.ToUpper(member.Name))
		if err := writePrivate(filepath.Join(p.evidence, "team-turn-notice-"+strings.ToLower(member.Name)+".json"), ledgers[member.Name]); err != nil {
			return err
		}
	}
	return p.waitRenderedNotices(expected, []string{`"kind":"available"`})
}

func (p *panelProof) captureShutdownNotices(member previewMember) error {
	var saved []byte
	if err := waitFor(p.ctx, "native shutdown notice receipts", stageTimeout, func() (bool, error) {
		data, err := p.noticeLedger(member)
		var ledger struct {
			Lifecycle struct {
				Notices []previewControlNotice `json:"notices"`
			} `json:"lifecycle"`
		}
		entries, entriesErr := p.nativeNoticeEntries()
		if err != nil || entriesErr != nil || json.Unmarshal(data, &ledger) != nil {
			return false, nil
		}
		for _, kind := range []string{"shutdown_approved", "shutdown_terminated"} {
			found := false
			for _, notice := range ledger.Lifecycle.Notices {
				if notice.Kind != kind || notice.Delivery != "native_observed" || notice.NativeEntryID == "" {
					continue
				}
				for _, entry := range entries {
					if entry.CustomType != "pig_litter_control" || entry.ID != notice.NativeEntryID {
						continue
					}
					var payload struct {
						Kind      string          `json:"kind"`
						RequestID json.RawMessage `json:"requestId"`
						Message   string          `json:"message"`
					}
					var details struct {
						Notice previewControlNotice `json:"notice"`
					}
					if json.Unmarshal([]byte(entry.Content), &payload) != nil || json.Unmarshal(entry.Details, &details) != nil || payload.Kind != notice.Kind || payload.Message != notice.Text || !bytes.Equal(payload.RequestID, notice.Request) || details.Notice.Kind != notice.Kind || details.Notice.Text != notice.Text || !bytes.Equal(details.Notice.Request, notice.Request) {
						return false, fmt.Errorf("shutdown notice content did not match its native receipt")
					}
					found = true
				}
			}
			if !found {
				return false, nil
			}
		}
		saved = data
		return true, nil
	}); err != nil {
		return err
	}
	if _, err := p.frame("team-readable-shutdown-notices"); err != nil {
		return err
	}
	if err := p.waitRenderedNotices([]string{"Teammate " + member.Name + " | shutdown accepted", "Teammate " + member.Name + " | shutdown complete"}, []string{`"kind":"shutdown_approved"`, `"kind":"shutdown_terminated"`}); err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-shutdown-notices.json"), saved)
}
