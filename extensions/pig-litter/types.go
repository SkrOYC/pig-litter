package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/MichaelKinsy/PiG/coding"
)

type ChildID string

type Ref struct {
	ID         ChildID `json:"id"`
	Generation int     `json:"generation"`
}

type State string

const (
	Starting    State = "starting"
	Running     State = "running"
	Stopping    State = "stopping"
	Completed   State = "completed"
	Partial     State = "partial"
	Failed      State = "failed"
	Stopped     State = "stopped"
	Unavailable State = "unavailable"
)

func (s State) terminal() bool {
	switch s {
	case Completed, Partial, Failed, Stopped, Unavailable:
		return true
	}
	return false
}

type treeError struct{ code, message string }

func (e *treeError) Error() string      { return e.message }
func reject(code, message string) error { return &treeError{code, message} }

type Usage struct {
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheRead   int     `json:"cacheRead"`
	CacheWrite  int     `json:"cacheWrite"`
	TotalTokens int     `json:"totalTokens"`
	Cost        float64 `json:"cost"`
}

type Outcome struct {
	Run               Ref    `json:"run"`
	State             State  `json:"state"`
	Model             string `json:"model"`
	Text              string `json:"text"`
	Error             string `json:"error,omitempty"`
	Truncated         bool   `json:"truncated"`
	HandbackTruncated bool   `json:"handbackTruncated,omitempty"`
	Usage             Usage  `json:"usage"`
}

type Snapshot struct {
	Ref
	Parent *ChildID `json:"parent"`
	Depth  int      `json:"depth"`
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Model  string   `json:"model"`
	State  State    `json:"state"`
}

type record struct {
	id                                         ChildID
	parent                                     ChildID
	parentGeneration                           int
	parentRun                                  *run
	depth                                      int
	name, kind, model, instructions, task, cwd string
	tools                                      []string
	canDelegate, readOnly                      bool
	history                                    *coding.SessionManager
	historyLost                                bool
	authority                                  *authority
	run                                        *run
	previous                                   *run
}

type message struct {
	mode, text string
	result     chan error
}

type run struct {
	ref                 Ref
	state               State
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	started             bool
	deadline            bool
	outcome             *Outcome
	prompt              string
	mailbox             chan message
	mu                  sync.Mutex
	messageMu           sync.Mutex
	pendingMessages     int
	session             *coding.Session
	disposing           bool
	ready               chan struct{}
	receipts            map[Ref]*completionReceipt
	completionSendError string
}

type Inspection struct {
	Child            Snapshot          `json:"child"`
	Outcome          *Outcome          `json:"outcome,omitempty"`
	HistoryAvailable bool              `json:"historyAvailable"`
	Mailbox          []Outcome         `json:"mailbox"`
	MailboxTotal     int               `json:"mailboxTotal"`
	MailboxMore      bool              `json:"mailboxMore"`
	Entries          []json.RawMessage `json:"entries"`
	NextOffset       int               `json:"nextOffset"`
	More             bool              `json:"more"`
}

func bounded(text string, maximum int) (string, bool) {
	if len(text) <= maximum {
		return text, false
	}
	end := maximum
	for end > 0 && text[end]&0xc0 == 0x80 {
		end--
	}
	return text[:end], true
}

func historyBytes(manager *coding.SessionManager) int {
	if manager == nil {
		return 0
	}
	data, err := json.Marshal(manager.Entries())
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(data)
}

func previewOutcome(outcome *Outcome, maximum int) *Outcome {
	if outcome == nil {
		return nil
	}
	copy := *outcome
	var clipped bool
	copy.Text, clipped = bounded(copy.Text, maximum)
	copy.HandbackTruncated = clipped
	copy.Error, clipped = bounded(copy.Error, 256)
	copy.HandbackTruncated = copy.HandbackTruncated || clipped
	return &copy
}

func (r Ref) String() string { return fmt.Sprintf("%s:%d", r.ID, r.Generation) }
