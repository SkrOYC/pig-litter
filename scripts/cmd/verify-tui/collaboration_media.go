package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type collaborationMediaArtifact struct {
	Path              string `json:"path"`
	Origin            string `json:"origin"`
	SHA256            string `json:"sha256,omitempty"`
	SourceVideoSHA256 string `json:"source_video_sha256,omitempty"`
	SourceTimestampMS *int64 `json:"source_timestamp_ms,omitempty"`
	Width             int    `json:"width,omitempty"`
	Height            int    `json:"height,omitempty"`
	Frames            int    `json:"frames,omitempty"`
	Duration          string `json:"duration_seconds,omitempty"`
}

type collaborationMediaProcess struct {
	Command    []string        `json:"command"`
	Identity   observedProcess `json:"identity"`
	StartedNS  int64           `json:"started_offset_ns"`
	ExitedNS   int64           `json:"exited_offset_ns,omitempty"`
	ExitStatus int             `json:"exit_status"`
	Joined     bool            `json:"joined"`
}

type collaborationMediaSample struct {
	OffsetNS int64  `json:"offset_ns"`
	Pane     string `json:"pane"`
	Clients  string `json:"clients"`
	Error    string `json:"error,omitempty"`
}

type collaborationObserverIdentity struct {
	Observer observedProcess `json:"observer"`
	Client   observedProcess `json:"client"`
}

type collaborationStartupMeasurement struct {
	Clock        string          `json:"clock"`
	Process      observedProcess `json:"process"`
	StartNS      int64           `json:"start_ns"`
	ReadyNS      int64           `json:"ready_ns"`
	DurationNS   int64           `json:"duration_ns"`
	MediaEnabled bool            `json:"media_enabled"`
	Readiness    string          `json:"readiness"`
}

type collaborationFixtureSession struct {
	ID     string        `json:"id"`
	File   ownedResource `json:"file"`
	SHA256 string        `json:"sha256"`
}

type collaborationFixtureEvidence struct {
	Clock                  string                        `json:"clock"`
	BootID                 string                        `json:"boot_id"`
	TimeNamespace          string                        `json:"time_namespace"`
	ProviderPort           int                           `json:"provider_port"`
	ProviderStartedNS      int64                         `json:"provider_started_ns"`
	ProviderCloseStartedNS int64                         `json:"provider_close_started_ns"`
	Workspace              ownedResource                 `json:"workspace"`
	FixtureFile            ownedResource                 `json:"fixture_file"`
	RootSessions           []collaborationFixtureSession `json:"root_sessions,omitempty"`
}

func collaborationFileIdentity(path string) (ownedResource, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ownedResource{}, err
	}
	if !info.Mode().IsRegular() {
		return ownedResource{}, fmt.Errorf("fixture file is not regular: %s", path)
	}
	return ownedResource{Path: path, Identity: statIdentity(info)}, nil
}

func collaborationStartup(capture *ptyCapture, readyNS int64, mediaEnabled bool) (*collaborationStartupMeasurement, error) {
	if capture == nil || capture.active == nil {
		return nil, errors.New("startup measurement has no owned pane generation")
	}
	command := capture.active.command
	data, err := os.ReadFile(filepath.Join(command.Directory, "started.json"))
	if err != nil {
		return nil, fmt.Errorf("startup process receipt: %w", err)
	}
	var started paneCompletion
	if err := json.Unmarshal(data, &started); err != nil {
		return nil, err
	}
	if started.Generation != command.Generation || started.Process.PID <= 0 || started.Process.StartTicks == "" || !observedAlive(started.Process) || started.StartedMonotonicNS <= 0 || readyNS < started.StartedMonotonicNS {
		return nil, errors.New("startup process identity or monotonic interval is invalid")
	}
	return &collaborationStartupMeasurement{
		Clock: "CLOCK_MONOTONIC", Process: started.Process, StartNS: started.StartedMonotonicNS, ReadyNS: readyNS,
		DurationNS: readyNS - started.StartedMonotonicNS, MediaEnabled: mediaEnabled,
		Readiness: "startup frame, non-shell pane, and 100x32 geometry",
	}, nil
}

