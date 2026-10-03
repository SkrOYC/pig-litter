#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../../../.." && pwd -P)"
mode="${1:-run}"

doctor() {
	local pig_path pig_version go_version tmux_path tmux_version
	for command_name in pig go tmux jq ps; do
		if ! command -v "$command_name" >/dev/null 2>&1; then
			printf 'missing required command: %s\n' "$command_name" >&2
			return 1
		fi
	done

	pig_path="$(command -v pig)"
	case "$pig_path" in
		*-pig-0.3.1/bin/pig) ;;
		*) printf 'expected pinned PiG 0.3.1, got %s\n' "$pig_path" >&2; return 1 ;;
	esac
	pig_version="$(pig --version)"
	if [[ "$pig_version" != "0.3.1+0.87.1" ]]; then
		printf 'expected PiG 0.3.1+0.87.1, got %s\n' "$pig_version" >&2
		return 1
	fi

	go_version="$(go env GOVERSION)"
	if [[ "$go_version" != "go1.27.1" ]]; then
		printf 'expected Go 1.27.1, got %s\n' "$go_version" >&2
		return 1
	fi

	tmux_path="$(command -v tmux)"
	case "$tmux_path" in
		*-tmux-3.7c/bin/tmux) ;;
		*) printf 'expected pinned tmux 3.7c, got %s\n' "$tmux_path" >&2; return 1 ;;
	esac
	tmux_version="$(tmux -V)"
	if [[ "$tmux_version" != "tmux 3.7c" ]]; then
		printf 'expected tmux 3.7c, got %s\n' "$tmux_version" >&2
		return 1
	fi

	for path in "$repo_root/extensions/pig-litter/extension.go" "$repo_root/piglet.yaml"; do
		if [[ ! -f "$path" ]]; then
			printf 'required file is missing: %s\n' "$path" >&2
			return 1
		fi
	done

	if [[ -e "$repo_root/.pig" ]]; then
		printf 'refusing to drive a repository with existing .pig project state: %s/.pig\n' "$repo_root" >&2
		return 1
	fi

	printf 'PiG %s (%s), Go %s, tmux %s (%s); extension and Piglet files are present\n' \
		"$pig_version" "$pig_path" "$go_version" "$tmux_version" "$tmux_path"
}

if [[ "$mode" == doctor ]]; then
	doctor
	exit
fi
if [[ "$mode" != run ]]; then
	printf 'usage: %s [run|doctor]\n' "$0" >&2
	exit 2
fi

if ! doctor_output="$(doctor)"; then
	exit 1
fi
pig_path="$(command -v pig)"
runtime_path="$PATH"
pig_version="$(pig --version)"
go_version="$(go env GOVERSION)"
go_path="$(command -v go)"
tmux_version="$(tmux -V)"
tmux_path="$(command -v tmux)"

