package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type paneCommand struct {
	Generation string          `json:"generation"`
	Directory  string          `json:"directory"`
	Owner      observedProcess `json:"owner"`
	Args       []string        `json:"args"`
}

type paneCompletion struct {
	ClosingID  string          `json:"closingID,omitempty"`
	Generation string          `json:"generation"`
	Process    observedProcess `json:"process"`
	StartedAt  string          `json:"startedAt"`
	ExitedAt   string          `json:"exitedAt,omitempty"`
	Status     int             `json:"status"`
}

type captureInterval struct {
	Generation string          `json:"generation"`
	Pane       observedProcess `json:"pane"`
	Pipe       observedProcess `json:"pipe"`
	Completion paneCompletion  `json:"completion"`
	ReadyAt    string          `json:"readyAt"`
	ClosedAt   string          `json:"closedAt,omitempty"`
	BytesRead  int64           `json:"bytesRead"`
	BytesSaved int64           `json:"bytesSaved"`
	EOF        bool            `json:"eof"`
	Joined     bool            `json:"joined"`
	Boundary   bool            `json:"boundary"`
	Overflow   bool            `json:"overflow"`
	Hostile    bool            `json:"hostile"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
}

type captureEvidence struct {
	Status      string            `json:"status"`
	Saved       int64             `json:"saved"`
	RawIdentity string            `json:"rawIdentity"`
	SHA256      string            `json:"sha256"`
	Intervals   []captureInterval `json:"intervals"`
}

type paneCapture struct {
	command    paneCommand
	interval   captureInterval
	connection net.Conn
	ready      chan struct{}
	boundary   chan struct{}
	result     chan captureInterval
	closing    atomic.Bool
}

type ptyCapture struct {
	directory string
	evidence  string
	binary    string
	target    string
	tmux      func(...string) (string, error)
	raw       *os.File
	rawInfo   os.FileInfo
	limit     int64
	saved     int64
	intervals []captureInterval
	active    *paneCapture
}

func newPTYCapture(directory, evidence, binary, target string, tmux func(...string) (string, error), limit int64) (*ptyCapture, error) {
	path := filepath.Join(evidence, "pty.raw")
	raw, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := raw.Stat()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := writePrivate(path+".stderr", nil); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return &ptyCapture{directory: directory, evidence: evidence, binary: binary, target: target, tmux: tmux, raw: raw, rawInfo: info, limit: limit}, nil
}

func (c *ptyCapture) start(args []string) (observedProcess, error) {
	if c.active != nil {
		return observedProcess{}, errors.New("inconclusive capture: previous pane generation has not joined")
	}
	occupied, err := c.tmux("display-message", "-p", "-t", c.target, "#{pane_pipe}")
	if err != nil || occupied != "0" {
		return observedProcess{}, fmt.Errorf("inconclusive capture: pane pipe is occupied or unavailable: %q (%v)", occupied, err)
	}
	directory, err := os.MkdirTemp(c.directory, "g-")
	if err != nil {
		return observedProcess{}, err
	}
	owner, err := readObservedProcess(os.Getpid())
	if err != nil {
		return observedProcess{}, err
	}
	command := paneCommand{Generation: rand.Text(), Directory: directory, Owner: owner, Args: args}
	if err := writeCaptureJSON(filepath.Join(directory, "command.json"), command); err != nil {
		return observedProcess{}, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "p"), Net: "unix"})
	if err != nil {
		return observedProcess{}, err
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return observedProcess{}, err
	}
	if _, err := c.tmux("respawn-pane", "-k", "-t", c.target, "exec "+joinShellCommand(c.binary, []string{"--capture-pane", filepath.Join(directory, "command.json")})); err != nil {
		return observedProcess{}, err
	}
	panePID, err := c.tmux("display-message", "-p", "-t", c.target, "#{pane_pid}")
	if err != nil {
		return observedProcess{}, err
	}
	pid, err := strconv.Atoi(panePID)
	if err != nil {
		return observedProcess{}, err
	}
	pipeCommand := "exec " + joinShellCommand(c.binary, []string{"--capture-pipe", listener.Addr().String(), command.Generation}) + " 2>> " + shellQuote(filepath.Join(c.evidence, "pty.raw.stderr"))
	if _, err := c.tmux("pipe-pane", "-O", "-t", c.target, pipeCommand); err != nil {
		return observedProcess{}, err
	}
	connection, err := listener.AcceptUnix()
	if err != nil {
		return observedProcess{}, fmt.Errorf("inconclusive capture: recorder did not acknowledge readiness: %w", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	line, err := reader.ReadString('\n')
	var hello struct {
		Generation string          `json:"generation"`
		Process    observedProcess `json:"process"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(line), &hello)
	}
	if err != nil || hello.Generation != command.Generation {
		_ = connection.Close()
		return observedProcess{}, errors.New("inconclusive capture: recorder identity did not match generation")
	}
	_ = connection.SetReadDeadline(time.Time{})
	pane, err := readObservedProcess(pid)
	if err != nil {
		_ = connection.Close()
		return observedProcess{}, err
	}
	active := &paneCapture{command: command, interval: captureInterval{Generation: command.Generation, Pane: pane, Pipe: hello.Process, ReadyAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "inconclusive"}, connection: connection, ready: make(chan struct{}), boundary: make(chan struct{}), result: make(chan captureInterval, 1)}
	c.active = active
	go c.record(active, reader)
	<-active.ready
	if err := c.checkActive(); err != nil {
		return pane, err
	}
	if _, err := io.WriteString(connection, "ready\n"); err != nil {
		return pane, err
	}
	if err := writePrivate(filepath.Join(directory, "start"), []byte(command.Generation)); err != nil {
		return pane, err
	}
	return pane, nil
}

