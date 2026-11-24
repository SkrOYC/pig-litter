//go:build p08native

package piglitter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/cli"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/SkrOYC/pig-litter/extensions/pig-litter/memberinput"
)

type nativeMember struct {
	manifest     nativeMemberManifest
	client       *http.Client
	transport    *http.Transport
	admission    *coding.InputAdmission
	bridge       *memberinput.Bridge
	mu           sync.Mutex
	window       memberinput.WindowID
	windowSeq    uint64
	expectWindow uint64
	windowState  *nativeWindowState
	windowChange chan struct{}
	windows      map[memberinput.WindowID]context.Context
	started      bool
	ready        bool
	turn         uint64
	boundary     uint64
	approved     uint64
	continuation chan struct{}
	pendingTools []string
	control      context.CancelFunc
	calls        map[string]struct{}
	causes       map[string]context.Context
}

type nativeWindowState struct {
	sequence uint64
	ready    chan struct{}
	err      error
}

type nativeResponseError struct {
	path   string
	status string
}

func NativeMemberAvailable() bool { return true }

func (err *nativeResponseError) Error() string {
	return fmt.Sprintf("native member %s: %s", err.path, err.status)
}

func RunNativeMember(path string) error {
	manifest, err := readNativeMemberManifest(path)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != manifest.Cwd {
		return errors.New("native member started in the wrong working directory")
	}
	if err := os.Setenv("PIG_CODING_AGENT_DIR", manifest.AgentDirectory); err != nil {
		return err
	}
	if manifest.PigHome != "" {
		if err := os.Setenv("PIG_HOME", manifest.PigHome); err != nil {
			return err
		}
	}
	if manifest.UsePiDirs {
		if err := os.Setenv("PIG_USE_PI_DIRS", "1"); err != nil {
			return err
		}
	} else if err := os.Unsetenv("PIG_USE_PI_DIRS"); err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", manifest.Socket)
	}}
	member := &nativeMember{manifest: manifest, transport: transport, client: &http.Client{Transport: transport}, bridge: memberinput.New(), calls: map[string]struct{}{}, causes: map[string]context.Context{}, windows: map[memberinput.WindowID]context.Context{}, windowChange: make(chan struct{})}
	defer transport.CloseIdleConnections()
	defer member.bridge.Close()
	member.admission, err = coding.NewInputAdmission(context.Background(), coding.InputAdmissionOptions{SessionID: string(manifest.Session), PendingLimit: manifest.MailboxLimit, OnWindow: member.publishWindow})
	if err != nil {
		return err
	}
	defer member.admission.Close()
	parts := strings.SplitN(manifest.Model, "/", 2)
	args := []string{"--session-id", string(manifest.Session)}
	if manifest.ResumeFile != "" {
		args = []string{"--session", manifest.ResumeFile}
	}
	args = append(args, "--session-dir", manifest.SessionDirectory, "--model", manifest.Model, "--tools", strings.Join(manifest.Tools, ","), "--no-extensions")
	if slices.ContainsFunc(manifest.Tools, isMCPTool) && !slices.ContainsFunc(manifest.Tools, func(name string) bool { return strings.HasPrefix(name, "mcp__") }) {
		excluded := []string{"mcp__*"}
		for _, name := range []string{"list_mcp_resources", "list_mcp_resource_templates", "read_mcp_resource"} {
			if !slices.Contains(manifest.Tools, name) {
				excluded = append(excluded, name)
			}
		}
		args = append(args, "--exclude-tools", strings.Join(excluded, ","))
	}
	for _, builtin := range []struct{ tool, extension string }{{"codemode", "builtin:codemode"}, {"tool_search", "builtin:tool-search"}} {
		if slices.Contains(manifest.Tools, builtin.tool) {
			args = append(args, "-e", builtin.extension)
		}
	}
	if manifest.Instructions != "" {
		args = append(args, "--append-system-prompt", manifest.Instructions)
	}
	if manifest.ProjectTrusted {
		args = append(args, "--approve")
	} else {
		args = append(args, "--no-approve")
	}
	if !manifest.PeerWake {
		args = append(args, "--", manifest.Prompt)
	}
	cli.Main(args, &cli.MainOptions{InputAdmission: member.admission, HostPolicy: &cli.HostPolicy{FixedModel: coding.FixedModel{Provider: parts[0], ID: parts[1]}, RequiredFactory: member.factory}})
	return nil
}