type collaborationMediaReceipt struct {
	Version         int                                   `json:"version"`
	Status          string                                `json:"status"`
	StartedAt       string                                `json:"started_at"`
	FinishedNS      int64                                 `json:"finished_offset_ns,omitempty"`
	RecordingNS     int64                                 `json:"recording_started_monotonic_ns,omitempty"`
	RecorderReadyNS int64                                 `json:"recorder_ready_monotonic_ns,omitempty"`
	StartupReadyNS  int64                                 `json:"startup_ready_monotonic_ns,omitempty"`
	Socket          string                                `json:"tmux_socket"`
	Session         string                                `json:"tmux_session"`
	Display         string                                `json:"display,omitempty"`
	Tools           map[string]string                     `json:"tools"`
	ToolSHA256      map[string]string                     `json:"tool_sha256"`
	Processes       map[string]*collaborationMediaProcess `json:"processes"`
	Observer        collaborationObserverIdentity         `json:"observer"`
	Terminal        observedProcess                       `json:"terminal"`
	PaneID          string                                `json:"pane_id,omitempty"`
	PaneBefore      string                                `json:"pane_before_attach,omitempty"`
	PaneAfter       string                                `json:"pane_after_attach,omitempty"`
	NoInputForward  bool                                  `json:"no_input_forwarding"`
	Samples         []collaborationMediaSample            `json:"samples,omitempty"`
	ProcessesExited bool                                  `json:"processes_exited"`
	Video           collaborationMediaArtifact            `json:"video"`
	Screenshot      collaborationMediaArtifact            `json:"screenshot"`
	Error           string                                `json:"error,omitempty"`
}

type collaborationMediaChild struct {
	command *exec.Cmd
	done    chan error
	record  *collaborationMediaProcess
	input   io.WriteCloser
}

type collaborationMedia struct {
	directory string
	options   options
	verifier  string
	tmux      string
	socket    string
	session   string
	env       []string
	started   time.Time
	receipt   collaborationMediaReceipt
	children  map[string]*collaborationMediaChild
	stop      context.CancelFunc
	monitored chan []collaborationMediaSample
}

func newCollaborationMedia(evidence string, options options, verifier, tmux, socket, session string, env []string) (*collaborationMedia, error) {
	directory := filepath.Join(evidence, "media")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, err
	}
	started := time.Now()
	stamp := int64(1000)
	media := &collaborationMedia{
		directory: directory, options: options, verifier: verifier, tmux: tmux, socket: socket, session: session, env: env, started: started,
		children: make(map[string]*collaborationMediaChild),
		receipt: collaborationMediaReceipt{
			Version: 1, Status: "inconclusive", StartedAt: started.UTC().Format(time.RFC3339Nano), Socket: socket, Session: session,
			Tools: map[string]string{}, ToolSHA256: map[string]string{}, Processes: map[string]*collaborationMediaProcess{},
			Video:      collaborationMediaArtifact{Path: "media/terminal.mp4", Origin: "live_terminal"},
			Screenshot: collaborationMediaArtifact{Path: "media/screenshot.png", Origin: "video_frame", SourceTimestampMS: &stamp},
		},
	}
	return media, media.save()
}

func (media *collaborationMedia) save() error {
	return writeCaptureJSON(filepath.Join(media.directory, "receipt.json"), media.receipt)
}

