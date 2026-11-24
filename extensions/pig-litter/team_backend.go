package piglitter

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (reader contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.r.Read(data)
}

type backendRequest string

type backendChoice struct {
	kind     backendRequest
	fallback string
}

type memberInput interface{ memberInput() }

type memberTextInput struct{ mode, text string }

type memberCompletionInput struct {
	details completionDetails
	content string
	mode    string
	trigger bool
}

func (memberTextInput) memberInput()       {}
func (memberCompletionInput) memberInput() {}

type memberInputDisposition uint8

const (
	memberInputQueued memberInputDisposition = iota + 1
	memberInputHandled
	memberInputUncertain
)

type memberPhase uint8

const (
	memberStarting memberPhase = iota + 1
	memberStreaming
	memberSettling
	memberIdle
	memberClosed
)

type memberHistoryBound struct{ entries, bytes int }

type memberSnapshot struct {
	session ChildID
	cwd     string
	model   string
	tools   []string
	phase   memberPhase
	pending int
	entries []json.RawMessage
	leaf    *string
	clipped bool
}

type memberControl interface {
	Queue(context.Context, memberInput) (memberInputDisposition, error)
	Snapshot(context.Context, memberHistoryBound) (memberSnapshot, error)
	RecordCompletion(context.Context, completionAudit) error
	Cancel()
	Focus(context.Context) error
}

type memberLogPin struct {
	path, cwd     string
	session       ChildID
	file          os.FileInfo
	header        json.RawMessage
	baselineCount int
	baselineHash  [32]byte
	baselineSet   bool
}

type memberLogSnapshot struct {
	header  json.RawMessage
	entries []json.RawMessage
}

type memberSession struct {
	File   string   `json:"file"`
	Model  string   `json:"model"`
	Tools  []string `json:"tools"`
	Launch string   `json:"launch"`
}

type memberResumePin struct {
	log    memberLogPin
	count  int
	digest [32]byte
	leaf   *string
}

type memberServerIdentity struct {
	Socket string `json:"socket"`
	PID    int    `json:"pid"`
	Birth  string `json:"birth"`
	Target string `json:"target"`
	Window string `json:"window,omitempty"`
	TTY    string `json:"tty,omitempty"`
}

type memberPaneIdentity struct {
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	Birth   string `json:"birth"`
	Command string `json:"command"`
	TTY     string `json:"tty,omitempty"`
}

type memberOwnership struct {
	Backend          backendRequest       `json:"backend"`
	Member           MemberRef            `json:"member"`
	Admission        string               `json:"admission"`
	Execution        string               `json:"execution"`
	OwnerEpoch       uint64               `json:"ownerEpoch"`
	ProcessScope     string               `json:"processScope,omitempty"`
	Launch           string               `json:"launch"`
	LaunchPhase      string               `json:"launchPhase,omitempty"`
	Cwd              string               `json:"cwd"`
	Model            string               `json:"model"`
	Tools            []string             `json:"tools"`
	SessionDirectory string               `json:"sessionDirectory"`
	Executable       string               `json:"executable"`
	ExecutableSHA256 string               `json:"executableSha256"`
	Manifest         string               `json:"manifest"`
	Server           memberServerIdentity `json:"server"`
	Pane             *memberPaneIdentity  `json:"pane,omitempty"`
}

type memberBinding struct {
	record      *record
	run         *run
	member      MemberRef
	admission   string
	execution   string
	ownerEpoch  uint64
	launch      string
	log         memberLogPin
	ownership   *memberOwnership
	owned       *memberOwnership
	control     memberControl
	ready       bool
	readyMu     sync.Mutex
	readyHash   [32]byte
	readyDone   chan struct{}
	readyErr    error
	pinMu       sync.Mutex
	startingLog *memberStartingLog
	denialMu    sync.Mutex
	denials     map[memberPlanDenial]string
	settleMu    sync.Mutex
	boundary    uint64
	evaluating  bool
	settlement  *memberSettlement
}

type memberPlanDenial struct {
	response string
	call     planDenial
}

const (
	backendEmbedded backendRequest = "embedded"
	backendAuto     backendRequest = "auto"
	backendTmux     backendRequest = "tmux"
	backendITerm    backendRequest = "iterm2"
)

func isPaneBackend(request backendRequest) bool {
	return request == backendTmux || request == backendITerm
}

func chooseTeamBackend(request backendRequest, probe func() error) (backendChoice, error) {
	switch request {
	case "", backendEmbedded:
		return backendChoice{kind: backendEmbedded}, nil
	case backendAuto, backendTmux, backendITerm:
		if err := probe(); err != nil {
			if request == backendAuto {
				return backendChoice{kind: backendEmbedded, fallback: "tmux unavailable: " + err.Error()}, nil
			}
			return backendChoice{}, reject("unavailable", string(request)+" teammate backend is unavailable: "+err.Error()+"; choose embedded or auto")
		}
		if request == backendAuto {
			request = backendTmux
		}
		return backendChoice{kind: request}, nil
	default:
		return backendChoice{}, reject("rejected", "backend must be embedded, tmux, iterm2, or auto")
	}
}

func InspectNativeProcessPath(pid int, birth string) (string, error) {
	return memberProcessPath(pid, birth, "")
}
