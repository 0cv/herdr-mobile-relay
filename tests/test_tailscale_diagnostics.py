#!/usr/bin/env python3
"""Hosted-only source controls; not product qualification or local test authority."""
import os
import sys
if (os.environ.get('GITHUB_ACTIONS') != 'true' or
        os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted' or os.getuid() == 0):
    raise SystemExit('diagnostic controls require a non-root hosted runner')

import importlib.util
import json
import select
import signal
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SOURCE = Path(__file__).resolve().parents[1] / 'scripts/ci/tailscale-diagnostics.py'
spec = importlib.util.spec_from_file_location('tailscale_diagnostics', SOURCE)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)  # after guard; never run main by importing


class SourceControls(unittest.TestCase):
    def test_wall_is_not_spawn_estimate(self):
        first = module.summary_cell([9, 11, 15], 3, 6)
        second = module.summary_cell([100, 200, 300], 3, 6)
        self.assertEqual(first['top_level_processes'], second['top_level_processes'])
        self.assertEqual(first['known_inner_processes'], second['known_inner_processes'])
        self.assertEqual(first['wall_ns_total'], 35)
        self.assertEqual(second['wall_ns_total'], 600)
        self.assertEqual(first['attribution'], 'end_to_end_only')
        for walls, outer, inner in (([], 0, 0), ([1] * 4, 4, 0), ([1], 2, 0), ([1], 1, 3)):
            with self.assertRaises(ValueError): module.summary_cell(walls, outer, inner)

    def test_rewrite_short_circuits_cmp_not_model_check(self):
        self.assertEqual(module.inner_per_sample('model_check_rewrite'), 1)
        self.assertEqual(module.inner_per_sample('fast_direct'), 1)
        self.assertEqual(module.inner_per_sample('strict_delegate'), 1)
        self.assertEqual(module.inner_per_sample('cmp_unchanged'), 0)
        self.assertEqual(module.inner_per_sample('real_tool'), 0)
        cell = module.summary_cell([3, 4, 5], 3, 3 * module.inner_per_sample('model_check_rewrite'))
        self.assertEqual(cell['known_inner_processes'], 3)

    def test_missing_alias_never_becomes_negative_pass(self):
        self.assertEqual(module.status_for_control(False, True, True), 'unsupported')
        self.assertEqual(module.status_for_control(True, False, True), 'inconclusive')
        self.assertEqual(module.status_for_control(True, True, False), 'red')
        self.assertEqual(module.status_for_control(True, True, True), 'observed')

    def test_profile_child_receives_term_without_losing_parent_mask(self):
        # This suite is guarded before fixture import; no service/product fixture runs.
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        import test_service_uninstall_cli as fixture
        saved_profile = os.environ.get('HERDR_DIAG_PROFILE_FILE')
        # Opt in to the ownership path without writing a profile or running main().
        os.environ['HERDR_DIAG_PROFILE_FILE'] = 'hosted-mask-control'
        prior = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGUSR1})
        parent_mask = signal.pthread_sigmask(signal.SIG_BLOCK, set())
        owned_before = dict(fixture._OWNED_GROUPS)
        child = None
        try:
            real_popen = subprocess.Popen

            def checked_popen(*args, **kwargs):
                self.assertIn(signal.SIGTERM, signal.pthread_sigmask(signal.SIG_BLOCK, set()))
                return real_popen(*args, **kwargs)

            code = 'import os, signal; os.write(1, b"ready\\n"); signal.pause()'
            with mock.patch.object(fixture.subprocess, 'Popen', side_effect=checked_popen):
                child = fixture.spawn_owned_session(
                    [sys.executable, '-c', code], stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, set()), parent_mask)
            self.assertIs(fixture._OWNED_GROUPS.get(child.pid), child)
            self.assertEqual(owned_before.keys(), fixture._OWNED_GROUPS.keys() - {child.pid})
            ready, _, _ = select.select([child.stdout], [], [], 5)
            self.assertTrue(ready, 'owned child did not become ready')
            self.assertEqual(os.read(child.stdout.fileno(), 6), b'ready\n')
            os.killpg(child.pid, signal.SIGTERM)
            # A timeout or SIGKILL is never a passing TERM-delivery oracle.
            self.assertEqual(child.wait(timeout=5), -signal.SIGTERM)

            def failed_popen(*args, **kwargs):
                self.assertIn(signal.SIGTERM, signal.pthread_sigmask(signal.SIG_BLOCK, set()))
                raise OSError('controlled spawn failure')

            with mock.patch.object(fixture.subprocess, 'Popen', side_effect=failed_popen):
                with self.assertRaisesRegex(OSError, 'controlled spawn failure'):
                    fixture.spawn_owned_session([sys.executable, '-c', code])
            self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, set()), parent_mask)
            self.assertIs(fixture._OWNED_GROUPS.get(child.pid), child)
            self.assertEqual(fixture._OWNED_GROUPS.keys(), owned_before.keys() | {child.pid})
        finally:
            try:
                if child is not None:
                    try:
                        if child.poll() is None:
                            try: os.killpg(child.pid, signal.SIGKILL)
                            except ProcessLookupError: pass
                        child.wait(timeout=5)
                    finally:
                        child.stdout.close()
                        fixture.forget_owned_session(child)
            finally:
                signal.pthread_sigmask(signal.SIG_SETMASK, prior)
                if saved_profile is None: os.environ.pop('HERDR_DIAG_PROFILE_FILE', None)
                else: os.environ['HERDR_DIAG_PROFILE_FILE'] = saved_profile
        self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, set()), prior)
        self.assertEqual(fixture._OWNED_GROUPS, owned_before)

    def test_profile_aggregation_is_finite_and_redacted(self):
        # Import the guarded fixture only after hosted admission. No fixture is
        # constructed; these pure counter controls cannot read canary bytes.
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        import test_service_uninstall_cli as fixture
        saved = fixture._PROFILE.copy()
        original_env = os.environ.get('HERDR_DIAG_PROFILE_FILE')
        original_temp = os.environ['RUNNER_TEMP']
        try:
            with tempfile.TemporaryDirectory(dir=os.environ['RUNNER_TEMP']) as temporary:
                # Use only a private runner directory as the profile destination.
                root = Path(temporary)
                os.chmod(root, 0o700)
                os.environ['RUNNER_TEMP'] = str(root)
                os.environ['HERDR_DIAG_PROFILE_FILE'] = str(root / 'service.json')
                fixture._PROFILE = {'schema': 1, 'perturbation': 'instrumented_wall_includes_fixture_and_tracing',
                                    'sample_limit': 128, 'cases_seen': 0, 'cases_sampled': [], 'cells': {}}
                for index in range(130):
                    fixture.profile_record('Darwin', 'private-source-canary' if index else '',
                                           index + 1, {'stat': 1}, {'stat': 3})
                fixture.profile_flush()
                data = json.loads((root / 'service.json').read_text())
                self.assertEqual(data['cases_seen'], 130)
                self.assertEqual(len(data['cases_sampled']), 128)
                self.assertEqual(data['sample_limit'], 128)
                self.assertEqual(data['cases_seen'] - len(data['cases_sampled']), 2)
                self.assertEqual(data['cells']['darwin_adverse']['stub_spawns'], 258)
                self.assertNotIn('private-source-canary', json.dumps(data))
                self.assertNotIn(str(root), json.dumps(data))
        finally:
            fixture._PROFILE = saved
            if original_env is None: os.environ.pop('HERDR_DIAG_PROFILE_FILE', None)
            else: os.environ['HERDR_DIAG_PROFILE_FILE'] = original_env
            os.environ['RUNNER_TEMP'] = original_temp


if __name__ == '__main__': unittest.main()