func (media *collaborationMedia) start(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return errors.New("live terminal media requires Linux with Xvfb and x11grab")
	}
	for name, program := range map[string]string{"Xvfb": media.options.mediaXvfb, "xterm": media.options.mediaXterm, "ffmpeg": media.options.mediaFFmpeg, "ffprobe": media.options.mediaFFprobe} {
		path, err := exec.LookPath(program)
		if err != nil {
			return fmt.Errorf("media tool %s is unavailable: %w", name, err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		media.receipt.Tools[name] = path
		media.receipt.ToolSHA256[name], err = fileSHA256(path)
		if err != nil {
			return err
		}
	}
	if err := media.save(); err != nil {
		return err
	}
	readDisplay, writeDisplay, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readDisplay.Close()
	xvfb := exec.Command(media.receipt.Tools["Xvfb"], "-displayfd", "3", "-screen", "0", "1024x768x24", "-nolisten", "tcp")
	xvfb.ExtraFiles = []*os.File{writeDisplay}
	_, err = media.startChild("xvfb", xvfb, false)
	_ = writeDisplay.Close()
	if err != nil {
		return err
	}
	displayResult := make(chan string, 1)
	go func() {
		data := make([]byte, 64)
		n, _ := readDisplay.Read(data)
		displayResult <- strings.TrimSpace(string(data[:n]))
	}()
	select {
	case display := <-displayResult:
		if _, err := strconv.Atoi(display); err != nil {
			return errors.New("Xvfb did not report a valid display")
		}
		media.receipt.Display = ":" + display
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(8 * time.Second):
		return errors.New("Xvfb did not report a display")
	}
	media.receipt.PaneBefore, err = media.tmuxCall(ctx, "display-message", "-p", "-t", media.session, "#{pane_id} #{pane_width}x#{pane_height} #{pane_pid}")
	if err != nil {
		return err
	}
	beforeFields := strings.Fields(media.receipt.PaneBefore)
	if len(beforeFields) != 3 || !strings.HasPrefix(beforeFields[0], "%") || beforeFields[1] != "100x32" {
		return errors.New("media observer has an invalid initial pane identity or geometry")
	}
	media.receipt.PaneID = beforeFields[0]
	observerPath := filepath.Join(media.directory, "observer.json")
	xterm := exec.Command(media.receipt.Tools["xterm"], "-geometry", "100x33+0+0", "-title", "PiG live terminal observer", "-e", media.verifier, "--media-observer", observerPath, media.tmux, media.socket, media.session)
	xterm.Env = append(withoutEnvironment(os.Environ(), "DISPLAY"), "DISPLAY="+media.receipt.Display)
	if _, err := media.startChild("xterm", xterm, false); err != nil {
		return err
	}
	if err := waitFor(ctx, "media observer attachment", 8*time.Second, func() (bool, error) {
		data, err := os.ReadFile(observerPath)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(data, &media.receipt.Observer); err != nil {
			return false, err
		}
		sample := media.sample(ctx)
		return sample.Error == "", nil
	}); err != nil {
		return err
	}
	media.receipt.Terminal, err = media.observerTerminal()
	if err != nil {
		return err
	}
	media.receipt.ToolSHA256["terminal"], err = fileSHA256(media.receipt.Terminal.Executable)
	if err != nil {
		return err
	}
	media.receipt.PaneAfter, err = media.tmuxCall(ctx, "display-message", "-p", "-t", media.session, "#{pane_id} #{pane_width}x#{pane_height} #{pane_pid}")
	if err != nil {
		return err
	}
	if media.receipt.PaneBefore != media.receipt.PaneAfter {
		return fmt.Errorf("media observer changed pane geometry or identity: %q to %q", media.receipt.PaneBefore, media.receipt.PaneAfter)
	}
	media.receipt.NoInputForward = true
	recorder := exec.Command(media.receipt.Tools["ffmpeg"], "-hide_banner", "-f", "x11grab", "-draw_mouse", "0", "-framerate", "15", "-video_size", "1024x768", "-i", media.receipt.Display, "-an", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-y", filepath.Join(media.directory, "terminal.mp4"))
	recorder.Env = append(withoutEnvironment(os.Environ(), "DISPLAY"), "DISPLAY="+media.receipt.Display)
	if _, err := media.startChild("ffmpeg", recorder, true); err != nil {
		return err
	}
	media.receipt.RecordingNS, err = collaborationMonotonicNow()
	if err != nil {
		return err
	}
	if err := waitFor(ctx, "media recorder readiness", 15*time.Second, func() (bool, error) {
		if !observedAlive(media.children["ffmpeg"].record.Identity) {
			return false, errors.New("media recorder exited before readiness")
		}
		data, err := os.ReadFile(filepath.Join(media.directory, "ffmpeg.log"))
		if err != nil {
			return false, err
		}
		return bytes.Contains(data, []byte("Press [q] to stop")) && bytes.Contains(data, []byte("frame=")), nil
	}); err != nil {
		return err
	}
	media.receipt.RecorderReadyNS, err = collaborationMonotonicNow()
	if err != nil {
		return err
	}
	monitorCtx, cancel := context.WithCancel(context.Background())
	media.stop = cancel
	media.monitored = make(chan []collaborationMediaSample, 1)
	go func() {
		var samples []collaborationMediaSample
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				media.monitored <- samples
				return
			case <-ticker.C:
				sample := media.sample(monitorCtx)
				if monitorCtx.Err() == nil {
					samples = append(samples, sample)
				}
			}
		}
	}()
	return media.save()
}

