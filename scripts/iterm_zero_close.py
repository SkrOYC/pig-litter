import time


OWNED_ALTERNATE = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  set wantedAlternate to missing value
  set numericMatches to 0
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        set numericMatches to numericMatches + 1
        set currentRef to current session of candidateWindow
        if currentRef is missing value then error "Owned session is missing."
        if (unique id of currentRef) is not wantedSession then error "Owned session changed."
        set wantedAlternate to alternate identifier of candidateWindow as text
      end if
    end repeat
    if numericMatches is not 1 or wantedAlternate is missing value or wantedAlternate is "" then error "Window identity is ambiguous."
    set alternateMatches to 0
    repeat with candidateWindow in windows
      if (alternate identifier of candidateWindow as text) is wantedAlternate then set alternateMatches to alternateMatches + 1
    end repeat
    if alternateMatches is not 1 then error "Alternate window identity is ambiguous."
    return wantedAlternate
  end tell
end run
'''


ZERO_WINDOW_ACTION = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedAlternate to item 2 of argv
  set wantedNonce to item 3 of argv
  set actionName to item 4 of argv
  set expectedTitle to item 5 of argv
  set numericMatches to 0
  set alternateMatches to 0
  set nonceMatches to 0
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then set numericMatches to numericMatches + 1
      if (alternate identifier of candidateWindow as text) is wantedAlternate then set alternateMatches to alternateMatches + 1
      if (name of candidateWindow as text) contains wantedNonce then set nonceMatches to nonceMatches + 1
    end repeat
    if numericMatches is not 1 or alternateMatches is not 1 or nonceMatches is not 1 then return "refused: ambiguous identity"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        if (alternate identifier of candidateWindow as text) is not wantedAlternate then return "refused: alternate identity changed"
        set windowTitle to name of candidateWindow as text
        if windowTitle does not start with wantedNonce then return "refused: nonce missing"
        if (count of tabs of candidateWindow) is not 0 then return "refused: window has tabs"
        if (current session of candidateWindow) is not missing value then return "refused: current session exists"
        set isVisible to visible of candidateWindow
        set isCloseable to closeable of candidateWindow
        if actionName is "inspect" then return "eligible" & linefeed & windowTitle & linefeed & isVisible & linefeed & isCloseable
        if actionName is not "close" then return "refused: unknown action"
        if windowTitle is not expectedTitle then return "refused: title changed"
        if isCloseable is not true then return "refused: window is not closeable"
        tell candidateWindow to close
        return "close requested"
      end if
    end repeat
    return "refused: window missing"
  end tell
end run
'''


def capture_alternate(diagnostic, driver, target):
    value = diagnostic.action("owned-zero.original-alternate", OWNED_ALTERNATE, target["window"], target["session"])
    if not value or value.strip() in ("", "missing value") or "\n" in value:
        raise RuntimeError("The original alternate window identifier was not verified")
    driver.safe_text(value)
    return value


