package piglitter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type itermServer struct {
	pid                                            int
	birth, executable, window, target, tty, helper string
}

var itermSessionID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var itermTTY = regexp.MustCompile(`^/dev/ttys[0-9]+$`)
var itermWindowID = regexp.MustCompile(`^[0-9]+$`)

const itermFind = `on run argv
set lookupValue to item 1 of argv
set lookupField to item 2 of argv
set foundSessions to {}
tell application id "com.googlecode.iterm2"
  repeat with candidateWindow in windows
    repeat with candidateTab in tabs of candidateWindow
      repeat with candidateSession in sessions of candidateTab
        if (lookupField is "tty" and (tty of candidateSession as text) is lookupValue) or (lookupField is "id" and (unique id of candidateSession as text) is lookupValue) then
          set end of foundSessions to ((id of candidateWindow as text) & linefeed & (unique id of candidateSession as text) & linefeed & (tty of candidateSession as text))
        end if
      end repeat
    end repeat
  end repeat
end tell
if (count of foundSessions) is 0 then return "missing"
if (count of foundSessions) is not 1 then error "iTerm session identity is ambiguous"
return item 1 of foundSessions
end run`

const itermSplit = `on run argv
set wantedWindow to item 1 of argv
set wantedSession to item 2 of argv
set wantedTTY to item 3 of argv
set launchCommand to item 4 of argv
tell application id "com.googlecode.iterm2"
  repeat with candidateWindow in windows
    if (id of candidateWindow as text) is wantedWindow then
      repeat with candidateTab in tabs of candidateWindow
        repeat with candidateSession in sessions of candidateTab
          if (unique id of candidateSession as text) is wantedSession and (tty of candidateSession as text) is wantedTTY then
            tell candidateSession to set createdSession to (split vertically with same profile command launchCommand)
            return (id of candidateWindow as text) & linefeed & (unique id of createdSession as text) & linefeed & (tty of createdSession as text)
          end if
        end repeat
      end repeat
    end if
  end repeat
end tell
error "iTerm target session changed"
end run`

const itermClose = `on run argv
set wantedWindow to item 1 of argv
set wantedSession to item 2 of argv
set wantedTTY to item 3 of argv
tell application id "com.googlecode.iterm2"
  repeat with candidateWindow in windows
    repeat with candidateTab in tabs of candidateWindow
      repeat with candidateSession in sessions of candidateTab
        if (unique id of candidateSession as text) is wantedSession then
          if (id of candidateWindow as text) is not wantedWindow or (tty of candidateSession as text) is not wantedTTY then error "iTerm owned session changed"
          tell candidateSession to close
          return "closed"
        end if
      end repeat
    end repeat
  end repeat
end tell
return "missing"
end run`

const itermSelect = `on run argv
set wantedWindow to item 1 of argv
set wantedSession to item 2 of argv
set wantedTTY to item 3 of argv
tell application id "com.googlecode.iterm2"
  repeat with candidateWindow in windows
    if (id of candidateWindow as text) is wantedWindow then
      repeat with candidateTab in tabs of candidateWindow
        repeat with candidateSession in sessions of candidateTab
          if (unique id of candidateSession as text) is wantedSession and (tty of candidateSession as text) is wantedTTY then
            tell candidateTab to select
            tell candidateSession to select
            tell candidateWindow to select
            return "selected"
          end if
        end repeat
      end repeat
    end if
  end repeat
end tell
error "iTerm owned session changed"
end run`

func itermCall(ctx context.Context, script string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/osascript", append([]string{"-"}, args...)...)
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	if err != nil {
		detail, _ := bounded(strings.TrimSpace(string(output)), 1024)
		return "", fmt.Errorf("iTerm AppleScript failed: %w: %s", err, detail)
	}
	return strings.TrimSpace(string(output)), nil
}

