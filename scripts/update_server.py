#!/usr/bin/env python3
"""Update the custom Magpie installation on 100.100.1.4, with rollback."""
import argparse
import datetime
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path('/opt/magpie')
MAINT = ROOT / 'maintenance'
CONFIG = Path('/var/lib/magpie/.config/magpie')
ENVFILE = Path('/etc/magpie.env')
UNITFILE = Path('/etc/systemd/system/magpie.service')
ACCESSFILE = Path('/root/magpie-access.txt')
USER = 'magpie-build'
GO = ROOT / 'tools/go1.26.3/bin/go'
BUN = ROOT / 'tools/bun-linux-x64-baseline/bun'
REPOSITORY = 'https://github.com/hamajun-tao/magpie.git'


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def private_json(path, value):
    temp = path.with_name(path.name + '.' + uuid.uuid4().hex)
    temp.write_text(json.dumps(value, indent=2, ensure_ascii=False) + '\n')
    temp.chmod(0o600)
    os.replace(temp, path)


def switch_release(release):
    release = release.resolve(strict=True)
    if release.parent != (ROOT / 'releases').resolve() or not (release / 'magpie').is_file():
        raise RuntimeError('refusing a release outside /opt/magpie/releases')
    link = ROOT / ('.current-' + uuid.uuid4().hex)
    link.symlink_to(release)
    os.replace(link, ROOT / 'current')


def service(action):
    subprocess.run(['systemctl', action, 'magpie.service'], check=True, timeout=60)


def request(opener, url, token=None):
    headers = {'Authorization': 'Bearer ' + token} if token else {}
    try:
        with opener.open(urllib.request.Request(url, headers=headers), timeout=10) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def verify(version):
    """Read credentials locally; never put them in output or subprocess arguments."""
    env = dict(line.split('=', 1) for line in ENVFILE.read_text().splitlines() if '=' in line)
    access = ACCESSFILE.read_text()
    token = next(line.split('：', 1)[1] for line in access.splitlines() if line.startswith('API Key：'))
    web = 'http://100.100.1.4:3430'
    api = 'http://100.100.1.4:3425/v1/models'
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    assert request(opener, web + '/')[0] == 401, 'management auth failed'
    assert request(opener, api)[0] == 401, 'gateway auth failed'
    assert request(opener, api, 'invalid-update-check')[0] == 401, 'invalid token accepted'
    code, body = request(opener, api, token)
    assert code == 200 and isinstance(json.loads(body)['data'], list), 'model catalog failed'
    jar = http.cookiejar.CookieJar()
    auth = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))
    # urllib includes the URL in network exceptions, so callers must not log exceptions here.
    assert request(auth, web + '/?k=' + env['MAGPIE_WEB_KEY'])[0] == 200, 'management login failed'
    code, body = request(auth, web + '/api/settings')
    assert code == 200 and json.loads(body)['version'] == version, 'wrong running version'
    code, body = request(auth, web + '/api/providers')
    assert code == 200, 'providers page failed'
    json.loads(body)


def wait_verified(version):
    for _ in range(12):
        try:
            verify(version)
            return
        except Exception:
            time.sleep(2)
    raise RuntimeError('deployment acceptance failed; credentials and URLs omitted')


def protected_hashes():
    return {name: digest(CONFIG / name) for name in ('providers.json', 'caller-keys.json', 'settings.json')}


def health_policy():
    state = json.loads((CONFIG / 'model-health.json').read_text())
    return {name: state[name] for name in ('enabled', 'intervalMinutes', 'failureThreshold')}


def copy_state(source, target):
    """Copy the full config tree and retain the service user's ownership."""
    shutil.copytree(source, target, symlinks=True)
    for original in [source, *source.rglob('*')]:
        copied = target / original.relative_to(source)
        state = original.lstat()
        os.lchown(copied, state.st_uid, state.st_gid)


def restore_state(backup):
    restored = CONFIG.with_name('.magpie-restore-' + uuid.uuid4().hex)
    failed = CONFIG.with_name('.magpie-failed-' + uuid.uuid4().hex)
    copy_state(backup / 'config', restored)
    os.replace(CONFIG, failed)
    try:
        os.replace(restored, CONFIG)
    except BaseException:
        os.replace(failed, CONFIG)
        raise
    # Keep the failed version's state for diagnosis, behind the private backup directory.
    failed.chmod(0o700)
    owner = backup.stat()
    os.chown(failed, owner.st_uid, owner.st_gid)
    shutil.move(str(failed), backup / 'failed-config')
    shutil.copy2(backup / 'magpie.env', ENVFILE)