func (c *ptyCapture) checkActive() error {
	active := c.active
	if active == nil {
		return errors.New("inconclusive capture: no owned pane generation")
	}
	value, err := c.tmux("display-message", "-p", "-t", c.target, "#{pane_pid} #{pane_pipe} #{pane_pipe_pid}")
	want := fmt.Sprintf("%d 1 %d", active.interval.Pane.PID, active.interval.Pipe.PID)
	if err != nil || value != want || !observedAlive(active.interval.Pipe) || !observedAlive(active.interval.Pane) {
		return fmt.Errorf("inconclusive capture: pane or recording pipe was detached, replaced, or interrupted: %q", value)
	}
	return nil
}

func captureBoundary(identity string) []byte {
	return []byte("\x1eVERIFY_CAPTURE_END_" + identity + "\x1f")
}

func (c *ptyCapture) record(active *paneCapture, reader io.Reader) {
	result := active.interval
	close(active.ready)
	buffer := make([]byte, 32<<10)
	var tail []byte
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			result.BytesRead += int64(n)
			scan := append(tail, chunk...)
			result.Hostile = result.Hostile || hostilePTY(scan)
			if !result.Boundary && bytes.Contains(scan, []byte("\x1eVERIFY_CAPTURE_END_")) {
				completion, completionErr := readPaneCompletion(active.command)
				if completionErr != nil && !os.IsNotExist(completionErr) {
					result.Error = completionErr.Error()
				}
				if completionErr == nil && bytes.Contains(scan, captureBoundary(completion.ClosingID)) {
					result.Completion = completion
					result.Boundary = true
					close(active.boundary)
				}
			}
			tail = append([]byte(nil), scan[max(0, len(scan)-256):]...)
			remaining := c.limit - c.saved
			keep := min(int64(n), max(int64(0), remaining))
			if keep != int64(n) {
				result.Overflow = true
			}
			if keep > 0 {
				written, writeErr := c.raw.Write(chunk[:keep])
				c.saved += int64(written)
				result.BytesSaved += int64(written)
				if writeErr != nil {
					result.Error = writeErr.Error()
				}
			}
		}
		if err != nil {
			result.EOF = errors.Is(err, io.EOF)
			if !result.EOF {
				result.Error = err.Error()
			} else if !active.closing.Load() {
				result.Error = "recorder reached EOF before the owned close transition"
			}
			result.ClosedAt = time.Now().UTC().Format(time.RFC3339Nano)
			active.result <- result
			return
		}
	}
}

func readPaneCompletion(command paneCommand) (paneCompletion, error) {
	data, err := os.ReadFile(filepath.Join(command.Directory, "exit.json"))
	if err != nil {
		return paneCompletion{}, err
	}
	var completion paneCompletion
	if err := json.Unmarshal(data, &completion); err != nil {
		return paneCompletion{}, err
	}
	data, err = os.ReadFile(filepath.Join(command.Directory, "started.json"))
	if err != nil {
		return paneCompletion{}, err
	}
	var started paneCompletion
	if err := json.Unmarshal(data, &started); err != nil {
		return paneCompletion{}, err
	}
	if completion.Generation != command.Generation || completion.Process != started.Process || completion.Process.PID <= 0 || completion.Process.StartTicks == "" || completion.StartedAt != started.StartedAt || completion.ExitedAt == "" || completion.ClosingID == "" || completion.ClosingID == command.Generation || observedAlive(completion.Process) {
		return paneCompletion{}, errors.New("inconclusive capture: private completion does not establish the owned program exit and closing identity")
	}
	return completion, nil
}

