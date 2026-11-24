#!/usr/bin/env python3
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

OPERATIONS = (
    'openTeamStoreWithSync', 'Reserve', 'Commit', 'Rollback', 'SetWorkspacePhase',
    'Reservations', 'SetState', 'PauseMember', 'Snapshot', 'SnapshotWithReservations', 'Close',
)
SOURCES = (
    'extensions/pig-litter/team_store.go',
    'scripts/cmd/verify-lifecycle/team_roster.go',
    'scripts/cmd/verify-lifecycle/lifecycle.go',
)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def git(checkout, *args):
    return subprocess.check_output(['git', '-C', str(checkout), *args])


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise ValueError(f'expected one instrumentation target: {old!r}')
    return text.replace(old, new)


def prepare(checkout, candidate, output):
    checkout = checkout.resolve()
    output = output.resolve()
    if git(checkout, 'rev-parse', 'HEAD').decode().strip() != candidate:
        raise ValueError('candidate checkout does not match the requested SHA')
    output.mkdir(mode=0o700)
    originals = {}
    for name in SOURCES:
        data = (checkout / name).read_bytes()
        if data != git(checkout, 'show', f'{candidate}:{name}'):
            raise ValueError(f'instrumentation input differs from candidate: {name}')
        originals[name] = data.decode()

    store = replace_once(originals[SOURCES[0]], '\t"sync"\n', '\t"sync"\n\t"time"\n')
    for name in OPERATIONS:
        lines = store.splitlines(keepends=True)
        matches = [i for i, line in enumerate(lines) if line.startswith('func ') and (f' {name}(' in line or f') {name}(' in line)]
        if len(matches) != 1 or not lines[matches[0]].rstrip().endswith('{'):
            raise ValueError(f'cannot locate metadata operation {name}')
        lines.insert(matches[0] + 1, f'\tdefer p03PerformanceRecord("{name}", time.Now())\n')
        store = ''.join(lines)
    store += '''
var p03PerformanceMu sync.Mutex

func p03PerformanceRecord(operation string, started time.Time) {
    elapsed := time.Since(started).Nanoseconds()
    path := os.Getenv("P03_PERF_TRACE")
    if path == "" { return }
    p03PerformanceMu.Lock()
    defer p03PerformanceMu.Unlock()
    file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
    if err != nil { fmt.Fprintf(os.Stderr, "p03 performance trace: %v\\n", err); return }
    if _, err = fmt.Fprintf(file, "{\\"operation\\":%q,\\"duration_ns\\":%d}\\n", operation, elapsed); err != nil { fmt.Fprintf(os.Stderr, "p03 performance trace: %v\\n", err) }
    if err = file.Close(); err != nil { fmt.Fprintf(os.Stderr, "p03 performance trace: %v\\n", err) }
}
'''
    team = originals[SOURCES[1]]
    team = replace_once(team, '\tpauseAfter  string\n}', (
        '\tpauseAfter  string\n'
        '\tperfStart time.Time\n'
        '\tperfReady map[string]int64\n'
        '\tperfReopenStart time.Time\n'
        '\tperfReopenFirstNS int64\n}'
    ))
    team = replace_once(team, '\tcase 0:\n\t\tf.team.stage = 1\n', (
        '\tcase 0:\n'
        '\t\tf.mu.Lock()\n'
        '\t\tf.team.perfStart = time.Now()\n'
        '\t\tf.mu.Unlock()\n'
        '\t\tf.team.stage = 1\n'
    ))
    reopen = '\t\tf.team.stage = 7\n\t\treturn "", []call{{"team_list", map[string]any{}}}, nil\n'
    team = replace_once(team, reopen, (
        '\t\tf.mu.Lock()\n'
        '\t\tf.team.perfReopenFirstNS = time.Since(f.team.perfReopenStart).Nanoseconds()\n'
        '\t\tf.mu.Unlock()\n' + reopen
    ))
    team = replace_once(team, '\t\tf.team.saw[name] = true\n', (
        '\t\tf.team.saw[name] = true\n'
        '\t\tf.team.perfReady[name] = time.Since(f.team.perfStart).Nanoseconds()\n'
    ))
    team = replace_once(team, 'saw: map[string]bool{}}', 'saw: map[string]bool{}, perfReady: map[string]int64{}}')
    team = replace_once(team, '\tbefore := f.teamRequests()\n\treopenArgs :=', (
        '\tbefore := f.teamRequests()\n'
        '\tf.mu.Lock()\n'
        '\tf.team.perfReopenStart = time.Now()\n'
        '\tf.mu.Unlock()\n'
        '\treopenArgs :='
    ))
    result = '\trefs, started, wrote, saw := maps.Clone(f.team.refs), maps.Clone(f.team.started), maps.Clone(f.team.wrote), maps.Clone(f.team.saw)\n'
    team = replace_once(team, result + '\tf.mu.Unlock()\n\treturn writeJSON(', (
        result + '\tready, reopenNS := maps.Clone(f.team.perfReady), f.team.perfReopenFirstNS\n'
        '\tf.mu.Unlock()\n'
        '\tif err := writeJSON(filepath.Join(directory, "team-performance.json"), map[string]any{"clock": "Go monotonic time.Since", "ready_ns": ready, "reopen_to_first_parent_request_ns": reopenNS}); err != nil { return err }\n'
        '\treturn writeJSON('
    ))
    end_environment = '\treturn environment\n}\n\nfunc (f *fixture) runLifecycle'
    lifecycle = replace_once(originals[SOURCES[2]], end_environment, (
        '\tif trace := os.Getenv("P03_PERF_TRACE"); trace != "" { environment = append(environment, "P03_PERF_TRACE="+trace) }\n'
        + end_environment
    ))

    replacements = {}
    for name, data in zip(SOURCES, (store, team, lifecycle)):
        destination = output / Path(name).name
        destination.write_text(data)
        replacements[name] = destination
    overlays = {}
    for name, paths in {'extension': SOURCES[:1], 'driver': SOURCES[1:]}.items():
        path = output / f'{name}-overlay.json'
        path.write_text(json.dumps({'Replace': {str(checkout / source): str(replacements[source]) for source in paths}}, indent=2) + '\n')
        overlays[name] = path
    tracked = {str(checkout / name): digest(data.encode()) for name, data in originals.items()}
    generated = {str(path): digest(path.read_bytes()) for path in [*replacements.values(), *overlays.values()]}
    manifest = {'candidate': candidate, 'checkout': str(checkout), 'source_sha256': tracked, 'generated_sha256': generated, 'overlays': {name: str(path) for name, path in overlays.items()}}
    (output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    verify(output / 'manifest.json', candidate)


def verify(manifest_path, candidate):
    manifest = json.loads(manifest_path.read_text())
    checkout = Path(manifest['checkout'])
    if manifest['candidate'] != candidate or git(checkout, 'rev-parse', 'HEAD').decode().strip() != candidate:
        raise ValueError('overlay candidate identity changed')
    expected_sources = {str(checkout / name) for name in SOURCES}
    if set(manifest['source_sha256']) != expected_sources:
        raise ValueError('overlay source list is incomplete')
    for name, expected in manifest['source_sha256'].items():
        data = Path(name).read_bytes()
        relative = str(Path(name).relative_to(checkout))
        if digest(data) != expected or data != git(checkout, 'show', f'{candidate}:{relative}'):
            raise ValueError(f'overlay source changed: {name}')
    for name, expected in manifest['generated_sha256'].items():
        if digest(Path(name).read_bytes()) != expected:
            raise ValueError(f'overlay output changed: {name}')
    for overlay, sources in (('extension', SOURCES[:1]), ('driver', SOURCES[1:])):
        path = manifest['overlays'][overlay]
        mapping = json.loads(Path(path).read_text())['Replace']
        expected = {str(checkout / name): str(manifest_path.parent / Path(name).name) for name in sources}
        if mapping != expected or path not in manifest['generated_sha256'] or any(value not in manifest['generated_sha256'] for value in mapping.values()):
            raise ValueError(f'invalid {overlay} overlay')
    return manifest


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=('prepare', 'verify'))
    parser.add_argument('--checkout', type=Path)
    parser.add_argument('--candidate', required=True)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    if args.action == 'prepare':
        if args.checkout is None:
            parser.error('prepare requires --checkout')
        prepare(args.checkout, args.candidate, args.output)
    else:
        verify(args.output / 'manifest.json', args.candidate)


if __name__ == '__main__':
    main()
