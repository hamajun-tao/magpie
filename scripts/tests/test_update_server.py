import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch, call, Mock

spec = importlib.util.spec_from_file_location('update_server', Path(__file__).parents[1] / 'update_server.py')
update = importlib.util.module_from_spec(spec)
spec.loader.exec_module(update)


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.config = self.root / 'config'
        self.config.mkdir()
        for name, content in {
            'providers.json': '[{"id":"seekai","models":["a","b","c"]}]',
            'caller-keys.json': '[{"key":"test-private-key"}]',
            'settings.json': '{"noAutoUpdate":true}',
            'model-health.json': '{"enabled":true,"intervalMinutes":30,"failureThreshold":2}',
        }.items():
            (self.config / name).write_text(content)
            (self.config / name).chmod(0o600)
        (self.config / 'nested').mkdir()
        (self.config / 'nested/state.json').write_text('original nested state')
        self.env = self.root / 'magpie.env'
        self.env.write_text('MAGPIE_WEB_KEY=test-web-key\n')
        self.unit = self.root / 'magpie.service'
        self.unit.write_text('test service')
        self.access = self.root / 'access.txt'
        self.access.write_text('API Key：test-private-key\n')
        for name, value in {'ROOT': self.root, 'CONFIG': self.config, 'ENVFILE': self.env,
                            'UNITFILE': self.unit, 'ACCESSFILE': self.access}.items():
            p = patch.object(update, name, value)
            p.start()
            self.addCleanup(p.stop)
        self.old = self.release('old')
        self.new = self.release('new')
        (self.root / 'current').symlink_to(self.old)
        self.report = {'run': 'test-run', 'version': 'new'}
        self.services = patch.object(update, 'service').start()
        self.accept = patch.object(update, 'wait_verified').start()
        self.addCleanup(patch.stopall)

    def release(self, version):
        p = self.root / 'releases' / version
        p.mkdir(parents=True)
        (p / 'magpie').write_bytes(b'test binary ' + version.encode())
        (p / 'release.json').write_text(json.dumps({'built_version': version}))
        return p

    def test_success_keeps_config_keys_and_health(self):
        before = update.protected_hashes()
        update.deploy(self.new, self.report)
        self.assertEqual((self.root / 'current').resolve(), self.new)
        self.assertEqual(update.protected_hashes(), before)
        self.assertTrue(self.report['accepted'])
        self.assertTrue(self.report['health_enabled'])
        self.assertEqual(self.services.call_args_list, [call('stop'), call('start')])
        backup = Path(self.report['backup']) / 'config'
        self.assertEqual((backup / 'caller-keys.json').stat().st_uid, (self.config / 'caller-keys.json').stat().st_uid)
        self.assertEqual((self.root / 'last-update.json').stat().st_mode & 0o777, 0o600)

    def test_failed_acceptance_restores_full_state_and_old_service(self):
        before = update.protected_hashes()

        def accept(version):
            if version == 'new':
                (self.config / 'providers.json').write_text('modified by new version')
                (self.config / 'nested/state.json').write_text('modified nested state')
                (self.config / 'new-state.json').write_text('new incompatible state')
                self.env.write_text('changed env')
                raise RuntimeError('new version failed acceptance')

        self.accept.side_effect = accept
        with self.assertRaisesRegex(RuntimeError, 'failed acceptance'):
            update.deploy(self.new, self.report)
        self.assertEqual((self.root / 'current').resolve(), self.old)
        self.assertEqual(update.protected_hashes(), before)
        self.assertEqual((self.config / 'nested/state.json').read_text(), 'original nested state')
        self.assertFalse((self.config / 'new-state.json').exists())
        self.assertEqual(self.env.read_text(), 'MAGPIE_WEB_KEY=test-web-key\n')
        self.assertTrue(self.report['rolled_back'])
        self.accept.assert_any_call('old')
        self.services.assert_has_calls([call('stop'), call('start'), call('stop'), call('start')])

    def test_backup_failure_restarts_original_without_switching(self):
        with patch.object(update, 'copy_state', side_effect=OSError('disk full')):
            with self.assertRaisesRegex(OSError, 'disk full'):
                update.deploy(self.new, self.report)
        self.assertEqual((self.root / 'current').resolve(), self.old)
        self.services.assert_called_with('start')
        self.accept.assert_called_once_with('old')

    def test_stop_failure_still_attempts_to_restart_original(self):
        self.services.side_effect = [RuntimeError('stop timed out'), None, None]
        with self.assertRaisesRegex(RuntimeError, 'stop timed out'):
            update.deploy(self.new, self.report)
        self.assertEqual((self.root / 'current').resolve(), self.old)
        self.services.assert_called_with('start')
        self.assertTrue(self.report['rolled_back'])

    def test_changed_key_triggers_rollback_even_when_http_passes(self):
        before = update.protected_hashes()

        def accept(version):
            if version == 'new':
                (self.config / 'caller-keys.json').write_text('replaced key')

        self.accept.side_effect = accept
        with self.assertRaisesRegex(AssertionError, 'key or settings changed'):
            update.deploy(self.new, self.report)
        self.assertEqual(update.protected_hashes(), before)
        self.assertEqual((self.root / 'current').resolve(), self.old)

    def test_release_outside_release_directory_is_refused(self):
        outside = self.root / 'elsewhere'
        outside.mkdir()
        (outside / 'magpie').write_bytes(b'bad binary')
        with self.assertRaisesRegex(RuntimeError, 'outside'):
            update.switch_release(outside)
        self.assertEqual((self.root / 'current').resolve(), self.old)

    def test_disabled_health_checks_trigger_rollback(self):
        def accept(version):
            if version == 'new':
                state = json.loads((self.config / 'model-health.json').read_text())
                state['enabled'] = False
                (self.config / 'model-health.json').write_text(json.dumps(state))

        self.accept.side_effect = accept
        with self.assertRaisesRegex(AssertionError, 'health policy changed'):
            update.deploy(self.new, self.report)
        self.assertTrue(update.health_policy()['enabled'])
        self.assertEqual((self.root / 'current').resolve(), self.old)

    def runner(self):
        runner = update.Update.__new__(update.Update)
        runner.report = {'run': 'test-run', 'tree': 'new-tree', 'version': 'new', 'checks': []}
        runner.directory = self.root / 'build'
        runner.directory.mkdir()
        runner.source = lambda: self.root
        return runner

    def test_failed_check_never_stops_live_service(self):
        runner = self.runner()
        runner.check = Mock(side_effect=RuntimeError('tests failed'))
        with self.assertRaisesRegex(RuntimeError, 'tests failed'):
            runner.run()
        self.services.assert_not_called()
        self.assertEqual((self.root / 'current').resolve(), self.old)

    def test_same_tree_checks_live_service_without_restart(self):
        (self.old / 'release.json').write_text(json.dumps({'built_version': 'old', 'tree': 'new-tree'}))
        runner = self.runner()
        runner.check = Mock()
        runner.run()
        self.services.assert_not_called()
        runner.check.assert_not_called()
        self.accept.assert_called_once_with('old')
        self.assertTrue(runner.report['no_changes'])


if __name__ == '__main__':
    unittest.main()
