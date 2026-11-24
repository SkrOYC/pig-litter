#!/usr/bin/env python3
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import signal
import subprocess
import time

from prepare import OPERATIONS, verify

BASELINE = '2dcb8f4ea03e941df6ca543af8c862d0d283b40b'
NAMES = {'alpha', 'beta', 'gamma'}


def digest(path):
    hasher = hashlib.sha256()
    with path.open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            hasher.update(block)
    return hasher.hexdigest()


def git(path, *args):
    return subprocess.check_output(['git', '-C', str(path), *args], text=True).strip()


def positive(value):
    return type(value) is int and value > 0


def percentile(values, fraction):
    if not values or any(not positive(value) for value in values):
        raise ValueError('invalid or incomplete duration samples')
    return sorted(values)[math.ceil(fraction * len(values)) - 1]


def summarize(samples):
    groups = {name: [sample for sample in samples if sample['label'] == name] for name in ('baseline', 'candidate', 'team', 'metadata')}
    if any(len(group) != 20 for group in groups.values()):
        raise ValueError('sample coverage is incomplete')
    if any(sample.get('status') != 'passed' for group in groups.values() for sample in group):
        raise ValueError('a measured sample failed')
    baseline = [sample['spawn_ns'] for sample in groups['baseline']]
    candidate = [sample['spawn_ns'] for sample in groups['candidate']]
    base95, head95 = percentile(baseline, .95), percentile(candidate, .95)
    operations = {}
    for sample in groups['metadata']:
        for event in sample['metadata']:
            operations.setdefault(event['operation'], []).append(event['duration_ns'])
    if not {'Reserve', 'Commit', 'SetState'} <= operations.keys():
        raise ValueError('metadata operations are incomplete')
    stats = {name: {'count': len(values), 'p50_ns': percentile(values, .5), 'p95_ns': percentile(values, .95)} for name, values in operations.items()}
    ready = [max(sample['ready_ns'].values()) for sample in groups['team']]
    recreation = [sample['reopen_ns'] for sample in groups['team']]
    summary = {
        'status': 'passed',
        'spawn': {'baseline_p50_ns': percentile(baseline, .5), 'baseline_p95_ns': base95, 'candidate_p50_ns': percentile(candidate, .5), 'candidate_p95_ns': head95},
        'metadata': stats,
        'team_ready_p50_ns': percentile(ready, .5), 'team_ready_p95_ns': percentile(ready, .95), 'team_ready_max_ns': max(ready),
        'reopen_p50_ns': percentile(recreation, .5), 'reopen_p95_ns': percentile(recreation, .95),
    }
    if (head95 * 5 > base95 * 6 and head95 - base95 > 100_000_000
            or any(item['p95_ns'] >= 100_000_000 for item in stats.values())
            or max(ready) >= 3_000_000_000):
        summary['status'] = 'regressed'
    return summary


def check_checkout(path, sha, clean):
    if len(sha) != 40 or any(char not in '0123456789abcdef' for char in sha):
        raise ValueError('SHA must be a full lowercase commit hash')
    if git(path, 'rev-parse', 'HEAD') != sha:
        raise ValueError(f'checkout is not at {sha}')
    if clean and git(path, 'status', '--porcelain=v1', '--untracked-files=all'):
        raise ValueError(f'checkout is not clean at {sha}')