func (media *collaborationMedia) markStartupReady(readyNS int64) {
	media.receipt.StartupReadyNS = readyNS
	stamp := int64(1000)
	if readyNS > media.receipt.RecordingNS {
		stamp += (readyNS - media.receipt.RecordingNS) / int64(time.Millisecond)
	}
	media.receipt.Screenshot.SourceTimestampMS = &stamp
}

func withoutEnvironment(env []string, key string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return result
}

func (media *collaborationMedia) startChild(name string, command *exec.Cmd, input bool) (*collaborationMediaChild, error) {
	log, err := os.OpenFile(filepath.Join(media.directory, name+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	command.Stdout, command.Stderr = log, log
	child := &collaborationMediaChild{command: command, done: make(chan error, 1)}
	if input {
		child.input, err = command.StdinPipe()
		if err != nil {
			_ = log.Close()
			return nil, err
		}
	}
	if err := command.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	_ = log.Close()
	identity, err := readObservedProcess(command.Process.Pid)
	child.record = &collaborationMediaProcess{Command: append([]string(nil), command.Args...), Identity: identity, StartedNS: time.Since(media.started).Nanoseconds(), ExitStatus: -1}
	media.children[name] = child
	media.receipt.Processes[name] = child.record
	go func() { child.done <- command.Wait() }()
	if err != nil {
		return nil, fmt.Errorf("observe media process %s: %w", name, err)
	}
	return child, nil
}

func (media *collaborationMedia) tmuxCall(ctx context.Context, args ...string) (string, error) {
	return runTmux(ctx, media.tmux, media.socket, media.env, args...)
}

func (media *collaborationMedia) observerTerminal() (observedProcess, error) {
	observer := media.receipt.Observer.Observer
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(observer.PID), "stat"))
	if err != nil {
		return observedProcess{}, err
	}
	start, err := procStartTicks(string(data))
	if err != nil || start != observer.StartTicks {
		return observedProcess{}, errors.New("media observer identity changed before terminal inspection")
	}
	end := strings.LastIndexByte(string(data), ')')
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return observedProcess{}, errors.New("media observer has no recorded parent")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return observedProcess{}, err
	}
	processes, err := observeDescendants(media.children["xterm"].record.Identity.PID)
	if err != nil {
		return observedProcess{}, err
	}
	for _, process := range processes {
		if process.PID == parent {
			return process, nil
		}
	}
	return observedProcess{}, errors.New("media observer terminal is outside the launched process tree")
}

func (media *collaborationMedia) sample(ctx context.Context) collaborationMediaSample {
	sample := collaborationMediaSample{OffsetNS: time.Since(media.started).Nanoseconds()}
	var err error
	sample.Pane, err = media.tmuxCall(ctx, "display-message", "-p", "-t", media.session, "#{pane_id} #{pane_width}x#{pane_height} #{pane_pid}")
	if err == nil {
		sample.Clients, err = media.tmuxCall(ctx, "list-clients", "-F", "#{client_pid} #{client_flags}")
	}
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	if !strings.HasPrefix(sample.Pane, media.receipt.PaneID+" ") {
		sample.Error = "media observer no longer targets the original pane"
		return sample
	}
	found := false
	for _, line := range strings.Split(sample.Clients, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == strconv.Itoa(media.receipt.Observer.Client.PID) {
			found = strings.Contains(fields[1], "ignore-size") && !strings.Contains(fields[1], "read-only")
		}
	}
	if !found || !observedAlive(media.receipt.Observer.Observer) || !observedAlive(media.receipt.Observer.Client) {
		sample.Error = "observer client disappeared or did not have ignore-size without read-only"
	}
	if media.receipt.Terminal.PID > 0 && !observedAlive(media.receipt.Terminal) {
		sample.Error = "terminal process exited before recording finished"
	}
	for name, child := range media.children {
		if !observedAlive(child.record.Identity) {
			sample.Error = "media process exited before recording finished: " + name
			break
		}
	}
	return sample
}