def probe(diagnostic, driver, native_windows, result, control, seconds=30):
    target = result.get("target")
    nonce = result.get("nonce", "")
    alternate = result.get("alternate_id", "")
    processes = result.get("processes", [])
    host = result.get("host")
    if result.get("error") or result.get("state") != "present:missing" or result.get("exit_status") != 0 or result.get("processes_exited") is not True or result.get("requests") != 0 or not result.get("provider_joined") or result.get("provider_errors") or not target or not nonce or not alternate or len(processes) < 2 or target.get("process") not in processes or host not in processes or host.get("ppid") != target["process"]["pid"]:
        raise RuntimeError("The owned zero-tab target identity is incomplete")
    if target["window"] == control["window"] or target["session"] == control["session"] or target["tty"] == control["tty"]:
        raise RuntimeError("The target and control identities overlap")
    if any(driver.same_process(process) for process in processes):
        raise RuntimeError("An original PiG process is still alive")
    diagnostic.check_control("owned-zero.control-before", control)
    before = diagnostic.snapshot("startup-only-before-owned-close")
    target_id = int(target["window"])
    control_id = int(control["window"])
    for window_id in (target_id, control_id):
        original = [record for record in result.get("native_before", []) if record["number"] == window_id]
        if len(original) != 1 or original[0]["owner_pid"] != diagnostic.iterm["pid"]:
            raise RuntimeError("The original native window mapping was not verified")
    for window_id in (target_id, control_id):
        matches = [record for record in before if record["number"] == window_id]
        if len(matches) != 1 or matches[0]["owner_pid"] != diagnostic.iterm["pid"]:
            raise RuntimeError("Native window identity or owner changed")
    observed = diagnostic.action("owned-zero.inspect", ZERO_WINDOW_ACTION, target["window"], alternate, nonce, "inspect", "")
    fields = (observed or "").splitlines()
    if len(fields) != 4 or fields[0] != "eligible" or not (fields[1] == nonce or fields[1].startswith(nonce + " (")) or fields[2] not in ("true", "false") or fields[3] != "true":
        raise RuntimeError("The zero-tab target did not pass its identity and closeability checks")
    driver.safe_text(fields[1])
    diagnostic.event("owned-zero.verified", window=target["window"], alternate=alternate, nonce=nonce, title=fields[1], visible=fields[2], closeable=fields[3])
    current = driver.process_snapshot().get(diagnostic.iterm["pid"])
    if not current or current["start"] != diagnostic.iterm["start"] or any(driver.same_process(process) for process in processes):
        raise RuntimeError("The original process identities changed before close")
    diagnostic.check_control("owned-zero.control-before-action", control)
    current_native = native_windows()
    for window_id in (target_id, control_id):
        matches = [record for record in current_native if record["number"] == window_id]
        if len(matches) != 1 or matches[0]["owner_pid"] != diagnostic.iterm["pid"]:
            raise RuntimeError("Native window identity or owner changed before close")
    response = diagnostic.action("owned-zero.close", ZERO_WINDOW_ACTION, target["window"], alternate, nonce, "close", fields[1])
    if response != "close requested":
        raise RuntimeError(f"The exact owned window close was not requested: {response!r}")
    outcome = {"response": response, "alternate_id": alternate, "nonce": nonce, "title": fields[1], "visible": fields[2], "closeable": fields[3], "state": None, "native_target": None, "native_control": None}
    deadline = time.monotonic() + seconds
    while True:
        current = driver.process_snapshot().get(diagnostic.iterm["pid"])
        if not current or current["start"] != diagnostic.iterm["start"]:
            raise RuntimeError("The iTerm owner identity changed after the close request")
        if any(driver.same_process(process) for process in processes):
            raise RuntimeError("An original PiG process reappeared after the close request")
        diagnostic.check_control("owned-zero.control-poll", control)
        state = diagnostic.state("owned-zero.target-poll", target)
        if state == "present:changed":
            raise RuntimeError("The target acquired another session")
        records = native_windows()
        target_records = [record for record in records if record["number"] == target_id]
        control_records = [record for record in records if record["number"] == control_id]
        if len(control_records) != 1 or control_records[0]["owner_pid"] != diagnostic.iterm["pid"]:
            raise RuntimeError("The live control lost its native mapping")
        if len(target_records) > 1 or any(record["owner_pid"] != diagnostic.iterm["pid"] for record in target_records):
            raise RuntimeError("The target native identity became ambiguous")
        outcome.update({"state": state, "native_target": target_records, "native_control": control_records})
        diagnostic.event("owned-zero.poll", **outcome)
        if (state == "missing" and not target_records) or time.monotonic() >= deadline:
            break
        time.sleep(0.5)
    diagnostic.snapshot("startup-only-after-owned-close")
    diagnostic.check_control("owned-zero.control-after", control)
    return outcome