def check_release(path, candidate):
    release = json.loads(path.read_text())
    expected = {'baseline': BASELINE, 'candidate': candidate, 'run_id': os.environ.get('GITHUB_RUN_ID')}
    if (release.get('root_released') is not True
            or any(release.get(key) != value for key, value in expected.items())
            or not expected['run_id'] or os.environ.get('GITHUB_SHA') != candidate
            or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted'):
        raise ValueError('measurement lacks the hosted release for these exact inputs')
    return release


def fixture_sample(output, label, index, driver, extension, host, scenario, measure=False, trace=False):
    directory = output / f'{label}-{index:02d}'
    directory.mkdir(mode=0o700)
    runtime = directory / 'runtime'
    runtime.mkdir()
    env = {'PATH': os.environ['PATH'], 'HOME': str(directory), 'TMPDIR': str(runtime), 'LANG': 'C.UTF-8', 'TERM': 'xterm-256color'}
    if trace:
        env['P03_PERF_TRACE'] = str(directory / 'metadata.jsonl')
    command = [str(driver), '--pig-bin', str(host), '--extension', str(extension), '--scenario', scenario, '--evidence-root', str(directory)]
    if measure:
        command.append('--measure-performance')
    record = {'label': label, 'index': index, 'argv': command, 'start_ns': time.monotonic_ns()}
    (directory / 'sample.json').write_text(json.dumps(record, indent=2) + '\n')
    process = subprocess.Popen(command, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    timed_out = False
    try:
        stdout, stderr = process.communicate(timeout=90)
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(process.pid, signal.SIGTERM)
        try:
            stdout, stderr = process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            stdout, stderr = process.communicate()
    record.update({'end_ns': time.monotonic_ns(), 'exit': process.returncode, 'timeout': timed_out})
    (directory / 'stdout.log').write_bytes(stdout)
    (directory / 'stderr.log').write_bytes(stderr)
    try:
        if timed_out or process.returncode != 0:
            raise ValueError(f'{label} {index} fixture failed or timed out')
        response = json.loads(stdout)
        if response.get('passed') is not True or response.get('scenario') != scenario:
            raise ValueError('fixture did not report success for the requested scenario')
        fixture = Path(response['directory'])
        if fixture.parent.resolve() != directory.resolve():
            raise ValueError('fixture output escaped its sample directory')
        if any(b'p03 performance trace:' in path.read_bytes() for path in directory.rglob('stderr.log')):
            raise ValueError('metadata trace reported an error')
        if measure:
            performance = json.loads((fixture / 'performance.json').read_text())
            result = json.loads((fixture / 'result.json').read_text())
            if result.get('passed') is not True or result.get('exitCode') != 0 or result.get('isolatedEnvironment') is not True:
                raise ValueError('core fixture did not prove clean exit and isolation')
            record['spawn_ns'] = performance['spawn_to_first_request_ns']
            if not positive(record['spawn_ns']):
                raise ValueError('invalid spawn duration')
        if scenario == 'team-roster':
            performance = json.loads((fixture / 'team-performance.json').read_text())
            roster = json.loads((fixture / 'team-roster.json').read_text())
            ready = performance['ready_ns']
            if set(ready) != NAMES or any(not positive(value) for value in ready.values()):
                raise ValueError('three members did not all reach the ready boundary')
            if any(set(roster[key]) != NAMES or any(value is not True for value in roster[key].values()) for key in ('started', 'wrote', 'saw')):
                raise ValueError('fixture did not prove all writer effects and rosters')
            if (roster['pauseStateBeforeReopen'] != 'paused' or roster['pauseStateAfterReopen'] != 'paused'
                    or roster['memberRequestsBeforeReopen'] != roster['memberRequestsAfterReopen']):
                raise ValueError('native recreation did not preserve the team')
            record['ready_ns'] = ready
            record['reopen_ns'] = performance['reopen_to_first_parent_request_ns']
            if not positive(record['reopen_ns']):
                raise ValueError('invalid native recreation duration')
        if trace:
            events = [json.loads(line) for line in (directory / 'metadata.jsonl').read_text().splitlines()]
            if any(set(event) != {'operation', 'duration_ns'} or event['operation'] not in OPERATIONS or not positive(event['duration_ns']) for event in events):
                raise ValueError('invalid metadata record')
            counts = {name: sum(event['operation'] == name for event in events) for name in ('Reserve', 'Commit', 'SetState')}
            if any(count < 3 for count in counts.values()):
                raise ValueError(f'missing metadata records: {counts}')
            record['metadata'] = events
        record['status'] = 'passed'
    except Exception as error:
        record.update({'status': 'failed', 'error': str(error)})
    (directory / 'sample.json').write_text(json.dumps(record, indent=2) + '\n')
    return record


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=('smoke', 'run'))
    for name in ('checkout', 'instrumented-extension', 'host', 'team-driver', 'overlay', 'output'):
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument('--candidate', required=True)
    for name in ('baseline-checkout', 'baseline-extension', 'candidate-extension', 'core-driver', 'release'):
        parser.add_argument('--' + name, type=Path)
    args = parser.parse_args()
    clean = args.mode == 'run'
    check_checkout(args.checkout, args.candidate, clean)
    manifest = verify(args.overlay / 'manifest.json', args.candidate)
    if Path(manifest['checkout']) != args.checkout.resolve():
        raise ValueError('overlay belongs to a different checkout')
    if platform.system() != 'Linux' or platform.machine() != 'x86_64':
        raise ValueError('this verifier requires native Linux x86-64')
    names = ['instrumented_extension', 'host', 'team_driver']
    if clean:
        names += ['baseline_extension', 'candidate_extension', 'core_driver']
        if any(getattr(args, name) is None for name in ('baseline_checkout', 'baseline_extension', 'candidate_extension', 'core_driver', 'release')):
            raise ValueError('measurement requires the baseline, binaries, and release')
        release = check_release(args.release, args.candidate)
        check_checkout(args.baseline_checkout, BASELINE, True)
        if git(args.checkout, 'merge-base', args.candidate, BASELINE) != BASELINE:
            raise ValueError('baseline is not an ancestor of the candidate')
    else:
        release = None
    paths = {name: getattr(args, name) for name in names}
    if any(not path.is_absolute() or not path.is_file() or not os.access(path, os.X_OK) for path in paths.values()):
        raise ValueError('every binary must be an absolute executable regular file')
    hashes = {name: digest(path) for name, path in paths.items()}
    if clean and hashes['instrumented_extension'] == hashes['candidate_extension']:
        raise ValueError('instrumented and production extension binaries must differ')
    args.output.mkdir(mode=0o700)
    identity = {
        'mode': args.mode, 'candidate': args.candidate, 'candidate_tree': git(args.checkout, 'rev-parse', 'HEAD^{tree}'),
        'baseline': BASELINE if clean else None, 'baseline_tree': git(args.baseline_checkout, 'rev-parse', 'HEAD^{tree}') if clean else None,
        'binaries': {name: str(path) for name, path in paths.items()}, 'sha256': hashes, 'overlay': manifest,
        'verifier_sha256': {path.name: digest(path) for path in (Path(__file__), Path(__file__).with_name('prepare.py'))},
        'platform': platform.platform(), 'machine': platform.machine(), 'cpu': Path('/proc/cpuinfo').read_text(),
        'release': release, 'started_utc': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
        'boundaries': {
            'spawn': 'From the lead fixture sending litter_spawn to the first ordinary child fixture request.',
            'team_ready': 'From the lead fixture issuing team_spawn calls to each member verifying all writer effects and its complete team_list result.',
            'recreation': 'From immediately before reopening the native host to its first lead fixture request.',
            'metadata': 'Method entry through return, using Go monotonic time before appending the trace record.',
            'percentile': 'Nearest rank over all retained samples.',
        },
    }
    (args.output / 'identity.json').write_text(json.dumps(identity, indent=2) + '\n')
    (args.output / 'load-before.txt').write_text(Path('/proc/loadavg').read_text())
    samples = []

    def run(label, index, driver, extension, scenario, measure=False, trace=False):
        record = fixture_sample(args.output, label, index, driver, extension, args.host, scenario, measure, trace)
        samples.append(record)
        (args.output / 'samples.json').write_text(json.dumps(samples, indent=2) + '\n')
        if record['status'] != 'passed':
            raise ValueError(record['error'])

    try:
        if args.mode == 'smoke':
            run('smoke', 0, args.team_driver, args.instrumented_extension, 'team-roster', trace=True)
            summary = {'status': 'passed', 'purpose': 'functional instrumentation smoke, not a performance result'}
        else:
            for index in range(5):
                run('warmup-baseline', index, args.core_driver, args.baseline_extension, 'core', measure=True)
                run('warmup-candidate', index, args.core_driver, args.candidate_extension, 'core', measure=True)
            for index in range(20):
                order = [('baseline', args.baseline_extension), ('candidate', args.candidate_extension)]
                if index % 2:
                    order.reverse()
                for label, extension in order:
                    run(label, index, args.core_driver, extension, 'core', measure=True)
            for index in range(20):
                run('team', index, args.team_driver, args.candidate_extension, 'team-roster')
            for index in range(20):
                run('metadata', index, args.team_driver, args.instrumented_extension, 'team-roster', trace=True)
            summary = summarize(samples)
        if hashes != {name: digest(path) for name, path in paths.items()}:
            raise ValueError('a binary changed during the run')
        verify(args.overlay / 'manifest.json', args.candidate)
        check_checkout(args.checkout, args.candidate, clean)
        if clean:
            check_checkout(args.baseline_checkout, BASELINE, True)
    except Exception as error:
        summary = {'status': 'incomplete', 'reason': str(error), 'completed_samples': len(samples)}
    (args.output / 'load-after.txt').write_text(Path('/proc/loadavg').read_text())
    (args.output / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(json.dumps(summary, indent=2))
    return {'passed': 0, 'regressed': 1, 'incomplete': 2}[summary['status']]


if __name__ == '__main__':
    raise SystemExit(main())
