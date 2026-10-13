package piglitter

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/MichaelKinsy/PiG/coding"
)

type ChildID string

type Ref struct {
	ID ChildID `json:"id"`
}

type State string

const (
	Starting    State = "starting"
	Running     State = "running"
	Stopping    State = "stopping"
	Pausing     State = "pausing"
	Paused      State = "paused"
	Completed   State = "completed"
	Partial     State = "partial"
	Failed      State = "failed"
	Stopped     State = "stopped"
	Unavailable State = "unavailable"
)

func (s State) terminal() bool {
	switch s {
	case Completed, Partial, Failed, Stopped, Paused, Unavailable:
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
	ReportProvenance  string `json:"reportProvenance,omitempty"`
}

type Snapshot struct {
	Ref
	Parent       *ChildID `json:"parent"`
	Depth        int      `json:"depth"`
	Name         string   `json:"name"`
	DisplayLabel string   `json:"displayLabel"`
	Type         string   `json:"type"`
	Model        string   `json:"model"`
	Cwd          string   `json:"cwd"`
	State        State    `json:"state"`
}

type record struct {
	id                                   ChildID
	parent                               *record
	depth                                int
	name, kind, model, instructions, cwd string
	pendingTask                          string
	tools                                []string
	canDelegate, readOnly                bool
	history                              *coding.SessionManager
	historyLost                          bool
	run                                  *run
	previous                             *run
	paused                               bool
	latest                               *Outcome
	deliveryMu                           sync.Mutex
	receiptMu                            sync.Mutex
	receipts                             map[*run]*completionReceipt
	notices                              map[string]*reservation
}

type message struct {
	mode, text string
	result     chan error
}

type run struct {
	ref             Ref
	noticeID        string
	owner           *record
	deliveryReceipt *completionReceipt
	state           State
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	started         bool
	outcome         *Outcome
	prompt          string
	resumePaused    bool
	mailbox         chan message
	mu              sync.Mutex
	calls           *callScope
	messageMu       sync.Mutex
	pendingMessages int
	session         *coding.Session
	disposing       bool
	ready           chan struct{}
	deliveryError   string
	reservation     *reservation
}

type Inspection struct {
	Child   Snapshot `json:"child"`
	Outcome *Outcome `json:"outcome,omitempty"`
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

func (r Ref) String() string { return string(r.ID) }