func (member *nativeMember) call(ctx context.Context, path string, value, result any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://member"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("X-Pig-Litter-Member", member.manifest.Capability)
	cause := rand.Text()
	member.mu.Lock()
	member.causes[cause] = ctx
	member.mu.Unlock()
	defer func() {
		member.mu.Lock()
		delete(member.causes, cause)
		member.mu.Unlock()
	}()
	request.Header.Set("X-Pig-Litter-Cause", cause)
	response, err := member.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &nativeResponseError{path: path, status: response.Status}
	}
	return json.NewDecoder(io.LimitReader(response.Body, memberControlLimit)).Decode(result)
}

func (member *nativeMember) retry(ctx context.Context, path string, value, result any) error {
	for {
		err := member.call(ctx, path, value, result)
		if err == nil {
			return nil
		}
		var status *nativeResponseError
		if errors.As(err, &status) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (member *nativeMember) publishWindow(ctx context.Context, value *coding.InputWindow) (err error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	window := memberinput.WindowID(hex.EncodeToString(nonce[:]))
	if err := member.bridge.Open(ctx, window, memberinput.NativeWindow{Value: value}); err != nil {
		return err
	}
	windowCtx, cancelWindow := context.WithCancel(context.Background())
	context.AfterFunc(ctx, cancelWindow)
	member.mu.Lock()
	first := !member.started
	member.started = true
	member.windowSeq++
	sequence := member.windowSeq
	state := &nativeWindowState{sequence: sequence, ready: make(chan struct{})}
	member.window = window
	member.windows[window] = windowCtx
	member.windowState = state
	close(member.windowChange)
	member.windowChange = make(chan struct{})
	member.mu.Unlock()
	defer func() {
		member.mu.Lock()
		state.err = err
		close(state.ready)
		member.mu.Unlock()
	}()
	if !first {
		var reply map[string]bool
		return member.retry(ctx, "/input/window", memberWindowPublication{Window: string(window), Sequence: sequence}, &reply)
	}
	var transfers []memberStartingTransfer
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := member.retry(startup, "/input/start", memberStartingWindow{Window: string(window)}, &transfers); err != nil {
		return err
	}
	ids := make([]string, 0, len(transfers))
	for _, transfer := range transfers {
		input := memberinput.Input{Mode: memberinput.Mode(transfer.Mode), Text: transfer.Text}
		receipt, err := member.bridge.Admit(ctx, window, memberinput.TransferID(transfer.ID), input)
		if err != nil {
			return err
		}
		ids = append(ids, transfer.ID)
		go member.completeStarting(window, transfer.ID, receipt)
	}
	var result map[string]bool
	return member.retry(startup, "/input/installed", memberStartingInstallation{Window: string(window), Transfers: ids}, &result)
}

func (member *nativeMember) waitWindow(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		member.mu.Lock()
		state := member.windowState
		if state == nil || state.sequence < member.expectWindow {
			changed := member.windowChange
			member.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-wait.Done():
				return wait.Err()
			}
		}
		ready := state.ready
		member.mu.Unlock()
		select {
		case <-ready:
		case <-wait.Done():
			return wait.Err()
		}
		member.mu.Lock()
		if member.windowState != state {
			member.mu.Unlock()
			continue
		}
		err := state.err
		if err == nil && state.sequence >= member.expectWindow {
			member.expectWindow = 0
		}
		member.mu.Unlock()
		return err
	}
}

func (member *nativeMember) completeStarting(window memberinput.WindowID, id string, receipt *memberinput.Receipt) {
	defer receipt.Close()
	result, err := receipt.Await(context.Background())
	if err != nil {
		return
	}
	request := memberStartingResult{Window: string(window), Transfer: id, Disposition: string(result.Disposition), Error: result.Error}
	resultCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reply map[string]bool
	_ = member.retry(resultCtx, "/input/result", request, &reply)
}

func (member *nativeMember) stop(ctx context.Context) {
	_ = member.admission.Close()
	if ext := extension.FromContext(ctx); ext != nil {
		go func() {
			_ = ext.Abort()
			_ = ext.Shutdown()
		}()
	}
}

func (member *nativeMember) currentTurn() uint64 {
	member.mu.Lock()
	defer member.mu.Unlock()
	return member.turn
}

func nativeModelName(model *ai.Model) string {
	if model == nil {
		return ""
	}
	return model.ProviderID() + "/" + model.ID
}
