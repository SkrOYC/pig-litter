#!/usr/bin/env python3
import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import subprocess
import sys
import threading
import time
import unicodedata
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from iterm_native import native_windows
from iterm_window import ACKNOWLEDGE_WARNING, UI_CENSUS, VERIFY_RESIDUE, WINDOW_EXISTS
from iterm_zero_close import OWNED_ALTERNATE


NAMES = ("alpha", "beta", "gamma")
FILE_TOOLS = {"read", "write", "edit", "bash"}
CONTROL_TOOLS = {"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
TEAM_TOOLS = {"team_spawn", "team_list"}


def require(condition, reason):
    if not condition:
        raise RuntimeError(reason)


def safe_text(value):
    require(isinstance(value, str) and not any(unicodedata.category(c) == "Cc" for c in value), "Control character in terminal input or command argument")
    return value


def text_content(value):
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "".join(part.get("text", "") for part in value if isinstance(part, dict))
    return ""


def results(messages):
    values = []
    for message in messages:
        if message.get("role") != "tool":
            continue
        content = text_content(message.get("content"))
        try:
            values.append(json.loads(content))
        except json.JSONDecodeError:
            values.append(content)
    return values


def call(tool, **arguments):
    return [(tool, arguments)]


class Fixture:
    def __init__(self, scenario, workspace, evidence):
        self.scenario = scenario
        self.workspace = workspace
        self.evidence = evidence
        self.changed = threading.Condition()
        self.stage = 0
        self.sequence = 0
        self.requests = 0
        self.request_models = {}
        self.errors = []
        self.refs = {}
        self.started = set()
        self.wrote = set()
        self.rosters = {}
        self.observations = {}
        self.ordinary_started = False
        self.ordinary_release = False

    def wait_for(self, predicate, reason):
        require(self.changed.wait_for(predicate, timeout=35), reason)

    def tool_inventory(self, request, selected=True):
        names = {t.get("function", {}).get("name") for t in request.get("tools", [])}
        required = FILE_TOOLS | CONTROL_TOOLS | TEAM_TOOLS if selected else FILE_TOOLS
        require(required <= names, f"Missing tools: {sorted(required - names)}")
        if not selected:
            require(not names & (CONTROL_TOOLS | TEAM_TOOLS), "Unselected session registered Pig Litter tools")
        return sorted(names)

    def check_roster(self, result, states=None):
        require(result.get("operation") == "team_list" and result.get("state") == "listed", "team_list did not succeed")
        require(not result.get("pending") and len(result.get("members", [])) == 4, "Roster has missing or pending members")
        team = next(iter(self.refs.values()))["team"]
        require(result.get("team") == team, "Roster team identity changed")
        seen = {}
        for member in result["members"]:
            name = member.get("name")
            require(name not in seen, "Duplicate roster entry")
            if name == "team-lead":
                require(member.get("role") == "lead" and member.get("ref", {}).get("id") == team["root"] and member["ref"].get("team") == team, "Invalid roster lead")
            else:
                require(name in self.refs and member.get("role") == "teammate" and member.get("ref") == self.refs[name], "Invalid teammate reference")
                require(member.get("cwd") == str(self.workspace) and member.get("workspace") == {"kind": "shared_checkout", "cwd": str(self.workspace)}, "Teammate workspace changed")
                if states and member.get("state") != states[name]:
                    return False
            seen[name] = member.get("ref")
        require(set(seen) == set(NAMES) | {"team-lead"}, "Roster identities are incomplete")
        return True

    def team_turn(self, request, tool_results, prompt):
        model = request["model"]
        self.tool_inventory(request)
        if model.startswith("team_"):
            name = model.removeprefix("team_")
            require(name in NAMES, "Unexpected team member")
            if len(tool_results) == 0:
                self.started.add(name)
                self.changed.notify_all()
                self.wait_for(lambda: len(self.started) == 3, "Three member requests did not overlap")
                return call("write", path=f"team-{name}.txt", content=f"team-{name}-sentinel")
            if len(tool_results) == 1:
                require((self.workspace / f"team-{name}.txt").read_text() == f"team-{name}-sentinel", "Member file effect is missing")
                self.wrote.add(name)
                self.changed.notify_all()
                self.wait_for(lambda: len(self.wrote) == 3 and len(self.refs) == 3, "Members or admissions did not complete")
                return call("team_list")
            if len(tool_results) == 2:
                require(self.check_roster(tool_results[-1]), "Member roster mismatch")
                self.rosters[name] = tool_results[-1]
                self.changed.notify_all()
                return f"P03_{name.upper()}_DONE"
            raise RuntimeError("Unexpected team member turn")
        require(model == "parent", "Unexpected team model")
        if self.stage == 0:
            require(prompt == "P03_TEAM_FIRST", "Unexpected team prompt")
            self.stage = 1
            return [("team_spawn", {"type": "worker", "name": name, "task": f"Write the controlled team fixture file for {name}", "model": f"fixture/team_{name}", "workspace": {"kind": "shared_checkout"}}) for name in NAMES]
        if self.stage == 1:
            admissions = tool_results[-3:]
            require(len(admissions) == 3, "Three admissions are missing")
            for value in admissions:
                name = value.get("name")
                ref = value.get("member", {})
                require(value.get("operation") == "team_spawn" and value.get("state") == "starting" and name in NAMES and ref.get("id") and ref.get("generation"), "Invalid team admission")
                self.refs[name] = ref
            require(len(self.refs) == 3 and len({ref["id"] for ref in self.refs.values()}) == 3 and len({json.dumps(ref["team"], sort_keys=True) for ref in self.refs.values()}) == 1, "Team admissions have inconsistent identities")
            self.changed.notify_all()
            self.wait_for(lambda: len(self.rosters) == 3, "Member rosters are missing")
            self.stage = 2
            return call("team_list")
        if self.stage == 2:
            if not self.check_roster(tool_results[-1], {name: "idle" for name in NAMES}):
                return call("team_list")
            self.observations["lead_roster"] = tool_results[-1]
            self.stage = 3
            return call("litter_list")
        if self.stage == 3:
            require(tool_results[-1].get("operation") == "litter_list" and not tool_results[-1].get("children"), "Teammates appeared in the ordinary child list")
            self.stage = 4
            return call("litter_stop", id=self.refs["alpha"]["id"])
        if self.stage == 4:
            require(tool_results[-1].get("operation") == "litter_stop" and tool_results[-1].get("state") == "paused", "Member pause failed")
            self.stage = 5
            return call("team_list")
        if self.stage == 5:
            require(self.check_roster(tool_results[-1], {"alpha": "paused", "beta": "idle", "gamma": "idle"}), "Paused roster did not settle")
            self.observations["before_reopen"] = tool_results[-1]
            self.stage = 6
            return "P03_TEAM_FIRST_DONE"
        if self.stage == 6:
            require(prompt == "P03_TEAM_RESTART", "Reopen prompt is missing")
            self.stage = 7
            return call("team_list")
        if self.stage == 7:
            require(self.check_roster(tool_results[-1], {"alpha": "paused", "beta": "idle", "gamma": "idle"}), "Reopened roster differs")
            self.observations["after_reopen"] = tool_results[-1]
            self.stage = 8
            return call("litter_inspect", id=self.refs["alpha"]["id"])
        if self.stage == 8:
            child = tool_results[-1].get("child", {})
            require(tool_results[-1].get("operation") == "litter_inspect" and child.get("id") == self.refs["alpha"]["id"] and child.get("state") == "paused", "Reopened member inspection differs")
            self.observations["reopened_inspection"] = tool_results[-1]
            self.stage = 9
            return "P03_TEAM_REOPENED"
        raise RuntimeError("Unexpected team lead turn")

    def policy_turn(self, request, tool_results):
        model = request["model"]
        if model == "policy_ordinary":
            self.ordinary_started = True
            self.changed.notify_all()
            self.wait_for(lambda: self.ordinary_release, "Ordinary child was not released")
            return "P03_ORDINARY_DONE"
        if model == "policy_duplicate":
            return "P03_DUPLICATE_DONE"
        self.tool_inventory(request)
        if model == "policy_member":
            if not tool_results:
                return call("team_spawn", type="worker", name="forged", task="Attempt nested teammate", model="fixture/policy_duplicate", workspace={"kind": "shared_checkout"})
            denial = tool_results[-1]
            require(denial.get("operation") == "team_spawn" and denial.get("code") == "lead_required", "Teammate admission was not refused")
            self.observations["flat_team_denial"] = denial
            self.changed.notify_all()
            return "P03_MEMBER_POLICY_DONE"
        require(model == "parent", "Unexpected policy model")
        if self.stage == 0:
            self.stage = 1
            return call("team_spawn", type="worker", name="alpha", task="Check teammate authority", model="fixture/policy_member", workspace={"kind": "shared_checkout"})
        if self.stage == 1:
            value = tool_results[-1]
            require(value.get("operation") == "team_spawn" and value.get("name") == "alpha" and value.get("member", {}).get("id"), "Policy member admission failed")
            self.refs["alpha"] = value["member"]
            self.stage = 2
            return call("team_spawn", type="worker", name="team-lead", task="Attempt reserved name", model="fixture/policy_duplicate", workspace={"kind": "shared_checkout"})
        if self.stage == 2:
            value = tool_results[-1]
            require(value.get("operation") == "team_spawn" and value.get("state") == "rejected" and value.get("code") == "rejected" and "reserved" in value.get("report", ""), "Reserved name was not refused")
            self.observations["reserved_denial"] = value
            self.stage = 3
            return call("team_spawn", type="worker", name="alpha", task="Accept a duplicate name", model="fixture/policy_duplicate", workspace={"kind": "shared_checkout"})
        if self.stage == 3:
            value = tool_results[-1]
            ref = value.get("member", {})
            require(value.get("operation") == "team_spawn" and value.get("name") == "alpha-2" and ref.get("id") and ref["id"] != self.refs["alpha"]["id"], "Duplicate name did not receive its suffix")
            self.refs["alpha-2"] = ref
            self.stage = 4
            return call("litter_spawn", type="scout", name="alpha", task="Return the named ordinary marker", model="fixture/policy_ordinary")
        if self.stage == 4:
            value = tool_results[-1]
            child = value.get("child", {})
            require(value.get("operation") == "litter_spawn" and child.get("id") and child.get("name") == "alpha" and child.get("parent") == self.refs["alpha"]["team"]["root"] and child["id"] not in {r["id"] for r in self.refs.values()}, "Ordinary child admission differs from the member")
            self.observations["ordinary_admission"] = value
            self.wait_for(lambda: self.ordinary_started and "flat_team_denial" in self.observations, "Ordinary child or member refusal is missing")
            self.stage = 5
            return call("team_list")
        if self.stage == 5:
            value = tool_results[-1]
            members = value.get("members", [])
            roster = {m.get("name"): m.get("ref") for m in members}
            require(value.get("operation") == "team_list" and value.get("team") == self.refs["alpha"]["team"] and len(members) == 3 and roster.get("alpha") == self.refs["alpha"] and roster.get("alpha-2") == self.refs["alpha-2"] and roster.get("team-lead", {}).get("id") == self.refs["alpha"]["team"]["root"] and not value.get("pending"), "Policy roster contains a forbidden or missing member")
            self.observations["policy_roster"] = value
            self.stage = 6
            return call("litter_list")
        if self.stage == 6:
            value = tool_results[-1]
            children = value.get("children", [])
            ordinary = self.observations["ordinary_admission"]["child"]
            require(value.get("operation") == "litter_list" and any(c.get("id") == ordinary["id"] and c.get("name") == "alpha" for c in children) and not any(c.get("id") in {r["id"] for r in self.refs.values()} for c in children), "Ordinary list is inconsistent")
            self.observations["ordinary_list"] = value
            self.ordinary_release = True
            self.changed.notify_all()
            self.stage = 7
            return call("litter_wait", id=ordinary["id"], timeoutMs=30000)
        if self.stage == 7:
            value = tool_results[-1]
            outcome = value.get("outcome") or {}
            require(value.get("operation") == "litter_wait" and value.get("state") == "completed" and outcome.get("text") == "P03_ORDINARY_DONE" and outcome.get("run", {}).get("id") == self.observations["ordinary_admission"]["child"]["id"], "Ordinary child did not complete")
            self.observations["ordinary_outcome"] = value
            self.stage = 8
            return "P03_POLICY_DONE"
        raise RuntimeError("Unexpected policy lead turn")

    def iterm_turn(self, request, tool_results, prompt):
        model = request["model"]
        if model == "team_alpha":
            if not tool_results:
                matches = [process for process in process_snapshot().values() if f"{self.extension} --pig-litter-member " in process["command"]]
                require(len(matches) == 1, "Native helper process is missing or ambiguous")
                member = matches[0]
                member["executable"] = executable(member["pid"])
                require(member["executable"] == os.path.realpath(self.extension), "Native helper executable changed")
                tty = "/dev/" + member["tty"]
                window, session = applescript(PANE_BY_TTY, tty).split("\t")
                require(window == self.lead["id"] and session != self.lead["session"] and tty != self.lead["tty"], "Native member is not in a distinct pane of the lead window")
                member.update({"window": window, "session": session, "terminal": tty})
                self.observations["native_member"] = member
                probe = probe_identity(Path(os.environ["RUNNER_TEMP"]) / "collaboration-platform-probe" / "result.json")
                require(probe and applescript(SESSION, *probe, "exists") == "present", "Unrelated probe window changed")
                return call("write", path="p08-iterm.txt", content="P08_ITERM_SIDE_EFFECT")
            require((self.workspace / "p08-iterm.txt").read_text() == "P08_ITERM_SIDE_EFFECT", "Native member side effect is missing")
            return "P08_ITERM_MEMBER_DONE"
        require(model == "parent", "Unexpected iTerm lead model")
        if self.stage == 0:
            require(prompt == "P08_ITERM", "Unexpected iTerm prompt")
            self.stage = 1
            return call("team_spawn", type="worker", name="alpha", task="Write the iTerm fixture file and report completion", model="fixture/team_alpha", workspace={"kind": "shared_checkout"}, backend="iterm2")
        if self.stage == 1:
            admission = tool_results[-1]
            ref = admission.get("member", {})
            require(admission.get("operation") == "team_spawn" and admission.get("state") == "starting" and ref.get("id"), f"iTerm admission failed: {admission}")
            self.refs["alpha"] = ref
            self.stage = 2
            return call("litter_wait", id=ref["id"], timeoutMs=90000)
        if self.stage == 2:
            result = tool_results[-1]
            require(result.get("operation") == "litter_wait" and result.get("state") == "completed" and (result.get("outcome") or {}).get("text") == "P08_ITERM_MEMBER_DONE", f"iTerm completion failed: {result}")
            self.observations["native_outcome"] = result
            self.stage = 3
            return "P08_ITERM_DONE"
        raise RuntimeError("Unexpected iTerm lead turn")

    def decide(self, request):
        with self.changed:
            self.requests += 1
            model = request.get("model", "")
            self.request_models[model] = self.request_models.get(model, 0) + 1
            require(self.requests <= 160, "Provider request limit exceeded")
            index = self.requests
            (self.evidence / f"request-{index:03}.json").write_text(json.dumps(request, indent=2) + "\n")
            messages = request.get("messages", [])
            prompt = next((text_content(m.get("content")) for m in reversed(messages) if m.get("role") == "user"), "")
            if self.scenario == "unselected":
                require(model == "parent" and prompt == "P03_UNSELECTED", "Unexpected unselected request")
                self.observations["unselected_tools"] = self.tool_inventory(request, selected=False)
                self.stage = 1
                return "P03_UNSELECTED_DONE"
            tool_results = results(messages)
            if self.scenario == "iterm":
                return self.iterm_turn(request, tool_results, prompt)
            if self.scenario == "team":
                return self.team_turn(request, tool_results, prompt)
            return self.policy_turn(request, tool_results)

    def response(self, request):
        turn = self.decide(request)
        with self.changed:
            self.sequence += 1
            sequence = self.sequence
        if isinstance(turn, str):
            chunks = [({"role": "assistant", "content": turn}, None), ({}, "stop")]
        else:
            calls = [{"index": i, "id": f"p03-{sequence}-{i}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}} for i, (name, args) in enumerate(turn)]
            chunks = [({"role": "assistant", "tool_calls": calls}, "tool_calls")]
        output = []
        for delta, finish in chunks:
            data = {"id": "p03-fixture", "object": "chat.completion.chunk", "created": 1, "model": request["model"], "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            if finish:
                data["usage"] = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
            output.append("data: " + json.dumps(data) + "\n\n")
        return ("".join(output) + "data: [DONE]\n\n").encode()


def start_provider(fixture):
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            try:
                require(self.path.endswith("/chat/completions"), "Unexpected provider route")
                length = int(self.headers.get("Content-Length", "0"))
                require(0 < length <= 8 * 1024 * 1024, "Provider request size is invalid")
                request = json.loads(self.rfile.read(length))
                data = fixture.response(request)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            except Exception as error:
                with fixture.changed:
                    fixture.errors.append(str(error))
                    fixture.changed.notify_all()
                try:
                    self.send_error(500, str(error))
                except OSError:
                    pass

        def log_message(self, *_args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


CREATE = r'''
tell application "iTerm"
  activate
  set ownedWindow to (create window with default profile)
  set ownedSession to current session of ownedWindow
  tell ownedSession
    set columns to 120
    set rows to 32
  end tell
  return (unique id of ownedSession) & linefeed & (tty of ownedSession) & linefeed & (id of ownedWindow as text)
end tell
'''

SESSION = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  set actionName to item 3 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        repeat with candidateTab in tabs of candidateWindow
          repeat with candidateSession in sessions of candidateTab
            if (unique id of candidateSession) is wantedSession then
              if actionName is "exists" then return "present"
              if actionName is "text" then return text of candidateSession
              if actionName is "send" then
                tell candidateSession to write text (item 4 of argv)
                return "sent"
              end if
              if actionName is "exit" then
                set controlD to ASCII character 4
                tell candidateSession to write text controlD newline NO
                return "sent exit key"
              end if
              if actionName is "close" then
                tell candidateWindow to close
                return "closed"
              end if
              error "Unknown action."
            end if
          end repeat
        end repeat
        if actionName is "close" then return "session missing"
        if actionName is "exists" then return "missing"
        error "Owned window session changed."
      end if
    end repeat
  end tell
  if actionName is "close" then return "already closed"
  if actionName is "exists" then return "missing"
  error "Owned window is missing."
end run
'''


SIBLING = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  set actionName to item 3 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        repeat with candidateTab in tabs of candidateWindow
          repeat with candidateSession in sessions of candidateTab
            if (unique id of candidateSession as text) is wantedSession then
              if actionName is "create" then
                tell candidateSession to set other to (split vertically with same profile command "/bin/sleep 600")
                return (unique id of other as text) & tab & (tty of other as text)
              end if
              if actionName is "close" and (tty of candidateSession as text) is (item 4 of argv) then
                tell candidateSession to close
                return "closed"
              end if
            end if
          end repeat
        end repeat
      end if
    end repeat
  end tell
  error "Fixture sibling session is unavailable or changed"
end run
'''


PANE_BY_TTY = r'''
on run argv
  set wantedTTY to item 1 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      repeat with candidateTab in tabs of candidateWindow
        repeat with candidateSession in sessions of candidateTab
          if (tty of candidateSession as text) is wantedTTY then return (id of candidateWindow as text) & tab & (unique id of candidateSession as text)
        end repeat
      end repeat
    end repeat
  end tell
  return "missing"
end run
'''


def applescript(script, *arguments):
    for value in arguments:
        safe_text(value)
    result = subprocess.run(["/usr/bin/osascript", "-", *arguments], input=script, text=True, capture_output=True, timeout=30)
    require(result.returncode == 0, f"AppleScript failed: {result.stderr.strip()}")
    return result.stdout.rstrip("\n")


def window_presence(window):
    result = applescript(WINDOW_EXISTS, window)
    require(result in ("present", "missing"), f"Unexpected iTerm window presence: {result}")
    return result


def capture_window_diagnostics(prefix, include_ui=False):
    records = []
    try:
        target = Path(str(prefix) + ".png")
        result = subprocess.run(["/usr/sbin/screencapture", "-x", str(target)], capture_output=True, text=True, timeout=15)
        records.append({"capture": "screenshot", "path": str(target), "returncode": result.returncode, "bytes": target.stat().st_size if target.is_file() else 0, "stderr": result.stderr})
    except Exception as error:
        records.append({"capture": "screenshot", "error": str(error)})
    if include_ui:
        try:
            result = subprocess.run(["/usr/bin/osascript", "-"], input=UI_CENSUS, capture_output=True, text=True, timeout=10)
            Path(str(prefix) + "-ui.txt").write_text(result.stdout + result.stderr)
            records.append({"capture": "ui", "returncode": result.returncode, "stderr": result.stderr})
        except Exception as error:
            records.append({"capture": "ui", "error": str(error)})
    return records


def process_snapshot():
    output = subprocess.run(["/bin/ps", "-axo", "pid=,ppid=,tty=,lstart=,command="], text=True, capture_output=True, check=True, timeout=10).stdout
    pattern = re.compile(r"^\s*(\d+)\s+(\d+)\s+(\S+)\s+(\S+\s+\S+\s+\d+\s+\S+\s+\d+)\s+(.*)$")
    processes = {}
    for line in output.splitlines():
        match = pattern.match(line)
        if match:
            pid, parent, tty, started, command = match.groups()
            processes[int(pid)] = {"pid": int(pid), "ppid": int(parent), "tty": tty, "start": " ".join(started.split()), "command": command}
    require(processes, "macOS ps returned no parseable process identities")
    return processes


def executable(pid):
    library = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    library.proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
    library.proc_pidpath.restype = ctypes.c_int
    buffer = ctypes.create_string_buffer(4096)
    require(library.proc_pidpath(pid, buffer, len(buffer)) > 0, f"Cannot inspect executable for process {pid}")
    return os.path.realpath(os.fsdecode(buffer.value))


def same_process(identity):
    current = process_snapshot().get(identity["pid"])
    if not current or current["start"] != identity["start"]:
        return False
    try:
        current_executable = executable(identity["pid"])
    except RuntimeError:
        current = process_snapshot().get(identity["pid"])
        if not current or current["start"] != identity["start"]:
            return False
        raise
    require(current_executable == identity["executable"], f"Executable changed for owned process {identity['pid']}")
    return True


def iterm_owner():
    matches = [process for process in process_snapshot().values() if process["command"].split(" ", 1)[0] == "/Applications/iTerm.app/Contents/MacOS/iTerm2"]
    require(len(matches) == 1, "iTerm process identity is ambiguous")
    owner = matches[0]
    owner["executable"] = executable(owner["pid"])
    return owner


def native_window(owner, window):
    require(same_process(owner), "iTerm process identity changed")
    matches = [record for record in native_windows() if record["number"] == int(window)]
    require(len(matches) == 1 and matches[0]["owner_pid"] == owner["pid"], "Native window identity or owner changed")
    return matches[0]


def idle_editor(frame):
    lines = frame.splitlines()
    borders = [index for index, line in enumerate(lines) if len(line.strip()) >= 29 and set(line.strip()) == {"─"}]
    return len(borders) >= 2 and not "".join(lines[borders[-2] + 1:borders[-1]]).strip() and any(line.split() and line.split()[-1] == "parent" for line in lines)


class Terminal:
    def __init__(self, root, evidence, workspace, agent, pig_home, sessions, pig, extension, selected, port):
        self.root = root
        self.evidence = evidence
        self.workspace = workspace
        self.agent = agent
        self.pig_home = pig_home
        self.sessions = sessions
        self.pig = pig
        self.extension = extension
        self.selected = selected
        self.port = port
        self.generation = 0
        self.window = None
        self.owned = {}
        self.generations = []
        self.residues = []
        self.unsafe = False
        self.inputs = []

    def session_action(self, action, value=None):
        require(self.window is not None, "No owned iTerm window")
        arguments = [self.window["id"], self.window["session"], action]
        if value is not None:
            arguments.append(safe_text(value))
        return applescript(SESSION, *arguments)

    def observe(self):
        if not self.window or not self.window.get("wrapper_pid"):
            return
        snapshot = process_snapshot()
        root_pid = self.window["wrapper_pid"]
        root_identity = next((process for process in self.owned.values() if process["pid"] == root_pid), None)
        if root_identity and not same_process(root_identity):
            return
        descendants = {root_pid}
        while True:
            expanded = descendants | {pid for pid, p in snapshot.items() if p["ppid"] in descendants}
            if expanded == descendants:
                break
            descendants = expanded
        for pid in descendants:
            process = snapshot.get(pid)
            if not process:
                continue
            try:
                process["executable"] = executable(pid)
            except RuntimeError:
                current = process_snapshot().get(pid)
                require(not current or current["start"] != process["start"], f"Cannot identify owned process {pid}")
                continue
            key = f"{pid}/{process['start']}/{process['executable']}"
            self.owned[key] = process
        self.window["snapshot_count"] += 1

    def wait(self, predicate, label, timeout=90):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.observe()
            if predicate():
                return
            time.sleep(0.4)
        raise RuntimeError(f"Timed out waiting for {label}")

    def launch(self, session_path=None):
        self.generation += 1
        generation = self.root / f"generation-{self.generation}"
        generation.mkdir()
        identity = applescript(CREATE).splitlines()
        require(len(identity) == 3 and identity[0] and re.fullmatch(r"/dev/ttys\d+", identity[1]) and re.fullmatch(r"\d+", identity[2]), "iTerm session identity is incomplete")
        self.window = {"session": identity[0], "tty": identity[1], "id": identity[2], "snapshot_count": 0}
        self.generations.append(self.window)
        environment = {
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "HOME": str(self.root / "home"), "SHELL": "/bin/sh", "LANG": "en_US.UTF-8", "TERM": "xterm-256color",
            "TMPDIR": str(self.root / "tmp"), "GOTMPDIR": str(self.root / "tmp"), "XDG_CACHE_HOME": str(self.root / "cache"), "XDG_RUNTIME_DIR": str(self.root / "tmp"),
            "PIG_HOME": str(self.pig_home), "PIG_CODING_AGENT_DIR": str(self.agent), "PIG_USE_PI_DIRS": "0", "PIG_OFFLINE": "1", "PI_OFFLINE": "1", "PI_TELEMETRY": "0", "PI_SKIP_VERSION_CHECK": "1",
        }
        args = [str(self.pig), "--no-extensions"]
        if self.selected:
            args += ["-e", str(self.extension)]
        args += ["--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", str(self.sessions), "--approve", "--model", "fixture/parent"]
        if session_path:
            args += ["--session", str(session_path)]
        command = ["/usr/bin/env", "-i", *[f"{k}={v}" for k, v in environment.items()], *args]
        for argument in command:
            safe_text(argument)
        wrapper = generation / "launch.sh"
        wrapper.write_text("#!/bin/sh\ncd " + shlex.quote(safe_text(str(self.workspace))) + " || exit 125\nprintf '%s\\n' \"$$\" > " + shlex.quote(str(generation / "wrapper.pid")) + "\n" + shlex.join(command) + "\nresult=$?\nprintf '%s\\n' \"$result\" > " + shlex.quote(str(generation / "exit.txt")) + "\nexit \"$result\"\n")
        self.window["launch_sent"] = True
        self.session_action("send", "exec /bin/sh " + shlex.quote(str(wrapper)))
        self.wait(lambda: (generation / "wrapper.pid").exists(), "owned terminal wrapper", 30)
        self.window["wrapper_pid"] = int((generation / "wrapper.pid").read_text().strip())
        self.wait(lambda: idle_editor(self.session_action("text")), "PiG editor", 90)
        self.observe()
        tty = self.window["tty"].removeprefix("/dev/")
        wrappers = [p for p in self.owned.values() if p["pid"] == self.window["wrapper_pid"] and p["tty"] == tty]
        hosts = [p for p in self.owned.values() if p["ppid"] == self.window["wrapper_pid"] and p["executable"] == os.path.realpath(self.pig) and p["tty"] == tty]
        extensions = [p for p in self.owned.values() if p["executable"] == os.path.realpath(self.extension)]
        require(wrappers and hosts, "PiG process is not attributed to the iTerm TTY and wrapper")
        self.window["wrapper_identity"] = wrappers[-1]
        if self.selected:
            require(extensions, "Selected extension process was not observed under the owned wrapper")
        else:
            require(not extensions, "Unselected session started the extension")
        self.window["host"] = hosts[-1]
        owner = iterm_owner()
        native = native_window(owner, self.window["id"])
        alternate = applescript(OWNED_ALTERNATE, self.window["id"], self.window["session"])
        require(alternate and alternate != "missing value" and "\n" not in alternate, "Alternate window identity is unavailable")
        self.window["ownership"] = {"iterm": owner, "native_before": native, "alternate_id": alternate}
        self.window["extension_seen"] = bool(extensions)
        self.window["generation_dir"] = str(generation)
        self.frame(f"startup-{self.generation}")

    def frame(self, name):
        frame = self.session_action("text")
        (self.evidence / f"{name}.txt").write_text(frame + "\n")
        return frame

    def send(self, message):
        safe_text(message)
        require(idle_editor(self.session_action("text")), "PiG editor was not ready for input")
        self.session_action("send", message)
        self.inputs.append({"input": message, "generation": self.generation, "time": time.time()})

    def await_marker(self, marker, fixture):
        def ready():
            require(not fixture.errors, f"Fixture provider failed: {fixture.errors}")
            frame = self.session_action("text")
            return marker in frame and idle_editor(frame) and native_marker(self.sessions, marker)
        self.wait(ready, marker, 120)
        self.frame(marker.lower())
        target = self.evidence / f"{marker.lower()}.png"
        capture = subprocess.run(["/usr/sbin/screencapture", "-x", str(target)], capture_output=True, text=True, timeout=30)
        if capture.returncode != 0 or not target.exists() or target.stat().st_size == 0:
            (self.evidence / f"{marker.lower()}-capture-blocked.txt").write_text(f"screencapture exit={capture.returncode}\n{capture.stderr}\n")
            raise RuntimeError(f"Screenshot unavailable for {marker}")

    def exit_host(self):
        require(idle_editor(self.session_action("text")), "PiG editor was not idle before exit")
        self.session_action("exit")
        generation = Path(self.window["generation_dir"])
        self.wait(lambda: (generation / "exit.txt").exists(), "PiG exit status", 30)
        exit_status = int((generation / "exit.txt").read_text().strip())
        self.window["exit_status"] = exit_status
        require(exit_status == 0, f"PiG exited with status {exit_status}")
        self.wait(lambda: not any(same_process(process) for process in self.owned.values()), "owned processes to exit", 30)
        self.close_window()

    def capture_closure(self, name, include_ui=False):
        prefix = self.evidence / f"closure-{self.generation}-{name}"
        self.window.setdefault("closure_captures", []).extend(capture_window_diagnostics(prefix, include_ui))

    def close_window(self):
        if self.window is None:
            return
        try:
            wrapper = self.window.get("wrapper_identity")
            if self.window.get("launch_sent"):
                require(wrapper is not None and wrapper in self.owned.values(), "Owned wrapper process identity was not observed")
            if wrapper is not None:
                require(not any(same_process(process) for process in self.owned.values()), "Owned processes are still running")
                self.window["process_exit_observed"] = {"time": time.time(), "wrapper": wrapper, "owned_count": len(self.owned)}
            self.window["close_result"] = self.session_action("close")
            self.window.setdefault("close_results", []).append({"time": time.time(), "result": self.window["close_result"]})
            require(self.window["close_result"] in ("closed", "already closed", "session missing"), "Unexpected iTerm close result")
            if self.window["close_result"] == "session missing" and self.window.get("ownership"):
                presence = window_presence(self.window["id"])
                self.window.setdefault("presence_results", []).append({"time": time.time(), "result": presence})
                if presence == "present":
                    host = self.window.get("host")
                    require(host is not None and host in self.owned.values(), "Owned PiG process identity was not observed")
                    ownership = self.window["ownership"]
                    native = native_window(ownership["iterm"], self.window["id"])
                    observation = applescript(VERIFY_RESIDUE, self.window["id"], self.window["session"], ownership["alternate_id"])
                    require(observation.splitlines() and observation.splitlines()[0] == "verified", "Retained window state is ambiguous")
                    self.window["fixture_teardown"] = {"status": "UNRELEASED FIXTURE RESIDUE", "time": time.time(), "native": native, "observation": observation}
                    self.capture_closure("unreleased-residue", include_ui=True)
                    self.residues.append(self.window)
                    self.window = None
                    return
            captured = False

            def closed():
                nonlocal captured
                presence = window_presence(self.window["id"])
                self.window.setdefault("presence_results", []).append({"time": time.time(), "result": presence})
                if presence == "missing":
                    return True
                if wrapper is not None:
                    if not captured:
                        self.capture_closure("before-warning")
                        captured = True
                    warning = applescript(ACKNOWLEDGE_WARNING, self.window["id"], self.window["session"])
                    self.window.setdefault("warning_results", []).append({"time": time.time(), "result": warning})
                    require(warning != "owned session changed", "Owned window session changed.")
                    if warning.startswith("acknowledged owned short-session warning"):
                        self.capture_closure("after-warning")
                return False

            self.wait(closed, "owned iTerm window to close", 30)
        except Exception as error:
            self.unsafe = True
            self.window["closure_error"] = str(error)
            self.capture_closure("failure", include_ui=True)
            raise
        self.window = None

    def cleanup(self):
        problems = []
        try:
            self.observe()
        except Exception as error:
            problems.append(str(error))
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and any(same_process(p) for p in self.owned.values()):
            time.sleep(0.2)
        survivors = [p for p in self.owned.values() if same_process(p)]
        for process in reversed(survivors):
            if same_process(process):
                try:
                    os.kill(process["pid"], signal.SIGTERM)
                except ProcessLookupError:
                    pass
        if survivors:
            problems.append("Owned processes required forced termination")
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and any(same_process(p) for p in self.owned.values()):
            time.sleep(0.2)
        remaining = [p for p in self.owned.values() if same_process(p)]
        if remaining:
            problems.append("Owned processes survived cleanup")
        if self.window and not remaining:
            try:
                self.close_window()
            except Exception as error:
                problems.append(str(error))
        (self.evidence / "terminal.json").write_text(json.dumps({"generations": self.generations, "inputs": self.inputs, "observed_processes": list(self.owned.values()), "survivors": remaining, "cleanup_errors": problems, "unreleased_fixture_residues": self.residues}, indent=2) + "\n")
        require(not problems, "; ".join(problems))


def native_marker(sessions, marker):
    for path in sessions.rglob("*.jsonl"):
        for line in path.read_text().splitlines():
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue
            message = entry.get("message", {})
            if entry.get("type") == "message" and message.get("role") == "assistant" and message.get("stopReason") == "stop" and marker in text_content(message.get("content")):
                return True
    return False


def save_native(root, evidence):
    evidence.mkdir(parents=True, exist_ok=True)
    destination = evidence / "native"
    destination.mkdir(exist_ok=True)
    manifest = []
    for source in (root / "sessions", root / "agent" / "sessions", root / "agent" / "pig-litter" / "roots"):
        if not source.exists():
            continue
        paths = list(source.rglob("*.jsonl")) + list(source.rglob("team.json"))
        for path in paths:
            require(path.is_file() and not path.is_symlink(), "Native evidence is not a regular owned file")
            relative = path.relative_to(root)
            target = destination / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, target)
            manifest.append({"source": str(path), "artifact": str(target.relative_to(evidence)), "sha256": hashlib.sha256(target.read_bytes()).hexdigest()})
    (evidence / "native-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    return manifest


def native_ids(root):
    found = set()
    for source in (root / "sessions", root / "agent" / "sessions"):
        if not source.exists():
            continue
        for path in source.rglob("*.jsonl"):
            for line in path.read_text().splitlines():
                entry = json.loads(line)
                if entry.get("type") == "session" and entry.get("id"):
                    found.add(entry["id"])
    return found


def fixture_files(root, port):
    for name in ("home", "pig-home", "agent", "sessions", "workspace", "workspace/.pig", "tmp", "cache"):
        (root / name).mkdir(parents=True, exist_ok=True, mode=0o700)
    models = ["parent", "team_alpha", "team_beta", "team_gamma", "policy_member", "policy_duplicate", "policy_ordinary"]
    config = {"providers": {"fixture": {"api": "openai-completions", "baseUrl": f"http://127.0.0.1:{port}/v1", "apiKey": "local-fixture-only", "models": [{"id": name, "name": name, "contextWindow": 32000, "maxTokens": 2048} for name in models]}}}
    (root / "agent" / "models.json").write_text(json.dumps(config))
    (root / "agent" / "auth.json").write_text(json.dumps({"fixture": {"type": "api_key", "key": "local-fixture-only"}}))
    for path in (root / "agent" / "trust.json", root / "agent" / "settings.json", root / "workspace" / ".pig" / "settings.json", root / "workspace" / ".pig" / "trust.json"):
        path.write_text("{}")
    (root / "workspace" / "litter.yaml").write_text("concurrency: 6\ndepth: 2\n")


def probe_identity(path):
    if not path.exists():
        return None
    result = json.loads(path.read_text())
    window, session = result.get("window_id", ""), result.get("session_id", "")
    if not window or not session:
        return None
    require(re.fullmatch(r"\d+", window), "Capability probe window identity is invalid")
    return window, session


def close_probe(path, evidence, identity=None):
    if identity is None:
        identity = probe_identity(path)
    if identity is None:
        (evidence / "probe-cleanup.txt").write_text("No attributable capability window was available.\n")
        return False
    observations = []
    try:
        closed = applescript(SESSION, *identity, "close")
        observations.append(closed)
        require(closed in ("closed", "already closed", "session missing"), "Unexpected capability window close result")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            presence = window_presence(identity[0])
            observations.append(presence)
            if presence == "missing":
                (evidence / "probe-cleanup.txt").write_text("\n".join(observations) + "\n")
                return True
            time.sleep(0.4)
    except Exception as error:
        observations.append(f"error: {error}")
        (evidence / "probe-cleanup.txt").write_text("\n".join(observations) + "\n")
        captures = capture_window_diagnostics(evidence / "probe-cleanup-failure", include_ui=True)
        (evidence / "probe-cleanup-diagnostics.json").write_text(json.dumps(captures, indent=2) + "\n")
        raise
    (evidence / "probe-cleanup.txt").write_text("\n".join(observations) + "\n")
    captures = capture_window_diagnostics(evidence / "probe-cleanup-failure", include_ui=True)
    (evidence / "probe-cleanup-diagnostics.json").write_text(json.dumps(captures, indent=2) + "\n")
    return False


def run_case(name, scratch, evidence, pig, extension):
    root = scratch / name
    root.mkdir(mode=0o700)
    evidence.mkdir(parents=True, mode=0o700)
    requests = evidence / "provider-requests"
    requests.mkdir()
    fixture = Fixture(name, root / "workspace", requests)
    server, thread = start_provider(fixture)
    fixture_files(root, server.server_address[1])
    terminal = Terminal(root, evidence, root / "workspace", root / "agent", root / "pig-home", root / "sessions", pig, extension, name != "unselected", server.server_address[1])
    product_problems = []
    teardown_problems = []
    try:
        terminal.launch()
        if name == "iterm":
            fixture.extension = str(extension)
            fixture.lead = terminal.window.copy()
            sibling_id, sibling_tty = applescript(SIBLING, fixture.lead["id"], fixture.lead["session"], "create").split("\t")
            deadline = time.monotonic() + 10
            sibling_matches = []
            while time.monotonic() < deadline and not sibling_matches:
                sibling_matches = [process for process in process_snapshot().values() if process["tty"] == sibling_tty.removeprefix("/dev/") and process["command"] == "/bin/sleep 600"]
                if not sibling_matches:
                    time.sleep(0.1)
            require(len(sibling_matches) == 1, "Fixture sibling process is missing or ambiguous")
            sibling = sibling_matches[0]
            sibling["executable"] = executable(sibling["pid"])
            sibling.update({"session": sibling_id, "terminal": sibling_tty})
            fixture.observations["sibling"] = sibling
            terminal.send("P08_ITERM")
            terminal.await_marker("P08_ITERM_DONE", fixture)
            require(fixture.stage == 3, "iTerm stage did not complete")
            member = fixture.observations["native_member"]
            terminal.wait(lambda: not same_process(member) and applescript(PANE_BY_TTY, member["terminal"]) == "missing", "native member process and pane cleanup", 30)
            probe = probe_identity(Path(os.environ["RUNNER_TEMP"]) / "collaboration-platform-probe" / "result.json")
            require(probe and applescript(SESSION, *probe, "exists") == "present", "Unrelated probe window was closed")
            require(same_process(sibling) and applescript(PANE_BY_TTY, sibling_tty) == fixture.lead["id"] + "\t" + sibling_id, "Unrelated sibling pane changed")
            require(applescript(SIBLING, fixture.lead["id"], sibling_id, "close", sibling_tty) == "closed", "Fixture sibling pane did not close")
            terminal.wait(lambda: not same_process(sibling), "fixture sibling process cleanup", 30)
            fixture.observations["cleanup"] = "native process and pane exited; unrelated sibling and probe remained until fixture cleanup"
        elif name == "team":
            terminal.send("P03_TEAM_FIRST")
            terminal.await_marker("P03_TEAM_FIRST_DONE", fixture)
            require(fixture.stage == 6, "Team first stage did not complete")
            save_native(root, evidence / "before-reopen")
            session_files = list((root / "sessions").rglob("*.jsonl"))
            require(len(session_files) == 1, "The root session path is ambiguous")
            before_requests = fixture.requests
            member_requests = {name: fixture.request_models.get(f"team_{name}", 0) for name in NAMES}
            terminal.exit_host()
            terminal.launch(session_files[0])
            require(fixture.requests == before_requests, "Reopen started an unsolicited provider request")
            terminal.send("P03_TEAM_RESTART")
            terminal.await_marker("P03_TEAM_REOPENED", fixture)
            require(fixture.stage == 9, "Reopened team stage did not complete")
            require(member_requests == {name: fixture.request_models.get(f"team_{name}", 0) for name in NAMES}, "Reopen restarted a teammate")
        elif name == "policy":
            terminal.send("P03_POLICY")
            terminal.await_marker("P03_POLICY_DONE", fixture)
            require(fixture.stage == 8, "Policy stage did not complete")
        else:
            terminal.send("P03_UNSELECTED")
            terminal.await_marker("P03_UNSELECTED_DONE", fixture)
            frame = terminal.session_action("text")
            require("/pig-litter" not in frame and "Litter " not in frame, "Unselected session displayed Pig Litter")
            require(not (root / "agent" / "pig-litter").exists(), "Unselected session wrote Pig Litter state")
        terminal.exit_host()
        observed_ids = native_ids(root)
        if fixture.refs:
            expected_ids = {ref["id"] for ref in fixture.refs.values()} | {next(iter(fixture.refs.values()))["team"]["root"]}
            require(expected_ids <= observed_ids, "Native root or member sessions are missing")
        require(not fixture.errors, f"Provider failed: {fixture.errors}")
    except Exception as error:
        product_problems.append(str(error))
        try:
            target = evidence / "failure.png"
            subprocess.run(["/usr/sbin/screencapture", "-x", str(target)], timeout=30, check=True, capture_output=True)
            require(target.is_file() and target.stat().st_size > 0, "Failure screenshot was empty")
        except Exception as capture_error:
            product_problems.append(f"Failure screenshot: {capture_error}")
        if terminal.window:
            try:
                terminal.frame("failure")
            except Exception as capture_error:
                product_problems.append(f"Failure capture: {capture_error}")
    finally:
        with fixture.changed:
            fixture.ordinary_release = True
            fixture.changed.notify_all()
        try:
            terminal.cleanup()
        except Exception as error:
            terminal.unsafe = True
            teardown_problems.append(str(error))
            product_problems.append(f"Fixture state could not be verified: {error}")
        for window in terminal.residues:
            teardown_problems.append(f"UNRELEASED FIXTURE RESIDUE: window {window['id']}")
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        if thread.is_alive():
            product_problems.append("Provider did not stop")
            teardown_problems.append("Provider did not stop")
        if fixture.errors and not any("Provider failed:" in problem for problem in product_problems):
            product_problems.append(f"Provider failed: {fixture.errors}")
        try:
            manifest = save_native(root, evidence)
            if not manifest:
                product_problems.append("No native sessions were retained")
        except Exception as error:
            product_problems.append(f"Native evidence: {error}")
        receipt = {"scenario": name, "status": "failed" if product_problems or teardown_problems else "pass", "product_status": "failed" if product_problems else "pass", "product_problems": product_problems, "teardown_status": "failed" if teardown_problems else "pass", "teardown_problems": teardown_problems, "safe_to_continue": not terminal.unsafe and terminal.window is None and not thread.is_alive(), "problems": product_problems + teardown_problems, "stage": fixture.stage, "refs": fixture.refs, "member_rosters": fixture.rosters, "observations": fixture.observations, "provider_errors": fixture.errors, "requests": fixture.requests, "request_models": fixture.request_models, "provider_joined": not thread.is_alive()}
        (evidence / "result.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--scenario", choices=("p03-all", "p03-team", "p03-policy", "p03-unselected", "p08-iterm"))
    parser.add_argument("--pig", type=Path)
    parser.add_argument("--extension", type=Path)
    parser.add_argument("--scratch", type=Path)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--cleanup-probe", action="store_true")
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    probe = Path(os.environ["RUNNER_TEMP"]) / "collaboration-platform-probe" / "result.json"
    if args.cleanup_probe:
        require(close_probe(probe, args.evidence), "The capability probe window could not be released")
        return
    require(all((args.scenario, args.pig, args.extension, args.scratch)), "Missing product verification argument")
    require(sys.platform == "darwin", "P03 iTerm verification requires macOS")
    capability = json.loads(probe.read_text())
    require(capability.get("status") == "pass" and capability.get("signature_status") == "verified" and capability.get("automation_status") == "approved", "Signed iTerm capability probe did not pass")
    identity = probe_identity(probe)
    require(identity is not None and applescript(SESSION, *identity, "exists") == "present", "The capability probe window is unavailable")
    cases = ("team", "policy", "unselected") if args.scenario == "p03-all" else (args.scenario.removeprefix("p03-").removeprefix("p08-"),)
    product_failures = []
    teardown_failures = []
    attempted = []
    for name in cases:
        attempted.append(name)
        try:
            receipt = run_case(name, args.scratch, args.evidence / name, args.pig, args.extension)
            if receipt and receipt["product_status"] != "pass":
                product_failures.append(f"{name}: {'; '.join(receipt['product_problems'])}")
            if receipt and receipt["teardown_status"] != "pass":
                teardown_failures.append(f"{name}: {'; '.join(receipt['teardown_problems'])}")
            if receipt and not receipt["safe_to_continue"]:
                product_failures.append(f"Stopped after {name}: fixture state is unsafe or ambiguous")
                if receipt["teardown_status"] == "pass":
                    teardown_failures.append(f"{name}: fixture state is unsafe or ambiguous")
                break
        except Exception as error:
            product_failures.append(str(error))
            teardown_failures.append(f"{name}: fixture outcome unavailable")
            break
    try:
        require(close_probe(probe, args.evidence, identity), "The capability probe window could not be released")
    except Exception as error:
        teardown_failures.append(f"Capability probe cleanup: {error}")
    failures = product_failures + teardown_failures
    summary = {"cases": list(cases), "attempted_cases": attempted, "not_run": [name for name in cases if name not in attempted], "product": {"status": "failed" if product_failures else "pass", "failures": product_failures}, "teardown": {"status": "failed" if teardown_failures else "pass", "failures": teardown_failures}, "failures": failures, "result": "failed" if failures else "pass"}
    (args.evidence / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    (args.evidence / "product-exit.txt").write_text("1\n" if product_failures else "0\n")
    if not teardown_failures:
        (args.evidence / "cleanup-ok").write_text("All owned windows, processes, and providers were released.\n")
    require(not failures, "; ".join(failures))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"P03 macOS verification failed: {error}", file=sys.stderr)
        sys.exit(1)