func (media *collaborationMedia) finish(runErr error) error {
	var failures []error
	if media.stop != nil {
		media.stop()
		media.receipt.Samples = <-media.monitored
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		media.receipt.Samples = append(media.receipt.Samples, media.sample(ctx))
		cancel()
		for _, sample := range media.receipt.Samples {
			if sample.Error != "" {
				failures = append(failures, fmt.Errorf("media observation failed: %s", sample.Error))
				break
			}
		}
	}
	if recorder := media.children["ffmpeg"]; recorder != nil {
		if recorder.input != nil {
			_, _ = io.WriteString(recorder.input, "q\n")
			_ = recorder.input.Close()
		}
		if err := media.joinChild(recorder, 12*time.Second, true); err != nil {
			failures = append(failures, fmt.Errorf("media recorder: %w", err))
		}
	} else {
		failures = append(failures, errors.New("media recorder did not start"))
	}
	if xterm := media.children["xterm"]; xterm != nil {
		if media.receipt.Observer.Observer.PID > 0 {
			failures = append(failures, signalObserved(media.receipt.Observer.Observer, syscall.SIGTERM))
		} else {
			processes, _ := observeDescendants(xterm.record.Identity.PID)
			for i := len(processes) - 1; i >= 0; i-- {
				if processes[i].PID != xterm.record.Identity.PID {
					failures = append(failures, signalObserved(processes[i], syscall.SIGTERM))
				}
			}
		}
		if err := media.joinChild(xterm, 5*time.Second, false); err != nil {
			failures = append(failures, fmt.Errorf("media xterm: %w", err))
		}
	}
	observerExited := func() bool {
		return (media.receipt.Observer.Observer.PID == 0 || !observedAlive(media.receipt.Observer.Observer)) &&
			(media.receipt.Observer.Client.PID == 0 || !observedAlive(media.receipt.Observer.Client)) &&
			(media.receipt.Terminal.PID == 0 || !observedAlive(media.receipt.Terminal))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	exited := waitUntilDone(ctx, observerExited)
	cancel()
	if !exited {
		for _, process := range []observedProcess{media.receipt.Observer.Observer, media.receipt.Observer.Client, media.receipt.Terminal} {
			if process.PID > 0 && observedAlive(process) {
				failures = append(failures, fmt.Errorf("media observer process %d required forced cleanup", process.PID))
				failures = append(failures, signalObserved(process, syscall.SIGKILL))
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = waitUntilDone(ctx, observerExited)
		cancel()
	}
	if xvfb := media.children["xvfb"]; xvfb != nil {
		failures = append(failures, signalObserved(xvfb.record.Identity, syscall.SIGTERM))
		if err := media.joinChild(xvfb, 5*time.Second, false); err != nil {
			failures = append(failures, fmt.Errorf("media Xvfb: %w", err))
		}
	}
	media.receipt.ProcessesExited = true
	for _, child := range media.children {
		if !child.record.Joined || observedAlive(child.record.Identity) {
			media.receipt.ProcessesExited = false
		}
	}
	for _, process := range []observedProcess{media.receipt.Observer.Observer, media.receipt.Observer.Client, media.receipt.Terminal} {
		if process.PID > 0 && observedAlive(process) {
			media.receipt.ProcessesExited = false
		}
	}
	if !media.receipt.ProcessesExited {
		failures = append(failures, errors.New("media process survived cleanup"))
	}
	if media.children["ffmpeg"] != nil {
		if err := media.validate(); err != nil {
			failures = append(failures, err)
		}
	}
	if runErr != nil {
		failures = append(failures, fmt.Errorf("driver did not complete: %w", runErr))
	}
	combined := errors.Join(failures...)
	if combined == nil {
		media.receipt.Status = "complete"
	} else {
		media.receipt.Error = evidenceText(combined.Error())
	}
	media.receipt.FinishedNS = time.Since(media.started).Nanoseconds()
	return errors.Join(combined, media.save())
}

func signalObserved(process observedProcess, signal os.Signal) error {
	if process.PID == 0 || !observedAlive(process) {
		return nil
	}
	current, err := readObservedProcess(process.PID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || current.StartTicks != process.StartTicks {
		return fmt.Errorf("media process identity changed before signal: %d", process.PID)
	}
	err = syscall.Kill(process.PID, signal.(syscall.Signal))
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (media *collaborationMedia) joinChild(child *collaborationMediaChild, timeout time.Duration, clean bool) error {
	var err error
	timedOut := false
	select {
	case err = <-child.done:
	case <-time.After(timeout):
		timedOut = true
		killErr := child.command.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		err = <-child.done
		err = errors.Join(errors.New("media process did not exit in time"), killErr, err)
	}
	child.record.ExitedNS = time.Since(media.started).Nanoseconds()
	child.record.Joined = true
	child.record.ExitStatus = child.command.ProcessState.ExitCode()
	if (clean || timedOut) && err != nil {
		return err
	}
	return nil
}

func collaborationMediaInput() []string {
	return []string{"-protocol_whitelist", "fd", "-f", "mov", "-enable_drefs", "0", "-use_absolute_path", "0", "-fd", "0", "-i", "fd:"}
}

func (media *collaborationMedia) mediaCommand(program string, args ...string) ([]byte, error) {
	file, err := os.Open(filepath.Join(media.directory, "terminal.mp4"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Stdin = file
	var output, diagnostics bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(program), err, evidenceText(diagnostics.String()))
	}
	return output.Bytes(), nil
}

func (media *collaborationMedia) validate() error {
	probeArgs := []string{"-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=width,height,nb_read_frames:format=duration", "-of", "json"}
	probeArgs = append(probeArgs, collaborationMediaInput()...)
	output, err := media.mediaCommand(media.receipt.Tools["ffprobe"], probeArgs...)
	if err != nil {
		return fmt.Errorf("probe recorded video: %w", err)
	}
	var probe struct {
		Streams []struct {
			Width  int    `json:"width"`
			Height int    `json:"height"`
			Frames string `json:"nb_read_frames"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &probe); err != nil || len(probe.Streams) != 1 {
		return errors.New("recorded video has invalid probe output")
	}
	frames, frameErr := strconv.Atoi(probe.Streams[0].Frames)
	duration, durationErr := strconv.ParseFloat(probe.Format.Duration, 64)
	if frameErr != nil || durationErr != nil || frames < 17 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 1 || probe.Streams[0].Width != 1024 || probe.Streams[0].Height != 768 {
		return errors.New("recorded video has invalid dimensions, duration, or frame count")
	}
	decodeArgs := append([]string{"-v", "error", "-xerror", "-err_detect", "explode"}, collaborationMediaInput()...)
	decodeArgs = append(decodeArgs, "-map", "0:v:0", "-f", "null", "-")
	if _, err := media.mediaCommand(media.receipt.Tools["ffmpeg"], decodeArgs...); err != nil {
		return fmt.Errorf("decode recorded video: %w", err)
	}
	stamp := *media.receipt.Screenshot.SourceTimestampMS
	lastSafeMS := int64(duration*1000) - 200
	if stamp > lastSafeMS {
		stamp = lastSafeMS
		media.receipt.Screenshot.SourceTimestampMS = &stamp
	}
	stampString := strconv.FormatFloat(float64(stamp)/1000, 'f', 3, 64)
	frameArgs := append([]string{"-v", "error", "-xerror", "-ss", stampString}, collaborationMediaInput()...)
	frameArgs = append(frameArgs, "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
	frame, err := media.mediaCommand(media.receipt.Tools["ffmpeg"], frameArgs...)
	if err != nil {
		return fmt.Errorf("extract video screenshot: %w", err)
	}
	image, err := png.Decode(bytes.NewReader(frame))
	if err != nil || image.Bounds().Dx() != 1024 || image.Bounds().Dy() != 768 {
		return errors.New("recorded screenshot is not a valid 1024x768 PNG")
	}
	if err := writePrivate(filepath.Join(media.directory, "screenshot.png"), frame); err != nil {
		return err
	}
	media.receipt.Video.Width, media.receipt.Video.Height, media.receipt.Video.Frames, media.receipt.Video.Duration = 1024, 768, frames, probe.Format.Duration
	media.receipt.Video.SHA256, err = fileSHA256(filepath.Join(media.directory, "terminal.mp4"))
	if err != nil {
		return err
	}
	media.receipt.Screenshot.SHA256, err = fileSHA256(filepath.Join(media.directory, "screenshot.png"))
	media.receipt.Screenshot.SourceVideoSHA256 = media.receipt.Video.SHA256
	media.receipt.Screenshot.Width, media.receipt.Screenshot.Height = 1024, 768
	return err
}