run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
evidence_root="${PIG_LITTER_EVIDENCE_DIR:-$repo_root/.pstack/evidence/pig-litter}"
case "$evidence_root" in
	/*) ;;
	*) evidence_root="$repo_root/$evidence_root" ;;
esac
evidence_dir="$evidence_root/$run_id"
mkdir -p "$evidence_dir"
printf '%s\n' "$doctor_output" > "$evidence_dir/doctor.txt"

mkdir -p "$repo_root/.pstack"
run_dir="$(mktemp -d "/tmp/pig-litter-${run_id}.XXXXXX")"
tmp_pig_home="$run_dir/pig-home"
socket_path="$run_dir/tmux.sock"
workspace="$repo_root"
server_started=0
outcome=running
failure_phase=""
failure_reason=""
server_cleanup=not_started
scratch_removed=false
cleanup_killed_pids=0
project_state_untouched=false
declare -a tracked_pid_files=()
declare -A case_result=( [direct]=pending [piglet]=pending [plain]=pending )
declare -A trust_action=( [direct]=not_requested [piglet]=not_requested [plain]=not_requested )
declare -A pane_dead=( [direct]=unknown [piglet]=unknown [plain]=unknown )
declare -A exit_status=( [direct]=unknown [piglet]=unknown [plain]=unknown )
declare -A exit_signal=( [direct]=unknown [piglet]=unknown [plain]=unknown )
declare -A process_tree_reaped=( [direct]=false [piglet]=false [plain]=false )
declare -A process_tree_count=( [direct]=0 [piglet]=0 [plain]=0 )

tmux_do() {
	env -i "PATH=$runtime_path" "HOME=$HOME" "TMPDIR=$run_dir" "TERM=xterm-256color" \
		tmux -S "$socket_path" "$@"
}

process_tree_pids() {
	local root_pid="$1"
	local parent_pid child_pid index=0
	local -a queue=("$root_pid")
	local -A seen=()
	seen["$root_pid"]=1
	while (( index < ${#queue[@]} )); do
		parent_pid="${queue[$index]}"
		while read -r child_pid; do
			[[ -n "$child_pid" ]] || continue
			if [[ -z "${seen[$child_pid]:-}" ]]; then
				seen["$child_pid"]=1
				queue+=("$child_pid")
			fi
		done < <(ps -A -o pid= -o ppid= | awk -v parent="$parent_pid" '$2 == parent { print $1 }')
		index=$((index + 1))
	done
	printf '%s\n' "${queue[@]}"
}

track_process_tree() {
	local case_name="$1" stage="$2" session_name="$3"
	local pane_pid pane_command
	local pids_file="$run_dir/$case_name.pids"
	pane_pid="$(tmux_do display-message -p -t "$session_name" '#{pane_pid}')"
	pane_command="$(tmux_do display-message -p -t "$session_name" '#{pane_current_command}')"
	if [[ ! "$pane_pid" =~ ^[0-9]+$ ]] || ! kill -0 "$pane_pid" 2>/dev/null; then
		failure_phase="$case_name doctor"
		failure_reason="tmux pane process is not live"
		return 1
	fi
	printf 'pane_pid=%s\npane_command=%s\n' "$pane_pid" "$pane_command" > "$evidence_dir/$case_name.pane-$stage.txt"
	process_tree_pids "$pane_pid" > "$evidence_dir/$case_name.process-tree-$stage.pids.txt"
	while read -r pane_pid; do
		[[ -n "$pane_pid" ]] || continue
		ps -p "$pane_pid" -o pid= -o ppid= -o comm= -o args=
	done < "$evidence_dir/$case_name.process-tree-$stage.pids.txt" > "$evidence_dir/$case_name.process-tree-$stage.txt"
	cat "$evidence_dir/$case_name.process-tree-$stage.pids.txt" >> "$pids_file"
	sort -nu "$pids_file" -o "$pids_file"
	process_tree_count["$case_name"]="$(wc -l < "$pids_file" | tr -d ' ')"
}

capture_pane() {
	tmux_do capture-pane -p -S -200 -t "$1"
}

fail_run() {
	failure_phase="$1"
	failure_reason="$2"
	printf '%s: %s\n' "$failure_phase" "$failure_reason" >&2
	exit 1
}

reap_tracked_processes() {
	local pid_file pid
	local -a pids=()
	for pid_file in "${tracked_pid_files[@]}"; do
		[[ -f "$pid_file" ]] || continue
		while read -r pid; do
			[[ -n "$pid" ]] || continue
			pids+=("$pid")
		done < "$pid_file"
	done
	for pid in "${pids[@]}"; do
		if kill -0 "$pid" 2>/dev/null; then
			kill -TERM "$pid" 2>/dev/null || true
			cleanup_killed_pids=$((cleanup_killed_pids + 1))
		fi
	done
	local deadline=$((SECONDS + 2))
	local live=1
	while (( SECONDS < deadline )); do
		live=0
		for pid in "${pids[@]}"; do
			if kill -0 "$pid" 2>/dev/null; then
				live=1
				break
			fi
		done
		(( live == 0 )) && break
		sleep 0.1
	done
	if (( live == 1 )); then
		for pid in "${pids[@]}"; do
			if kill -0 "$pid" 2>/dev/null; then
				kill -KILL "$pid" 2>/dev/null || true
				cleanup_killed_pids=$((cleanup_killed_pids + 1))
			fi
		done
	fi
}

finish() {
	local original_status=$?
	local final_status=$original_status
	local tree_still_alive=false
	local case_name launch_pid launch_pid_file pids_file pid
	trap - EXIT INT TERM
	if (( original_status == 0 )); then
		outcome=passed
	elif [[ "$outcome" == running ]]; then
		outcome=failed
		[[ -n "$failure_reason" ]] || failure_reason="verification exited with status $original_status"
	fi
	for case_name in direct piglet plain; do
		launch_pid_file="$run_dir/$case_name.pid"
		pids_file="$run_dir/$case_name.pids"
		[[ -s "$launch_pid_file" ]] || continue
		launch_pid="$(cat "$launch_pid_file")"
		[[ "$launch_pid" =~ ^[0-9]+$ ]] || continue
		if kill -0 "$launch_pid" 2>/dev/null; then
			process_tree_pids "$launch_pid" > "$evidence_dir/$case_name.process-tree-before-cleanup.pids.txt"
			while read -r pid; do
				[[ -n "$pid" ]] || continue
				ps -p "$pid" -o pid= -o ppid= -o comm= -o args=
			done < "$evidence_dir/$case_name.process-tree-before-cleanup.pids.txt" \
				> "$evidence_dir/$case_name.process-tree-before-cleanup.txt"
			cat "$evidence_dir/$case_name.process-tree-before-cleanup.pids.txt" >> "$pids_file"
			sort -nu "$pids_file" -o "$pids_file"
			process_tree_count["$case_name"]="$(wc -l < "$pids_file" | tr -d ' ')"
		fi
	done
	if (( server_started == 1 )); then
		if tmux_do kill-server >/dev/null 2>&1; then
			server_cleanup=stopped
		else
			server_cleanup=already_stopped
		fi
	fi
	for pid_file in "${tracked_pid_files[@]}"; do
		[[ -f "$pid_file" ]] || continue
		while read -r pid; do
			[[ -n "$pid" ]] || continue
			if kill -0 "$pid" 2>/dev/null; then tree_still_alive=true; fi
		done < "$pid_file"
	done
	if [[ "$tree_still_alive" == true ]]; then
		reap_tracked_processes
		failure_reason="one or more PiG or extension processes survived the exit check"
		failure_phase=cleanup
		outcome=failed
		final_status=1
	fi
	if [[ -n "$run_dir" && -d "$run_dir" ]]; then
		if rm -rf "$run_dir"; then
			scratch_removed=true
		else
			scratch_removed=false
			failure_reason="could not remove the owned scratch directory"
			failure_phase=cleanup
			outcome=failed
			final_status=1
		fi
	fi
	if [[ "$scratch_removed" != true ]]; then
		outcome=failed
		final_status=1
	fi
	if [[ -e "$repo_root/.pig" ]]; then
		project_state_untouched=false
		if [[ "$outcome" == passed ]]; then
			failure_phase=cleanup
			failure_reason="PiG created project-local .pig state; it is preserved for inspection"
			outcome=failed
			final_status=1
		fi
	else
		project_state_untouched=true
	fi
	jq -n \
		--arg runId "$run_id" \
		--arg result "$outcome" \
		--arg failurePhase "$failure_phase" \
		--arg failureReason "$failure_reason" \
		--arg doctor "$doctor_result" \
		--arg pigPath "$pig_path" \
		--arg pigVersion "$pig_version" \
		--arg goPath "$go_path" \
		--arg goVersion "$go_version" \
		--arg tmuxPath "$tmux_path" \
		--arg tmuxVersion "$tmux_version" \
		--arg directResult "${case_result[direct]}" \
		--arg directTrust "${trust_action[direct]}" \
		--arg directDead "${pane_dead[direct]}" \
		--arg directStatus "${exit_status[direct]}" \
		--arg directSignal "${exit_signal[direct]}" \
		--arg directReaped "${process_tree_reaped[direct]}" \
		--arg directPids "${process_tree_count[direct]}" \
		--arg pigletResult "${case_result[piglet]}" \
		--arg pigletTrust "${trust_action[piglet]}" \
		--arg pigletDead "${pane_dead[piglet]}" \
		--arg pigletStatus "${exit_status[piglet]}" \
		--arg pigletSignal "${exit_signal[piglet]}" \
		--arg pigletReaped "${process_tree_reaped[piglet]}" \
		--arg pigletPids "${process_tree_count[piglet]}" \
		--arg plainResult "${case_result[plain]}" \
		--arg plainTrust "${trust_action[plain]}" \
		--arg plainDead "${pane_dead[plain]}" \
		--arg plainStatus "${exit_status[plain]}" \
		--arg plainSignal "${exit_signal[plain]}" \
		--arg plainReaped "${process_tree_reaped[plain]}" \
		--arg plainPids "${process_tree_count[plain]}" \
		--arg serverCleanup "$server_cleanup" \
		--arg scratchRemoved "$scratch_removed" \
		--arg cleanupKilledPids "$cleanup_killed_pids" \
		--arg projectStateUntouched "$project_state_untouched" \
		--arg evidenceDir "$evidence_dir" \
		'{
		runId: $runId,
		result: $result,
		failure: {phase: $failurePhase, reason: $failureReason},
		doctor: $doctor,
		runtime: {
			pig: {path: $pigPath, version: $pigVersion},
			go: {path: $goPath, version: $goVersion},
			tmux: {path: $tmuxPath, version: $tmuxVersion}
		},
		network: "offline",
		providerCredentialsInjected: false,
		trustScope: "session only when prompted",
		projectStateUntouched: ($projectStateUntouched == "true"),
		launchArguments: {
			direct: [$pigPath, "--offline", "--no-session", "-e", "./extensions/pig-litter"],
			piglet: [$pigPath, "--offline", "--no-session", "--piglet", "./piglet.yaml"],
			plain: [$pigPath, "--offline", "--no-session"]
		},
		cases: {
			direct: {result: $directResult, trustAction: $directTrust, paneDead: ($directDead == "1"), exitStatus: $directStatus, exitSignal: $directSignal, processTreeReaped: ($directReaped == "true"), trackedPids: ($directPids | tonumber)},
			piglet: {result: $pigletResult, trustAction: $pigletTrust, paneDead: ($pigletDead == "1"), exitStatus: $pigletStatus, exitSignal: $pigletSignal, processTreeReaped: ($pigletReaped == "true"), trackedPids: ($pigletPids | tonumber)},
			plain: {result: $plainResult, trustAction: $plainTrust, paneDead: ($plainDead == "1"), exitStatus: $plainStatus, exitSignal: $plainSignal, processTreeReaped: ($plainReaped == "true"), trackedPids: ($plainPids | tonumber)}
		},
		cleanup: {tmuxServer: $serverCleanup, scratchRemoved: ($scratchRemoved == "true"), ownedPidsTerminated: ($cleanupKilledPids | tonumber)},
		evidenceDir: $evidenceDir
	}' \
		> "$evidence_dir/run.json"
	printf 'result=%s\nevidence=%s\n' "$outcome" "$evidence_dir"
	exit "$final_status"
}

doctor_result=passed
tracked_pid_files=("$run_dir/direct.pids" "$run_dir/piglet.pids" "$run_dir/plain.pids")
for pid_file in "${tracked_pid_files[@]}"; do : > "$pid_file"; done
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$tmp_pig_home/agent/sessions" "$run_dir/go-cache" "$run_dir/go-mod-cache"
printf 'set-option -g remain-on-exit on\nset-option -g exit-empty off\n' > "$run_dir/tmux.conf"
cat > "$run_dir/launch.sh" <<'LAUNCH'
#!/usr/bin/env bash
set -euo pipefail
pid_file="$1"
temp_root="$2"
pig_home="$3"
go_cache="$4"
go_mod_cache="$5"
workspace="$6"
pig_path="$7"
runtime_path="$8"
shift 8
printf '%s\n' "$$" > "$pid_file"
cd "$workspace"
exec env -i \
	"PATH=$runtime_path" \
	"HOME=$HOME" \
	"TMPDIR=$temp_root" \
	"TERM=xterm-256color" \
	"LANG=C.UTF-8" \
	"PIG_HOME=$pig_home" \
	"PIG_CODING_AGENT_DIR=$pig_home/agent" \
	"PIG_CODING_AGENT_SESSION_DIR=$pig_home/agent/sessions" \
	"PIG_USE_PI_DIRS=0" \
	"PIG_OFFLINE=1" \
	"PI_OFFLINE=1" \
	"GOPROXY=off" \
	"GOENV=off" \
	"GOTOOLCHAIN=local" \
	"GOCACHE=$go_cache" \
	"GOMODCACHE=$go_mod_cache" \
	"$pig_path" --offline --no-session "$@"
LAUNCH
chmod +x "$run_dir/launch.sh"

start_case() {
	local case_name="$1"
	local session_name="$2"
	tmp_pig_home="$run_dir/pig-home-$case_name"
	local pid_file="$run_dir/$case_name.pid"
	shift 2
	mkdir -p "$tmp_pig_home/agent/sessions"
	local -a command_args=("$run_dir/launch.sh" "$pid_file" "$run_dir" "$tmp_pig_home" "$run_dir/go-cache" "$run_dir/go-mod-cache" "$workspace" "$pig_path" "$runtime_path" "$@")
	local command_text
	printf -v command_text '%q ' "${command_args[@]}"
	if (( server_started == 0 )); then
		server_started=1
		env -i "PATH=$runtime_path" "HOME=$HOME" "TMPDIR=$run_dir" "TERM=xterm-256color" \
			tmux -S "$socket_path" -f "$run_dir/tmux.conf" new-session -d -s "$session_name" -c "$workspace" -x 100 -y 30 "$command_text"
	else
		tmux_do new-session -d -s "$session_name" -c "$workspace" -x 100 -y 30 "$command_text"
	fi
}

choose_session_only_trust() {
	local case_name="$1"
	local session_name="$2"
	local screen="$3"
	if [[ "$screen" != *"Trust project folder?"* ]]; then
		return 1
	fi
	printf '%s\n' "$screen" > "$evidence_dir/$case_name.trust-prompt.txt"
	if [[ "$screen" != *"Trust (this session only)"* || "$screen" != *"Trust parent folder"* ]]; then
		failure_phase="$case_name trust"
		failure_reason="PiG's expected session-only trust option was not visible"
		return 2
	fi
	tmux_do send-keys -t "$session_name" Down
	tmux_do send-keys -t "$session_name" Down
	screen="$(capture_pane "$session_name")"
	printf '%s\n' "$screen" > "$evidence_dir/$case_name.trust-selected.txt"
	if [[ "$screen" != *"→ Trust (this session only)"* ]]; then
		failure_phase="$case_name trust"
		failure_reason="keyboard navigation did not select the session-only trust option"
		return 2
	fi
	printf 'Down\nDown\nEnter on Trust (this session only)\n' > "$evidence_dir/$case_name.trust-action.txt"
	tmux_do send-keys -t "$session_name" Enter
	trust_action["$case_name"]=session_only
	return 0
}

wait_for_ready() {
	local case_name="$1"
	local session_name="$2"
	local ready_marker="$3"
	local deadline=$((SECONDS + 40))
	local screen="" pane_pid pane_dead trust_handled=0
	while (( SECONDS < deadline )); do
		if ! tmux_do has-session -t "$session_name" 2>/dev/null; then
			failure_phase="$case_name readiness"
			failure_reason="tmux session ended before the ready screen appeared"
			return 1
		fi
		pane_dead="$(tmux_do display-message -p -t "$session_name" '#{pane_dead}')"
		pane_pid="$(tmux_do display-message -p -t "$session_name" '#{pane_pid}')"
		if [[ "$pane_dead" == 1 ]] || [[ ! "$pane_pid" =~ ^[0-9]+$ ]] || ! kill -0 "$pane_pid" 2>/dev/null; then
			printf '%s\n' "$(capture_pane "$session_name")" > "$evidence_dir/$case_name.startup-exit.txt"
			printf '%s\n' "$(tmux_do display-message -p -t "$session_name" '#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{pane_current_command}')" \
				> "$evidence_dir/$case_name.startup-process.txt"
			failure_phase="$case_name readiness"
			failure_reason="PiG pane process exited or did not stay live before readiness"
			return 1
		fi
		screen="$(capture_pane "$session_name")"
		if [[ "$screen" == *"Trust project folder?"* && "$trust_handled" == 0 ]]; then
			if choose_session_only_trust "$case_name" "$session_name" "$screen"; then
				trust_handled=1
				continue
			else
				failure_phase="$case_name trust"
				[[ -n "$failure_reason" ]] || failure_reason="could not select session-only trust"
				return 1
			fi
		fi
		if [[ "$screen" == *"$ready_marker"* ]]; then
			printf '%s\n' "$screen" > "$evidence_dir/$case_name.ready.txt"
			return 0
		fi
		sleep 0.1
	done
	failure_phase="$case_name readiness"
	failure_reason="PiG did not display the expected ready marker: $ready_marker"
	printf '%s\n' "${screen:-no capture}" > "$evidence_dir/$case_name.timeout.txt"
	return 1
}

wait_for_notification() {
	local case_name="$1"
	local session_name="$2"
	local deadline=$((SECONDS + 10))
	local screen=""
	while (( SECONDS < deadline )); do
		if ! tmux_do has-session -t "$session_name" 2>/dev/null; then
			failure_phase="$case_name command"
			failure_reason="PiG session ended before the diagnostic notification appeared"
			return 1
		fi
		screen="$(capture_pane "$session_name")"
		if [[ "$screen" == *"Pig Litter is loaded. This bootstrap has no child delegation."* ]]; then
			printf '%s\n' "$screen" > "$evidence_dir/$case_name.after.txt"
			return 0
		fi
		sleep 0.1
	done
	printf '%s\n' "${screen:-no capture}" > "$evidence_dir/$case_name.notification-timeout.txt"
	failure_phase="$case_name command"
	failure_reason="the diagnostic notification did not appear"
	return 1
}

check_exit() {
	local case_name="$1"
	local session_name="$2"
	local deadline=$((SECONDS + 10))
	local fields dead status signal exit_signal_value
	tmux_do send-keys -t "$session_name" C-d
	while (( SECONDS < deadline )); do
		fields="$(tmux_do display-message -p -t "$session_name" '#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}')"
		IFS='|' read -r dead status signal <<< "$fields"
		if [[ "$dead" == 1 ]]; then break; fi
		sleep 0.1
	done
	dead="${dead:-unknown}"
	status="${status:-unknown}"
	exit_signal_value="${signal:-none}"
	pane_dead["$case_name"]="$dead"
	exit_status["$case_name"]="$status"
	exit_signal["$case_name"]="$exit_signal_value"
	printf 'pane_dead=%s\npane_dead_status=%s\npane_dead_signal=%s\n' "$dead" "$status" "$exit_signal_value" > "$evidence_dir/$case_name.exit.txt"
	if [[ "$dead" != 1 || "$status" != 0 || -n "$signal" ]]; then
		failure_phase="$case_name exit"
		failure_reason="expected pane_dead=1, pane_dead_status=0, and no signal; got $fields"
		return 1
	fi
	local pid_file="$run_dir/$case_name.pids"
	local pid
	local all_reaped=true
	: > "$evidence_dir/$case_name.process-tree-exit.txt"
	while read -r pid; do
		[[ -n "$pid" ]] || continue
		if kill -0 "$pid" 2>/dev/null; then
			all_reaped=false
			printf 'still_alive=%s\n' "$pid" >> "$evidence_dir/$case_name.process-tree-exit.txt"
		else
			printf 'reaped=%s\n' "$pid" >> "$evidence_dir/$case_name.process-tree-exit.txt"
		fi
	done < "$pid_file"
	if [[ "$all_reaped" != true ]]; then
		process_tree_reaped["$case_name"]=false
		failure_phase="$case_name process cleanup"
		failure_reason="a PiG or extension descendant remained after normal exit"
		return 1
	fi
	process_tree_reaped["$case_name"]=true
	tmux_do kill-session -t "$session_name"
}

run_case() {
	local case_name="$1"
	local ready_marker="$2"
	local session_name="pig-litter-$run_id-$case_name"
	local status_text="bootstrap only, no child delegation"
	local diagnostic_text="Pig Litter is loaded. This bootstrap has no child delegation."
	shift 2
	start_case "$case_name" "$session_name" "$@"
	wait_for_ready "$case_name" "$session_name" "$ready_marker"
	track_process_tree "$case_name" before "$session_name"
	local before_screen
	before_screen="$(capture_pane "$session_name")"
	printf '%s\n' "$before_screen" > "$evidence_dir/$case_name.before.txt"
	if [[ "$case_name" == plain ]]; then
		if [[ "$before_screen" == *"$status_text"* || "$before_screen" == *"$diagnostic_text"* ]]; then
			fail_run "$case_name assertion" "plain PiG displayed a Pig Litter status or notification"
		fi
		track_process_tree "$case_name" ready "$session_name"
	else
		if [[ "$before_screen" != *"$status_text"* || "$before_screen" == *"$diagnostic_text"* ]]; then
			fail_run "$case_name assertion" "selected startup did not show only the bootstrap status before the command"
		fi
		printf '/pig-litter\n' > "$evidence_dir/$case_name.action.txt"
		tmux_do send-keys -t "$session_name" -l "/pig-litter"
		tmux_do send-keys -t "$session_name" Enter
		wait_for_notification "$case_name" "$session_name"
		local after_screen
		after_screen="$(capture_pane "$session_name")"
		if [[ "$after_screen" != *"$status_text"* || "$after_screen" != *"$diagnostic_text"* ]]; then
			fail_run "$case_name assertion" "post-command screen did not show both distinct literals"
		fi
		track_process_tree "$case_name" after-command "$session_name"
		if (( process_tree_count["$case_name"] < 2 )); then
			fail_run "$case_name process check" "the selected extension had no observable child process"
		fi
	fi
	check_exit "$case_name" "$session_name"
	if [[ -e "$repo_root/.pig" ]]; then
		fail_run "$case_name project state" "PiG created project-local .pig state; it is preserved for inspection"
	fi
	case_result["$case_name"]=passed
}

run_case direct 'bootstrap only, no child delegation' -e ./extensions/pig-litter
run_case piglet 'bootstrap only, no child delegation' --piglet ./piglet.yaml
run_case plain 'No models available.'
if [[ -e "$repo_root/.pig" ]]; then
	fail_run cleanup "PiG created project-local .pig state; it is preserved for inspection"
fi
project_state_untouched=true
outcome=passed