def deploy(release, report):
    """Stop only after checks pass; restore config and binary on any switch failure."""
    previous = (ROOT / 'current').resolve(strict=True)
    backup = ROOT / 'backups' / report['run']
    backup.mkdir(parents=True, mode=0o700)
    report.update(previous_release=str(previous), backup=str(backup), deployed=False)
    stopped = False
    snapshot = False
    try:
        stopped = True
        service('stop')
        # Capture the stopped state, including health records, without serializing keys to logs.
        copy_state(CONFIG, backup / 'config')
        shutil.copy2(ENVFILE, backup / 'magpie.env')
        shutil.copy2(UNITFILE, backup / 'magpie.service')
        shutil.copy2(ACCESSFILE, backup / 'access.txt')
        snapshot = True
        # User edits made while checks ran are included in the stopped snapshot.
        before = protected_hashes()
        policy = health_policy()
        switch_release(release)
        service('start')
        wait_verified(report['version'])
        assert protected_hashes() == before, 'provider, gateway key or settings changed during update'
        assert health_policy() == policy, 'model health policy changed during update'
        health = json.loads((CONFIG / 'model-health.json').read_text())
        report.update(deployed=True, accepted=True, config_preserved=True,
                      health_enabled=health['enabled'], binary_sha256=digest(release / 'magpie'))
        private_json(ROOT / 'last-update.json', report)
    except BaseException:
        if stopped:
            service('stop')
            switch_release(previous)
            if snapshot:
                restore_state(backup)
            service('start')
            previous_version = json.loads((previous / 'release.json').read_text())['built_version']
            wait_verified(previous_version)
            report['rolled_back'] = True
        raise


