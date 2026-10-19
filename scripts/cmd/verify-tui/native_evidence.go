package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type nativeChild struct {
	RootID        string            `json:"rootId"`
	ID            string            `json:"id"`
	ParentID      string            `json:"parentId"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Model         string            `json:"model"`
	Cwd           string            `json:"cwd"`
	State         string            `json:"state"`
	Paused        bool              `json:"paused"`
	Admitted      bool              `json:"admitted"`
	NoticeID      string            `json:"noticeId"`
	Notices       []json.RawMessage `json:"notices"`
	PendingInputs []json.RawMessage `json:"pendingInputs"`
	Latest        *nativeOutcome    `json:"latest"`
}

type nativeOutcome struct {
	State, Model, Text, Error string
	Truncated                 bool
	ReportProvenance          string
	Run                       struct{ ID string }
	Usage                     json.RawMessage
	Raw                       json.RawMessage `json:"raw"`
}

func (outcome *nativeOutcome) UnmarshalJSON(data []byte) error {
	type decodedOutcome nativeOutcome
	if err := json.Unmarshal(data, (*decodedOutcome)(outcome)); err != nil {
		return err
	}
	outcome.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type nativeCompletion struct {
	EntryID   string          `json:"entryId"`
	Timestamp string          `json:"timestamp"`
	ChildID   string          `json:"childId"`
	Receipt   string          `json:"receipt"`
	Content   string          `json:"content"`
	Display   bool            `json:"display"`
	Raw       json.RawMessage `json:"raw"`
}

type nativeSession struct {
	ID            string                 `json:"id"`
	Cwd           string                 `json:"cwd"`
	Path          string                 `json:"path"`
	Entries       int                    `json:"entries"`
	LitterEntries int                    `json:"litterEntries"`
	Children      map[string]nativeChild `json:"children"`
	ChildOrder    []string               `json:"childOrder"`
	Messages      []json.RawMessage      `json:"messages"`
	Completions   []nativeCompletion     `json:"completions"`
	Raw           []byte                 `json:"-"`
}

func readNativeSession(path string) (nativeSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nativeSession{}, err
	}
	return parseNativeSession(path, data)
}

func parseNativeSession(path string, data []byte) (nativeSession, error) {
	view := nativeSession{Path: path, Children: map[string]nativeChild{}, Raw: data}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var entry struct {
			Type, ID, Cwd, CustomType, Timestamp string
			Data                                 json.RawMessage
			Message                              json.RawMessage
			Details                              json.RawMessage
			Content                              string
			Display                              bool
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nativeSession{}, fmt.Errorf("read genuine native record %s: %w", path, err)
		}
		view.Entries++
		switch entry.Type {
		case "session":
			view.ID, view.Cwd = entry.ID, entry.Cwd
		case "message":
			view.Messages = append(view.Messages, append(json.RawMessage(nil), entry.Message...))
		case "custom_message":
			if entry.CustomType == "litter_completion" {
				var receipt struct {
					ChildID string `json:"childId"`
					Receipt string `json:"receipt"`
				}
				if err := json.Unmarshal(entry.Details, &receipt); err != nil {
					return nativeSession{}, err
				}
				view.Completions = append(view.Completions, nativeCompletion{EntryID: entry.ID, Timestamp: entry.Timestamp, ChildID: receipt.ChildID, Receipt: receipt.Receipt, Content: entry.Content, Display: entry.Display, Raw: append(json.RawMessage(nil), scanner.Bytes()...)})
			}
		case "custom":
			if entry.CustomType == "litter_child_v1" {
				var child nativeChild
				if err := json.Unmarshal(entry.Data, &child); err != nil {
					return nativeSession{}, err
				}
				if _, exists := view.Children[child.ID]; !exists {
					view.ChildOrder = append(view.ChildOrder, child.ID)
				}
				view.Children[child.ID] = child
				view.LitterEntries++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nativeSession{}, err
	}
	if view.ID == "" {
		return nativeSession{}, fmt.Errorf("native session header missing in %s", path)
	}
	return view, nil
}

func readNativeSessions(roots ...string) ([]nativeSession, error) {
	paths, err := nativePaths(roots...)
	if err != nil {
		return nil, err
	}
	var sessions []nativeSession
	for _, path := range paths {
		view, err := readNativeSession(path)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, view)
	}
	return sessions, nil
}

func nativePaths(roots ...string) ([]string, error) {
	var paths []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func rootNativeSession(directory string) (nativeSession, error) {
	sessions, err := readNativeSessions(directory)
	if err != nil {
		return nativeSession{}, err
	}
	if len(sessions) != 1 {
		return nativeSession{}, fmt.Errorf("expected one genuine root session, found %d", len(sessions))
	}
	return sessions[0], nil
}

func saveNativeEvidence(destination string, roots ...string) error {
	paths, err := nativePaths(roots...)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	var failures []error
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		session, err := parseNativeSession(path, raw)
		if err != nil {
			failures = append(failures, err)
			if err := writePrivate(filepath.Join(destination, filepath.Base(path)), raw); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if filepath.Base(session.ID) != session.ID {
			return fmt.Errorf("invalid native session identity %q", session.ID)
		}
		if err := writePrivate(filepath.Join(destination, session.ID+".jsonl"), session.Raw); err != nil {
			return err
		}
		data, err := json.MarshalIndent(session, "", "  ")
		if err != nil {
			return err
		}
		if err := writePrivate(filepath.Join(destination, session.ID+".projection.json"), data); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func nativeDigests(roots ...string) (map[string]string, error) {
	paths, err := nativePaths(roots...)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		digest, err := fileSHA256(path)
		if err != nil {
			return nil, err
		}
		result[path] = digest
	}
	return result, nil
}

func completionMatches(completion nativeCompletion, child nativeChild, receipt string) error {
	var content struct {
		Origin, Kind, ReportProvenance, ID, State, Model, Report, Error string
		Truncated                                                       bool
	}
	if err := json.Unmarshal([]byte(completion.Content), &content); err != nil {
		return err
	}
	latest := child.Latest
	if latest == nil || latest.Run.ID != child.ID || child.NoticeID != receipt || completion.Display || completion.Receipt != receipt || completion.ChildID != child.ID || content.Origin != "pig-litter" || content.Kind != "litter_completion" || content.ReportProvenance != "child_assistant" || content.ReportProvenance != latest.ReportProvenance || content.ID != child.ID || content.State != latest.State || content.Model != latest.Model || content.Report != latest.Text || content.Error != latest.Error || content.Truncated != latest.Truncated {
		return fmt.Errorf("native completion does not match exact latest child outcome")
	}
	return checkReservation(child, receipt, false)
}

type nativeNotice struct {
	Nonce    string         `json:"nonce"`
	Consumed bool           `json:"consumed"`
	Outcome  *nativeOutcome `json:"outcome"`
}

func (notice *nativeNotice) UnmarshalJSON(data []byte) error {
	var view struct {
		Nonce    string
		Consumed *bool
		Outcome  *nativeOutcome
	}
	if err := json.Unmarshal(data, &view); err != nil {
		return err
	}
	if view.Nonce == "" || view.Consumed == nil || view.Outcome == nil {
		return fmt.Errorf("retained reservation lacks nonce, consumption, or outcome")
	}
	notice.Nonce, notice.Consumed, notice.Outcome = view.Nonce, *view.Consumed, view.Outcome
	return nil
}

func sameOutcome(left, right *nativeOutcome) bool {
	if left == nil || right == nil {
		return false
	}
	a, b := *left, *right
	a.Raw, b.Raw = nil, nil
	a.Usage, b.Usage = nil, nil
	if !reflect.DeepEqual(a, b) {
		return false
	}
	if !equalJSON(left.Usage, right.Usage) {
		return false
	}
	if len(left.Raw) > 0 && len(right.Raw) > 0 {
		return equalJSON(left.Raw, right.Raw)
	}
	return true
}

func equalJSON(left, right []byte) bool {
	values := [2]any{}
	for index, raw := range [][]byte{left, right} {
		if len(raw) == 0 {
			raw = []byte("null")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&values[index]) != nil {
			return false
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return false
		}
	}
	return equalJSONValue(values[0], values[1])
}

func equalJSONValue(left, right any) bool {
	switch left := left.(type) {
	case json.Number:
		right, ok := right.(json.Number)
		if !ok {
			return false
		}
		a, ok := new(big.Rat).SetString(string(left))
		if !ok {
			return false
		}
		b, ok := new(big.Rat).SetString(string(right))
		return ok && a.Cmp(b) == 0
	case map[string]any:
		right, ok := right.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, exists := right[key]
			if !exists || !equalJSONValue(value, other) {
				return false
			}
		}
		return true
	case []any:
		right, ok := right.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for index, value := range left {
			if !equalJSONValue(value, right[index]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}

func pendingReceipt(child nativeChild) (string, error) {
	if err := checkReservation(child, child.NoticeID, true); err != nil {
		return "", err
	}
	return child.NoticeID, nil
}

func checkReservation(child nativeChild, receipt string, pending bool) error {
	if receipt == "" || child.NoticeID != receipt || child.Latest == nil || child.Latest.Run.ID != child.ID {
		return fmt.Errorf("canonical retained receipt identity missing or mismatched")
	}
	matches := 0
	for _, raw := range child.Notices {
		var notice nativeNotice
		if err := json.Unmarshal(raw, &notice); err != nil {
			return err
		}
		same := sameOutcome(notice.Outcome, child.Latest)
		if notice.Nonce == receipt {
			matches++
			if !same || notice.Consumed == pending {
				return fmt.Errorf("canonical reservation outcome or consumption differs")
			}
		} else if !notice.Consumed && same {
			return fmt.Errorf("stray unconsumed reservation for canonical latest outcome")
		}
	}
	if matches > 1 || (pending && matches != 1) {
		return fmt.Errorf("canonical reservation missing or duplicated")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