func parseITermSession(value string) (window, session, tty string, err error) {
	parts := strings.Fields(value)
	if len(parts) != 3 || !itermWindowID.MatchString(parts[0]) || !itermSessionID.MatchString(parts[1]) || !itermTTY.MatchString(parts[2]) {
		return "", "", "", errors.New("iTerm returned an invalid session identity")
	}
	return parts[0], parts[1], parts[2], nil
}

func processTTY(ctx context.Context, pid int, birth string) (string, error) {
	before, err := processBirth(pid)
	if err != nil || before != birth {
		return "", errors.New("process identity changed before checking its terminal")
	}
	output, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "tty=").Output()
	if err != nil {
		return "", err
	}
	tty := "/dev/" + strings.TrimSpace(string(output))
	after, err := processBirth(pid)
	if err != nil || after != birth || !itermTTY.MatchString(tty) {
		return "", errors.New("process terminal identity is unavailable or changed")
	}
	return tty, nil
}

func itermProcessCandidates(output string, uid int) []int {
	var candidates []int
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != strconv.Itoa(uid) {
			continue
		}
		command := strings.Join(fields[2:], " ")
		const suffix = "/Contents/MacOS/iTerm2"
		index := strings.Index(command, suffix)
		if index < 0 || index+len(suffix) < len(command) && command[index+len(suffix)] != ' ' {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err == nil && pid > 0 {
			candidates = append(candidates, pid)
		}
	}
	return candidates
}

func itermApplicationExecutable(path string) bool {
	if !filepath.IsAbs(path) || !strings.HasSuffix(path, "/Contents/MacOS/iTerm2") {
		return false
	}
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	return filepath.Ext(bundle) == ".app"
}

func currentITermMemberEnvironment(ctx context.Context, sourcePath string, hostPID int, hostBirth, hostScope string) (tmuxMemberEnvironment, error) {
	if runtime.GOOS != "darwin" {
		return tmuxMemberEnvironment{}, errors.New("iTerm2 requires macOS")
	}
	helper, err := nativeMemberExecutable(ctx, sourcePath)
	if err != nil {
		return tmuxMemberEnvironment{}, err
	}
	scope, err := memberProcessScope(os.Getpid())
	if err != nil || scope != hostScope {
		return tmuxMemberEnvironment{}, errors.New("host process scope is unavailable or changed")
	}
	tty, err := processTTY(ctx, hostPID, hostBirth)
	if err != nil {
		return tmuxMemberEnvironment{}, fmt.Errorf("identify lead terminal: %w", err)
	}
	output, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "uid=,pid=,command=").Output()
	if err != nil {
		return tmuxMemberEnvironment{}, errors.New("iTerm2 application processes cannot be inspected")
	}
	var pid int
	var birth, executable string
	for _, candidate := range itermProcessCandidates(string(output), os.Getuid()) {
		candidateBirth, err := processBirth(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return tmuxMemberEnvironment{}, errors.New("iTerm2 process identity is uncertain")
		}
		path, err := memberProcessPath(candidate, candidateBirth, helper)
		if err != nil {
			if _, birthErr := processBirth(candidate); errors.Is(birthErr, os.ErrNotExist) {
				continue
			}
			return tmuxMemberEnvironment{}, errors.New("iTerm2 process executable is uncertain")
		}
		if !itermApplicationExecutable(path) {
			continue
		}
		candidateScope, err := memberProcessScope(candidate)
		if err != nil || candidateScope != scope {
			return tmuxMemberEnvironment{}, errors.New("iTerm2 process scope is unavailable or changed")
		}
		if pid != 0 {
			return tmuxMemberEnvironment{}, errors.New("multiple iTerm2 application processes are present")
		}
		pid, birth, executable = candidate, candidateBirth, path
	}
	if pid == 0 {
		return tmuxMemberEnvironment{}, errors.New("iTerm2 application process was not found")
	}
	value, err := itermCall(ctx, itermFind, tty, "tty")
	if err != nil {
		return tmuxMemberEnvironment{}, err
	}
	window, target, foundTTY, err := parseITermSession(value)
	if err != nil || foundTTY != tty {
		return tmuxMemberEnvironment{}, errors.New("lead process is not in an identifiable iTerm2 session")
	}
	server := &itermServer{pid: pid, birth: birth, executable: executable, window: window, target: target, tty: tty, helper: helper}
	if err := server.verify(ctx); err != nil {
		return tmuxMemberEnvironment{}, err
	}
	return tmuxMemberEnvironment{server: tmuxServer{socket: executable, pid: pid, birth: birth, target: target, iterm: server}, executable: helper, usePiDirs: os.Getenv("PIG_USE_PI_DIRS") == "1"}, nil
}

