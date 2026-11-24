package piglitter

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type tmuxServer struct {
	command string
	socket  string
	pid     int
	birth   string
	target  string
	iterm   *itermServer
}

type tmuxLaunch struct {
	executable string
	manifest   string
	cwd        string
	nonce      string
}

type tmuxPane struct {
	server  tmuxServer
	id      string
	pid     int
	birth   string
	command string
	tty     string
}

type tmuxPaneState struct {
	id      string
	pid     int
	dead    bool
	command string
}

var tmuxPaneID = regexp.MustCompile(`^%[0-9]+$`)
var tmuxLaunchNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)

func tmuxFromEnvironment(ctx context.Context, socketEnv, target string) (tmuxServer, error) {
	var server tmuxServer
	last := strings.LastIndexByte(socketEnv, ',')
	if last < 0 {
		return server, errors.New("tmux is not active; run the lead in tmux or choose embedded")
	}
	prefix := socketEnv[:last]
	middle := strings.LastIndexByte(prefix, ',')
	if middle < 0 || !filepath.IsAbs(prefix[:middle]) || !tmuxPaneID.MatchString(target) {
		return server, errors.New("tmux server and target pane are unavailable")
	}
	pid, err := strconv.Atoi(prefix[middle+1:])
	if err != nil || pid <= 0 {
		return server, errors.New("tmux server identity is invalid")
	}
	command, err := exec.LookPath("tmux")
	if err != nil {
		return server, errors.New("tmux executable is unavailable; install tmux or choose embedded")
	}
	birth, err := processBirth(pid)
	if err != nil {
		return server, fmt.Errorf("identify tmux server: %w", err)
	}
	server = tmuxServer{command: command, socket: prefix[:middle], pid: pid, birth: birth, target: target}
	if err := server.verify(ctx); err != nil {
		return tmuxServer{}, err
	}
	return server, nil
}

func (server tmuxServer) call(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, server.command, append([]string{"-S", server.socket}, args...)...).CombinedOutput()
	if err != nil {
		detail, _ := bounded(strings.TrimSpace(string(output)), 1024)
		return "", fmt.Errorf("tmux %s failed: %w: %s", args[0], err, detail)
	}
	return strings.TrimSuffix(string(output), "\n"), nil
}

func (server tmuxServer) verify(ctx context.Context) error {
	if server.iterm != nil {
		return server.iterm.verify(ctx)
	}
	birth, err := processBirth(server.pid)
	if err != nil || birth != server.birth {
		return errors.New("tmux server process identity changed")
	}
	value, err := server.call(ctx, "list-panes", "-a", "-F", "#{pid}")
	if err != nil || value == "" {
		return errors.New("tmux server is unavailable")
	}
	for _, pid := range strings.Split(value, "\n") {
		if pid != strconv.Itoa(server.pid) {
			return errors.New("tmux server identity changed")
		}
	}
	return nil
}

func (server tmuxServer) backend() backendRequest {
	if server.iterm != nil {
		return backendITerm
	}
	return backendTmux
}

func (server tmuxServer) identity() memberServerIdentity {
	identity := memberServerIdentity{Socket: server.socket, PID: server.pid, Birth: server.birth, Target: server.target}
	if server.iterm != nil {
		identity.Window, identity.TTY = server.iterm.window, server.iterm.tty
	}
	return identity
}

func safeLaunchPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, r := range path {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-", r)) {
			return false
		}
	}
	return true
}

func (launch tmuxLaunch) validate() error {
	if !safeLaunchPath(launch.executable) || !safeLaunchPath(launch.manifest) {
		return errors.New("native member executable and manifest need absolute paths without shell metacharacters")
	}
	if !tmuxLaunchNonce.MatchString(launch.nonce) || filepath.Base(launch.manifest) != "member-"+launch.nonce+".json" {
		return errors.New("native member manifest must contain its unique launch nonce")
	}
	if !filepath.IsAbs(launch.cwd) || strings.IndexFunc(launch.cwd, unicode.IsControl) >= 0 {
		return errors.New("native member cwd must be an absolute directory without control characters")
	}
	actual, err := canonicalTeamDirectory(launch.cwd)
	if err != nil || actual != launch.cwd {
		return errors.New("native member cwd must name an existing canonical directory")
	}
	return nil
}