class Update:
    def __init__(self):
        self.run_id = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ-') + uuid.uuid4().hex[:8]
        owner = pwd.getpwnam(USER)
        runs = MAINT / 'runs'
        runs.mkdir(exist_ok=True, mode=0o750)
        runs.chmod(0o750)
        os.chown(runs, owner.pw_uid, owner.pw_gid)
        self.directory = MAINT / 'runs' / self.run_id
        self.directory.mkdir(mode=0o750)
        self.directory.chmod(0o750)
        os.chown(self.directory, owner.pw_uid, owner.pw_gid)
        self.report = {'run': self.run_id, 'repository': REPOSITORY, 'branch': 'custom', 'checks': []}
        self.log = (self.directory / 'update.log').open('w')
        self.environment = {
            'PATH': str(GO.parent) + ':/usr/local/bin:/usr/bin:/bin',
            'HOME': str(MAINT / 'home'), 'GOPATH': str(MAINT / 'gopath'),
            'GOMODCACHE': str(MAINT / 'mod'), 'GOCACHE': str(MAINT / 'cache'),
            'XDG_CONFIG_HOME': str(MAINT / 'home/.config'),
            'XDG_CACHE_HOME': str(MAINT / 'home/.cache'),
            'GOTOOLCHAIN': 'local', 'GOMAXPROCS': '2', 'MAGPIE_BUN': str(BUN),
            'GIT_TERMINAL_PROMPT': '0', 'LANG': 'C.UTF-8',
        }

    def build_user(self, args, cwd=None, extra=None, capture=False):
        env = self.environment | (extra or {})
        command = ['runuser', '-u', USER, '--', 'env', '-i', *[k + '=' + v for k, v in env.items()], *map(str, args)]
        result = subprocess.run(command, cwd=cwd, text=True, timeout=1800,
                                stdout=subprocess.PIPE if capture else self.log, stderr=self.log)
        if result.returncode:
            raise RuntimeError('check or merge failed; see ' + str(self.directory / 'update.log'))
        return result.stdout.strip() if capture else None

    def source(self):
        repo = MAINT / 'repo'
        if not repo.exists():
            self.build_user(['git', 'clone', '--no-tags', '--branch', 'custom', REPOSITORY, repo])
        self.build_user(['git', 'fetch', '--no-tags', 'origin',
                         '+refs/heads/custom:refs/remotes/origin/custom',
                         '+refs/heads/main:refs/remotes/origin/main'], cwd=repo)
        custom = self.build_user(['git', 'rev-parse', 'origin/custom'], cwd=repo, capture=True)
        main = self.build_user(['git', 'rev-parse', 'origin/main'], cwd=repo, capture=True)
        source = self.directory / 'source'
        self.build_user(['git', 'worktree', 'add', '--detach', source, custom], cwd=repo)
        self.build_user(['git', '-c', 'user.name=Magpie updater', '-c', 'user.email=magpie-updater@localhost',
                         'merge', '--no-ff', '--no-edit', main], cwd=source)
        commit = self.build_user(['git', 'rev-parse', 'HEAD'], cwd=source, capture=True)
        tree = self.build_user(['git', 'rev-parse', 'HEAD^{tree}'], cwd=source, capture=True)
        self.report.update(custom=custom, main=main, commit=commit, tree=tree, version='custom-' + commit[:12])
        required = ['model_health_cli.go', 'internal/provider/model_health.go',
                    'internal/provider/model_health_test.go', 'internal/gateway/model_health_test.go']
        if any(not (source / path).is_file() for path in required):
            raise RuntimeError('custom model checks are missing; refusing official-only deployment')
        return source

    def check(self, name, args, source, extra=None):
        print(name + ' …', flush=True)
        self.build_user(args, cwd=source, extra=extra)
        self.report['checks'].append(name)

    def check_gui(self, source, current):
        changed = self.build_user(['git', 'diff', '--name-only', current['commit'], 'HEAD', '--',
                                   'internal/gui'], cwd=source, capture=True).splitlines()
        tests = {name for name in changed if name.startswith('internal/gui/tests/')
                 and name.endswith('.test.cjs') and (source / name).is_file()}
        if any(name.startswith('internal/gui/assets/') for name in changed):
            tests.update('internal/gui/tests/' + name + '.test.cjs' for name in ('gui-ja', 'gui-de'))
        if tests:
            self.check('Changed GUI tests (Chromium and WebKit, including locales)',
                       ['/usr/bin/node', '--test', '--test-concurrency=2', *sorted(tests)], source,
                       {'NODE_PATH': str(MAINT / 'ui/node_modules')})

    def run(self, check_only=False):
        source = self.source()
        current = json.loads((ROOT / 'current/release.json').read_text())
        if current.get('tree') == self.report['tree'] and not check_only:
            wait_verified(current['built_version'])
            self.report.update(no_changes=True, accepted=True)
            print('已是同一份代码，服务验收通过，无需重启。', flush=True)
            return
        self.check('Workflow rollback tests', ['python3', '-m', 'unittest', 'discover', '-s', 'scripts/tests', '-v'], source)
        self.check('Full Go suite', [GO, 'test', '-p', '2', '-tags', 'nogui', './...', '-count=1', '-timeout=10m'], source)
        self.check('Model checks race ×20', [GO, 'test', '-p', '2', '-race', '-tags', 'nogui',
                   './internal/provider', './internal/gateway', '-run', 'TestModelHealth|TestResponsesProbesUseArrayInput',
                   '-count=20', '-timeout=10m'], source)
        self.check('Go vet', [GO, 'vet', '-p', '2', '-tags', 'nogui', './...'], source)
        self.check_gui(source, current)
        binary = self.directory / 'magpie'
        self.check('Linux build', [GO, 'build', '-p', '2', '-tags', 'nogui', '-ldflags',
                   '-X main.version=' + self.report['version'], '-o', binary, '.'], source, {'CGO_ENABLED': '0'})
        self.check('Windows build', [GO, 'build', '-p', '2', '-o', self.directory / 'magpie.exe', '.'],
                   source, {'GOOS': 'windows', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'})
        if check_only:
            print('所有检查通过；未部署。', flush=True)
            return
        release = ROOT / 'releases' / (self.report['version'] + '-' + self.run_id)
        release.mkdir(mode=0o755)
        release.chmod(0o755)
        if binary.is_symlink() or not binary.is_file():
            raise RuntimeError('build output must be a regular file')
        shutil.copy2(binary, release / 'magpie')
        (release / 'magpie').chmod(0o755)
        scripts = release / 'scripts'
        scripts.mkdir(mode=0o755)
        scripts.chmod(0o755)
        shutil.copy2(source / 'scripts/update_server.py', scripts / 'update_server.py')
        (scripts / 'update_server.py').chmod(0o755)
        private_json(release / 'release.json', self.report | {'built_version': self.report['version'],
                     'binary_sha256': digest(release / 'magpie')})
        print('检查通过，正在备份、切换版本和验收 …', flush=True)
        deploy(release, self.report)
        print('更新和验收完成：' + self.report['version'], flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check-only', action='store_true', help='fetch, merge, test and build without deployment')
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('run as root or with sudo')
    os.umask(0o077)
    import fcntl
    with (MAINT / 'update.lock').open('w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            parser.error('another update is in progress')
        update = Update()
        try:
            update.run(args.check_only)
        except Exception as error:
            update.report.update(failed=True, error=str(error))
            print('更新停止：' + str(error), file=sys.stderr)
            if update.report.get('rolled_back'):
                print('旧版本和原配置已恢复，旧服务验收通过。', file=sys.stderr)
            return 1
        finally:
            private_json(update.directory / 'report.json', update.report)
            update.log.close()
    return 0


if __name__ == '__main__':
    sys.exit(main())