func (server *itermServer) verifyApp() error {
	birth, err := processBirth(server.pid)
	if err != nil || birth != server.birth {
		return errors.New("iTerm2 application identity changed")
	}
	matches, err := memberProcessMatchesExecutable(server.pid, server.birth, server.executable, server.helper)
	if err != nil || !matches {
		return errors.New("iTerm2 application executable changed")
	}
	return nil
}

func (server *itermServer) verify(ctx context.Context) error {
	if err := server.verifyApp(); err != nil {
		return err
	}
	value, err := itermCall(ctx, itermFind, server.target, "id")
	if err != nil {
		return err
	}
	window, id, tty, err := parseITermSession(value)
	if err != nil || window != server.window || id != server.target || tty != server.tty {
		return errors.New("iTerm2 lead session identity changed")
	}
	return server.verifyApp()
}

func (server *itermServer) launch(ctx context.Context, owner tmuxServer, launch tmuxLaunch) (tmuxPane, error) {
	if err := launch.validate(); err != nil {
		return tmuxPane{}, err
	}
	if err := server.verify(ctx); err != nil {
		return tmuxPane{}, err
	}
	command := launch.executable + " --pig-litter-member " + launch.manifest
	value, err := itermCall(ctx, itermSplit, server.window, server.target, server.tty, command)
	if err != nil {
		return tmuxPane{}, fmt.Errorf("iTerm pane allocation is uncertain: %w", err)
	}
	window, id, tty, err := parseITermSession(value)
	if err != nil || window != server.window || id == server.target || tty == server.tty {
		return tmuxPane{}, errors.New("iTerm pane allocation returned an unexpected identity")
	}
	pane := tmuxPane{server: owner, id: id, command: command, tty: tty}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		output, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,tty=,command=").Output()
		if err != nil {
			return tmuxPane{}, fmt.Errorf("identify iTerm member process: %w", err)
		}
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[1] != filepath.Base(tty) || strings.Join(fields[2:], " ") != command {
				continue
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil || pid <= 0 || pane.pid != 0 {
				return tmuxPane{}, errors.New("iTerm member process identity is ambiguous")
			}
			pane.pid = pid
		}
		if pane.pid != 0 {
			pane.birth, err = processBirth(pane.pid)
			if err != nil {
				return tmuxPane{}, err
			}
			if err := server.verifyPane(ctx, memberPaneIdentity{ID: pane.id, PID: pane.pid, Birth: pane.birth, Command: pane.command, TTY: pane.tty}); err != nil {
				return tmuxPane{}, err
			}
			return pane, nil
		}
		select {
		case <-ctx.Done():
			return tmuxPane{}, ctx.Err()
		case <-deadline.C:
			return tmuxPane{}, errors.New("iTerm member process could not be identified; ownership retained")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (server *itermServer) verifyPane(ctx context.Context, pane memberPaneIdentity) error {
	if err := server.verify(ctx); err != nil {
		return err
	}
	value, err := itermCall(ctx, itermFind, pane.ID, "id")
	if err != nil {
		return err
	}
	window, id, tty, err := parseITermSession(value)
	if err != nil || window != server.window || id != pane.ID || tty != pane.TTY || id == server.target {
		return errors.New("iTerm owned pane identity changed")
	}
	processTTY, err := processTTY(ctx, pane.PID, pane.Birth)
	if err != nil || processTTY != pane.TTY {
		return errors.New("iTerm member process terminal changed")
	}
	executable, _, ok := strings.Cut(pane.Command, " ")
	matches, err := memberProcessMatchesExecutable(pane.PID, pane.Birth, executable, server.helper)
	if !ok || err != nil || !matches {
		return errors.New("iTerm member executable changed")
	}
	return server.verifyApp()
}

func (server *itermServer) close(ctx context.Context, pane tmuxPane) error {
	if err := server.verifyApp(); err != nil {
		return err
	}
	value, err := itermCall(ctx, itermFind, pane.id, "id")
	if err != nil {
		return err
	}
	if value == "missing" {
		return nil
	}
	window, id, tty, err := parseITermSession(value)
	if err != nil || window != server.window || id != pane.id || tty != pane.tty || id == server.target {
		return errors.New("iTerm owned pane changed; refusing cleanup")
	}
	birth, birthErr := processBirth(pane.pid)
	if birthErr == nil && birth != pane.birth || birthErr != nil && !errors.Is(birthErr, os.ErrNotExist) {
		return errors.New("iTerm member process identity changed; refusing cleanup")
	}
	if birthErr == nil {
		currentTTY, err := processTTY(ctx, pane.pid, pane.birth)
		if err != nil || currentTTY != pane.tty {
			return errors.New("iTerm member process terminal changed; refusing cleanup")
		}
		executable, _, ok := strings.Cut(pane.command, " ")
		matches, err := memberProcessMatchesExecutable(pane.pid, pane.birth, executable, server.helper)
		if !ok || err != nil || !matches {
			return errors.New("iTerm member executable changed; refusing cleanup")
		}
	}
	if err := server.verifyApp(); err != nil {
		return err
	}
	_, err = itermCall(ctx, itermClose, window, id, tty)
	return err
}

func itermServerFromIdentity(identity memberServerIdentity, helper string) tmuxServer {
	server := &itermServer{pid: identity.PID, birth: identity.Birth, executable: identity.Socket, window: identity.Window, target: identity.Target, tty: identity.TTY, helper: helper}
	return tmuxServer{socket: identity.Socket, pid: identity.PID, birth: identity.Birth, target: identity.Target, iterm: server}
}

func confirmOldITermMemberGone(ctx context.Context, ownership *memberOwnership) error {
	server := itermServerFromIdentity(ownership.Server, ownership.Executable).iterm
	if err := server.verifyApp(); err != nil {
		return reject("unavailable", "previous iTerm application is unavailable or changed")
	}
	value, err := itermCall(ctx, itermFind, ownership.Pane.ID, "id")
	if err != nil || value != "missing" {
		return reject("unavailable", "previous iTerm pane still exists or is uncertain")
	}
	if err := server.verifyApp(); err != nil {
		return reject("unavailable", "previous iTerm application changed during reconciliation")
	}
	return nil
}

func confirmReservedITermUnstarted(ctx context.Context, ownership *memberOwnership, scope string) error {
	server := itermServerFromIdentity(ownership.Server, ownership.Executable).iterm
	serverScope, err := memberProcessScope(server.pid)
	if err != nil || serverScope != scope || server.verify(ctx) != nil {
		return reject("unavailable", "native iTerm application or lead session is unavailable or changed")
	}
	output, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "command=").Output()
	if err != nil {
		return reject("unavailable", "native iTerm processes cannot be verified")
	}
	command := ownership.Executable + " --pig-litter-member " + ownership.Manifest
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == command {
			return reject("unavailable", "native iTerm allocation may have started")
		}
	}
	if err := server.verify(ctx); err != nil {
		return reject("unavailable", "native iTerm application or lead session changed")
	}
	return nil
}