func parseTmuxPane(value string) (tmuxPaneState, error) {
	fields := strings.SplitN(value, "\t", 4)
	if len(fields) != 4 || !tmuxPaneID.MatchString(fields[0]) {
		return tmuxPaneState{}, errors.New("tmux returned an invalid pane identity")
	}
	if fields[2] != "0" && fields[2] != "1" {
		return tmuxPaneState{}, errors.New("tmux returned an invalid pane process")
	}
	pid := 0
	if fields[1] != "" {
		var err error
		pid, err = strconv.Atoi(fields[1])
		if err != nil || pid <= 0 {
			return tmuxPaneState{}, errors.New("tmux returned an invalid pane process")
		}
	} else if fields[2] != "1" {
		return tmuxPaneState{}, errors.New("tmux returned an invalid pane process")
	}
	return tmuxPaneState{id: fields[0], pid: pid, dead: fields[2] == "1", command: fields[3]}, nil
}

const tmuxPaneFormat = "#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_start_command}"

func (server tmuxServer) panes(ctx context.Context) ([]tmuxPaneState, error) {
	value, err := server.call(ctx, "list-panes", "-a", "-F", tmuxPaneFormat)
	if err != nil {
		return nil, err
	}
	var panes []tmuxPaneState
	for _, line := range strings.Split(value, "\n") {
		pane, err := parseTmuxPane(line)
		if err != nil {
			return nil, err
		}
		panes = append(panes, pane)
	}
	return panes, nil
}

func (server tmuxServer) rollbackLaunch(command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.verify(ctx); err != nil {
		return err
	}
	panes, err := server.panes(ctx)
	if err != nil {
		return err
	}
	var match *tmuxPaneState
	for _, pane := range panes {
		if pane.command == command {
			if match != nil {
				return errors.New("multiple panes match the launch; ownership is uncertain")
			}
			copy := pane
			match = &copy
		}
	}
	if match == nil {
		return errors.New("no pane matches the launch; allocation is uncertain")
	}
	pane := tmuxPane{server: server, id: match.id, pid: match.pid, command: command}
	if !match.dead {
		pane.birth, err = processBirth(match.pid)
		if err != nil {
			return err
		}
	}
	return pane.close(ctx)
}

func (server tmuxServer) launch(ctx context.Context, launch tmuxLaunch) (tmuxPane, error) {
	if server.iterm != nil {
		return server.iterm.launch(ctx, server, launch)
	}
	if err := launch.validate(); err != nil {
		return tmuxPane{}, err
	}
	if err := server.verify(ctx); err != nil {
		return tmuxPane{}, err
	}
	command := launch.executable + " --pig-litter-member " + launch.manifest
	output, err := server.call(ctx, "split-window", "-d", "-P", "-F", tmuxPaneFormat, "-t", server.target, "-c", launch.cwd, "--", launch.executable, "--pig-litter-member", launch.manifest)
	if err != nil {
		if cleanupErr := server.rollbackLaunch(command); cleanupErr != nil {
			return tmuxPane{}, fmt.Errorf("pane allocation could not be confirmed: %w; cleanup: %v", err, cleanupErr)
		}
		return tmuxPane{}, fmt.Errorf("pane allocation failed and the owned pane was removed: %w", err)
	}
	state, err := parseTmuxPane(output)
	if err != nil || state.command != command {
		if cleanupErr := server.rollbackLaunch(command); cleanupErr != nil {
			return tmuxPane{}, fmt.Errorf("pane allocation returned an unexpected identity; cleanup: %w", cleanupErr)
		}
		return tmuxPane{}, errors.New("pane allocation returned an unexpected identity; the owned pane was removed")
	}
	pane := tmuxPane{server: server, id: state.id, pid: state.pid, command: command}
	pane.birth, err = processBirth(pane.pid)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cleanupErr := pane.close(cleanupCtx)
		return tmuxPane{}, errors.Join(fmt.Errorf("identify native member process: %w", err), cleanupErr)
	}
	return pane, nil
}

func (pane tmuxPane) close(ctx context.Context) error {
	if pane.server.iterm != nil {
		return pane.server.iterm.close(ctx, pane)
	}
	if err := pane.server.verify(ctx); err != nil {
		return err
	}
	panes, err := pane.server.panes(ctx)
	if err != nil {
		return err
	}
	var found *tmuxPaneState
	for _, state := range panes {
		if state.id == pane.id {
			found = &state
			break
		}
	}
	if found == nil {
		return nil
	}
	if found.command != pane.command || !found.dead && found.pid != pane.pid {
		return errors.New("owned tmux pane identity changed; refusing cleanup")
	}
	if !found.dead {
		birth, err := processBirth(found.pid)
		if err != nil || pane.birth == "" || birth != pane.birth {
			return errors.New("owned tmux process identity changed; refusing cleanup")
		}
	}
	_, err = pane.server.call(ctx, "kill-pane", "-t", pane.id)
	return err
}