func (c *ptyCapture) finish(ctx context.Context) (paneCompletion, error) {
	active := c.active
	if active == nil {
		return paneCompletion{}, errors.New("inconclusive capture: missing owned interval")
	}
	var completion paneCompletion
	if err := waitFor(ctx, "owned program exit record", 10*time.Second, func() (bool, error) {
		if err := c.checkActive(); err != nil {
			return false, err
		}
		var err error
		completion, err = readPaneCompletion(active.command)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return completion, err
	}
	select {
	case <-active.boundary:
	case <-ctx.Done():
		return completion, fmt.Errorf("inconclusive capture: final output boundary missing: %w", ctx.Err())
	case <-time.After(5 * time.Second):
		return completion, errors.New("inconclusive capture: final output boundary missing")
	}
	if err := c.checkActive(); err != nil {
		return completion, err
	}
	active.closing.Store(true)
	if _, err := c.tmux("pipe-pane", "-t", c.target); err != nil {
		return completion, err
	}
	var interval captureInterval
	select {
	case interval = <-active.result:
		interval.Joined = true
	case <-ctx.Done():
		return completion, fmt.Errorf("inconclusive capture: recorder join failed: %w", ctx.Err())
	}
	_ = active.connection.Close()
	if !waitUntilDone(ctx, func() bool { return !observedAlive(interval.Pipe) }) {
		interval.Error = "owned pipe process did not exit"
	}
	if interval.Completion != completion {
		interval.Error = "recorder boundary does not match the private completion record"
	}
	if interval.EOF && interval.Boundary && interval.BytesRead > int64(len(captureBoundary(completion.ClosingID))) && interval.BytesSaved > 0 && !interval.Overflow && interval.Error == "" {
		interval.Status = "complete"
	}
	c.intervals = append(c.intervals, interval)
	c.active = nil
	persistErr := c.save()
	if err := writePrivate(filepath.Join(active.command.Directory, "release"), []byte(active.command.Generation)); err != nil {
		return completion, errors.Join(persistErr, err)
	}
	if interval.Status != "complete" {
		persistErr = errors.Join(persistErr, errors.New("inconclusive capture: incomplete interval; see pty.capture.json"))
	}
	if interval.Hostile {
		persistErr = errors.Join(persistErr, errors.New("attacker-controlled terminal sequence reached raw PTY"))
	}
	return completion, persistErr
}

func (c *ptyCapture) close(ctx context.Context) error {
	if active := c.active; active != nil {
		_ = active.connection.Close()
		select {
		case interval := <-active.result:
			interval.Joined = true
			interval.Status = "inconclusive"
			c.intervals = append(c.intervals, interval)
		case <-ctx.Done():
			return fmt.Errorf("inconclusive capture: recorder did not join: %w", ctx.Err())
		}
		c.active = nil
	}
	return errors.Join(c.save(), c.raw.Close())
}

func (c *ptyCapture) save() error {
	if err := c.raw.Sync(); err != nil {
		return err
	}
	path := filepath.Join(c.evidence, "pty.raw")
	info, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, c.rawInfo) || info.Size() != c.saved {
		return errors.New("inconclusive capture: raw file was replaced or truncated")
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return err
	}
	evidence := captureEvidence{Status: "complete", Saved: c.saved, RawIdentity: statIdentity(c.rawInfo), SHA256: digest, Intervals: c.intervals}
	if len(c.intervals) == 0 || c.active != nil {
		evidence.Status = "inconclusive"
	}
	for _, interval := range c.intervals {
		if interval.Status != "complete" {
			evidence.Status = "inconclusive"
		}
	}
	return writeCaptureJSON(filepath.Join(c.evidence, "pty.capture.json"), evidence)
}

func writeCaptureJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func runCapturePane(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var command paneCommand
	if err := json.Unmarshal(data, &command); err != nil {
		return err
	}
	if command.Generation == "" || len(command.Args) == 0 || command.Directory != filepath.Dir(path) {
		return errors.New("invalid capture pane command")
	}
	waitGate := func(name string) error {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		return waitFor(ctx, "capture "+name, runTimeout, func() (bool, error) {
			if !observedAlive(command.Owner) {
				return false, errors.New("capture owner exited")
			}
			data, err := os.ReadFile(filepath.Join(command.Directory, name))
			if os.IsNotExist(err) {
				return false, nil
			}
			return err == nil && string(data) == command.Generation, err
		})
	}
	if err := waitGate("start"); err != nil {
		return err
	}
	child := exec.Command(command.Args[0], command.Args[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	process, err := readObservedProcess(child.Process.Pid)
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return err
	}
	completion := paneCompletion{Generation: command.Generation, Process: process, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeCaptureJSON(filepath.Join(command.Directory, "started.json"), completion); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return err
	}
	waitErr := child.Wait()
	completion.ExitedAt = time.Now().UTC().Format(time.RFC3339Nano)
	completion.ClosingID = rand.Text()
	completion.Status = child.ProcessState.ExitCode()
	if err := writeCaptureJSON(filepath.Join(command.Directory, "exit.json"), completion); err != nil {
		return err
	}
	if _, err := os.Stdout.Write(captureBoundary(completion.ClosingID)); err != nil {
		return err
	}
	if err := waitGate("release"); err != nil {
		return err
	}
	return waitErr
}

func runCapturePipe(socket, generation string) error {
	connection, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	process, err := readObservedProcess(os.Getpid())
	if err != nil {
		return err
	}
	if err := json.NewEncoder(connection).Encode(struct {
		Generation string          `json:"generation"`
		Process    observedProcess `json:"process"`
	}{generation, process}); err != nil {
		return err
	}
	ack, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil || strings.TrimSpace(ack) != "ready" {
		return errors.New("capture recorder did not acknowledge readiness")
	}
	_, err = io.Copy(connection, os.Stdin)
	return err
}
