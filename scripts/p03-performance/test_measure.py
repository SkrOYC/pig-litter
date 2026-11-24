#!/usr/bin/env python3
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import measure
import prepare


def sample_set():
    samples = []
    for index in range(20):
        samples.extend([
            {'label': 'baseline', 'status': 'passed', 'spawn_ns': 500_000_000},
            {'label': 'candidate', 'status': 'passed', 'spawn_ns': 600_000_000},
            {'label': 'team', 'status': 'passed', 'ready_ns': {'alpha': 1, 'beta': 2, 'gamma': 2_999_999_999}, 'reopen_ns': 1},
            {'label': 'metadata', 'status': 'passed', 'metadata': [{'operation': name, 'duration_ns': 99_999_999} for name in ('Reserve', 'Commit', 'SetState')]},
        ])
    return samples


class MeasurementPolicyTest(unittest.TestCase):
    def test_original_threshold_boundaries(self):
        samples = sample_set()
        self.assertEqual(measure.summarize(samples)['status'], 'passed')
        for sample in samples:
            if sample['label'] == 'candidate':
                sample['spawn_ns'] += 1
        self.assertEqual(measure.summarize(samples)['status'], 'regressed')

    def test_spawn_regression_requires_both_limits(self):
        for baseline, candidate in ((1_000_000_000, 1_150_000_000), (100_000_000, 150_000_000)):
            samples = sample_set()
            for sample in samples:
                if sample['label'] == 'baseline':
                    sample['spawn_ns'] = baseline
                elif sample['label'] == 'candidate':
                    sample['spawn_ns'] = candidate
            self.assertEqual(measure.summarize(samples)['status'], 'passed')

    def test_spawn_gate_uses_nearest_rank_p95(self):
        samples = sample_set()
        candidates = [sample for sample in samples if sample['label'] == 'candidate']
        candidates[0]['spawn_ns'] = 900_000_000
        self.assertEqual(measure.summarize(samples)['status'], 'passed')
        candidates[1]['spawn_ns'] = 900_000_000
        self.assertEqual(measure.summarize(samples)['status'], 'regressed')

    def test_three_member_and_metadata_limits(self):
        samples = sample_set()
        next(sample for sample in samples if sample['label'] == 'team')['ready_ns']['gamma'] += 1
        self.assertEqual(measure.summarize(samples)['status'], 'regressed')
        samples = sample_set()
        for sample in samples:
            if sample['label'] == 'metadata':
                sample['metadata'][0]['duration_ns'] = 100_000_000
        self.assertEqual(measure.summarize(samples)['status'], 'regressed')

    def test_failed_missing_or_incomplete_samples_do_not_pass(self):
        samples = sample_set()
        samples[0]['status'] = 'failed'
        with self.assertRaisesRegex(ValueError, 'failed'):
            measure.summarize(samples)
        with self.assertRaisesRegex(ValueError, 'coverage'):
            measure.summarize(sample_set()[1:])
        samples = sample_set()
        for sample in samples:
            if sample['label'] == 'metadata':
                sample['metadata'] = []
        with self.assertRaisesRegex(ValueError, 'metadata operations'):
            measure.summarize(samples)

    def test_release_requires_hosted_run_and_exact_candidate(self):
        candidate = 'a' * 40
        release = {'baseline': measure.BASELINE, 'candidate': candidate, 'run_id': '123', 'root_released': True}
        env = {'GITHUB_RUN_ID': '123', 'GITHUB_SHA': candidate, 'RUNNER_ENVIRONMENT': 'github-hosted'}
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, env, clear=True):
            path = Path(directory) / 'release.json'
            path.write_text(json.dumps(release))
            self.assertEqual(measure.check_release(path, candidate), release)
            release['root_released'] = 1
            path.write_text(json.dumps(release))
            with self.assertRaisesRegex(ValueError, 'release'):
                measure.check_release(path, candidate)
            release['root_released'] = True
            path.write_text(json.dumps(release))
            with patch.dict(os.environ, {'GITHUB_SHA': 'b' * 40}):
                with self.assertRaisesRegex(ValueError, 'release'):
                    measure.check_release(path, candidate)
            with patch.dict(os.environ, {'RUNNER_ENVIRONMENT': 'self-hosted'}):
                with self.assertRaisesRegex(ValueError, 'release'):
                    measure.check_release(path, candidate)

    def test_overlay_rejects_changed_generated_source(self):
        checkout = Path(__file__).resolve().parents[2]
        candidate = measure.git(checkout, 'rev-parse', 'HEAD')
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'overlay'
            prepare.prepare(checkout, candidate, output)
            prepare.verify(output / 'manifest.json', candidate)
            path = output / 'team_store.go'
            path.write_text(path.read_text() + '\n')
            with self.assertRaisesRegex(ValueError, 'overlay output changed'):
                prepare.verify(output / 'manifest.json', candidate)


if __name__ == '__main__':
    unittest.main()
