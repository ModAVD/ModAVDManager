"""Run #95 cross-PR regressions against an explicitly selected integration tree.

Requires an existing Windows Go toolchain, Node, and SDK qemu-img. Does not fetch,
build an emulator, start adb, or modify the source tree. Keeps all run evidence.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


def replace_once(text: str, old: str, new: str) -> str:
    if text.count(old) != 1:
        raise ValueError(f"expected one source marker: {old!r}")
    return text.replace(old, new, 1)


def test_events(text: str) -> list[dict]:
    return [json.loads(line) for line in text.splitlines() if line.startswith('{')]


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', type=Path, required=True, help='tree containing Manager #56, #59, #60, #63, #64, #65')
    p.add_argument('--sdk', type=Path, required=True, help='existing SDK; qemu-img and DLLs are copied')
    p.add_argument('--out', type=Path, required=True, help='new evidence directory, never reused or deleted')
    p.add_argument('--go', default='go', help='existing go1.26.5 executable, no automatic download')
    p.add_argument('--mutations', action='store_true', help='also prove the added tests reject interaction regressions')
    args = p.parse_args()
    if os.name != 'nt':
        p.error('these process, file identity and link checks require Windows')
    src, sdk, out = args.source.resolve(), args.sdk.resolve(), args.out.resolve()
    if out == src or src in out.parents:
        p.error('evidence must be outside the selected source tree')
    scripts = Path(__file__).resolve().parent
    prerequisites = ['internal/emu/disk_links_windows.go', 'internal/emu/disk_lock.go',
                     'internal/emu/qemu_img_lifetime_windows_test.go', 'internal/emu/compact_policy.go',
                     'internal/emu/compact_budget_integration_test.go', 'scripts/test_measure_trim.py']
    for name in prerequisites:
        if not (src / name).is_file():
            p.error(f'missing PR prerequisite: {name}')
    if not (sdk / 'emulator/qemu-img.exe').is_file():
        p.error('SDK must already contain emulator/qemu-img.exe')
    env = {**os.environ, 'GOTOOLCHAIN': 'local', 'GOPROXY': 'off', 'GOENV': 'off',
           'GOFLAGS': '', 'GOWORK': 'off', 'GOOS': 'windows', 'GOARCH': 'amd64'}
    want = next(line.split()[1] for line in (src / 'go.mod').read_text().splitlines() if line.startswith('toolchain '))
    version = subprocess.check_output([args.go, 'env', 'GOVERSION'], env=env, text=True).strip()
    if version != want:
        p.error(f'need existing {want}, got {version}; pass --go with its executable path')
    out.mkdir(parents=True, exist_ok=False)
    emulator = out / 'sdk/emulator'
    emulator.mkdir(parents=True)
    for f in [sdk / 'emulator/qemu-img.exe', *(sdk / 'emulator').glob('*.dll')]:
        shutil.copyfile(f, emulator / f.name)
    temp = out / 'temp'
    temp.mkdir()
    env.update(MODAVD_TEST_SDK_ROOT=str(emulator.parent), TEMP=str(temp), TMP=str(temp),
               MODAVD_COMBINED_SOURCE=str(src))
    # Use testing.TempDir, or #51's testutil when present, with a private OS temp
    # root. Older opsTestTempDir ignores cleanup errors when MODAVD_TEST_TMP is set.
    env.pop('MODAVD_TEST_TMP', None)
    env.pop('MODAVD_TEST_COMPACT_SHORT_BUDGET', None)
    fixture_dir = scripts / 'testdata/disk_combined'
    replacements = {str(src / 'internal/emu' / f.name): str(f) for f in fixture_dir.glob('*.go')}
    def overlay(name: str, extra: dict | None = None) -> Path:
        path = out / f'{name}-overlay.json'
        path.write_text(json.dumps({'Replace': {**replacements, **(extra or {})}}, indent=2), encoding='utf-8')
        return path
    def run(name: str, cmd: list[str], *, extra_env: dict | None = None, failure: bool = False) -> list[dict]:
        log = out / f'{name}.log'
        with log.open('w', encoding='utf-8') as stream:
            completed = subprocess.run(cmd, cwd=src, env={**env, **(extra_env or {})}, stdout=stream,
                                       stderr=subprocess.STDOUT, timeout=180)
        text = log.read_text(encoding='utf-8')
        events = test_events(text)
        failed_test = any(e.get('Action') == 'fail' and e.get('Test') for e in events)
        if failure:
            # A compilation error or unrelated directory cleanup error is not a
            # successful mutation check. Require an interaction assertion.
            markers = ('wanted prompt compact busy', 'link bypassed by skip/lock',
                       'parent did not finish', 'child survived parent/cancellation')
            if not completed.returncode or not failed_test or not any(m in text for m in markers):
                raise RuntimeError(f'{name}: expected an interaction assertion failure; see {log}')
        elif completed.returncode:
            raise RuntimeError(f'{name}: exit {completed.returncode}; see {log}')
        print(f'{name}: {"regression detected" if failure else "passed"}', flush=True)
        return events
    def go(ov: Path, pattern: str, packages: list[str] | None = None) -> list[str]:
        return [args.go, 'test', f'-overlay={ov}', '-gcflags=modavd/internal/win=-d=checkptr=2',
                '-count=1', '-json', f'-run={pattern}', *(packages or ['./internal/emu'])]
    def git(*command: str) -> str:
        return subprocess.check_output(['git', *command], cwd=src, text=True, encoding='utf-8').strip()
    manifest = {'source_head': git('rev-parse', 'HEAD'), 'status': git('status', '--porcelain'),
                'go': version, 'qemu_sha256': hashlib.sha256((emulator / 'qemu-img.exe').read_bytes()).hexdigest(),
                'fixtures': {f.name: hashlib.sha256(f.read_bytes()).hexdigest() for f in fixture_dir.glob('*.go')}}
    source_files = [*prerequisites, 'internal/emu/disk.go', 'internal/win/bound_windows.go',
                    'internal/ui/web/js/actions.js', 'internal/ui/web/js/panels.js', 'internal/ui/web/js/format.js']
    manifest['sources'] = {name: hashlib.sha256((src / name).read_bytes()).hexdigest() for name in source_files}
    (out / 'manifest.json').write_text(json.dumps(manifest, indent=2), encoding='utf-8')
    run('qemu-version', [str(emulator / 'qemu-img.exe'), '--version'])
    normal = overlay('normal')
    pattern = '^TestCombined|^TestCompactRealCompressedSkip$|^TestDiskCompactIntegration$|^TestCompactBudgetMissingAndGrowth$|^TestCompactResult|^TestDiskJobs$|^TestQemuImgLifetime'
    events = run('combined', go(normal, pattern, ['./internal/emu', './internal/cli', './internal/mcp', './internal/ui']))
    required = ['TestCombinedCompactInterruption/kill/convert', 'TestCombinedCompactInterruption/kill/compare',
                'TestCombinedCompactInterruption/cancel/convert', 'TestCombinedCompactInterruption/cancel/compare',
                'TestCombinedLinkedLowBenefit/hardlink', 'TestCombinedLinkedLowBenefit/symlink',
                'TestDiskCompactIntegration/BackingData', 'TestCompactRealCompressedSkip',
                'TestCompactResultCLI', 'TestCompactResultMCP', 'TestCompactResultGUI']
    passed = {e.get('Test') for e in events if e.get('Action') == 'pass'}
    if set(required) - passed:
        raise RuntimeError(f'required tests did not pass (skips do not count): {set(required) - passed}')
    policy = src / 'internal/emu/compact_policy.go'
    changed = out / 'short-budget.go'
    changed.write_text(replace_once(policy.read_text(encoding='utf-8'), 'work := max(virtual, allocated, onDisk)',
                                   'return 5 * time.Second\n\twork := max(virtual, allocated, onDisk)'), encoding='utf-8')
    short = overlay('short-budget', {str(policy): str(changed)})
    timed = run('timeouts', go(short, '^TestCombinedCompactInterruption/timeout'), extra_env={'MODAVD_TEST_COMPACT_SHORT_BUDGET': '1'})
    for stage in ['convert', 'compare']:
        if not any(e.get('Action') == 'pass' and e.get('Test') == f'TestCombinedCompactInterruption/timeout/{stage}' for e in timed):
            raise RuntimeError(f'timeout/{stage} did not run')
    run('ui', ['node', '--test', str(scripts / 'test_disk_combined_ui.mjs')])
    if args.mutations:
        disk = src / 'internal/emu/disk.go'
        source = disk.read_text(encoding='utf-8')
        mutations = [
            ('lost-link-check', disk, replace_once(source, 'if err := checkCompactOverlayLinks(overlay, st); err != nil {',
                 'if err := error(nil); err != nil {'), '^TestCombinedLinkedLowBenefit/hardlink'),
            ('lost-disk-lock', disk, replace_once(source, '{"disk", "正在整理磁盘"},', ''), '^TestCombinedCompactInterruption/cancel/convert'),
        ]
        bound = src / 'internal/win/bound_windows.go'
        old = bound.read_text(encoding='utf-8')
        new, count = re.subn(r'(jobObjectLimitKillOnJobClose\s*=\s*)0x2000', r'\g<1>0', old)
        if count != 1:
            raise RuntimeError('kill-on-close source marker changed')
        mutations.append(('lost-job-lifetime', bound, new, '^TestCombinedCompactInterruption/kill/convert'))
        for name, target, body, pattern in mutations:
            file = out / f'{name}.go'
            file.write_text(body, encoding='utf-8')
            run(name, go(overlay(name, {str(target): str(file)}), pattern), failure=True)
    (out / 'result.json').write_text(json.dumps({'passed': True, 'required_tests': required,
        'scaled_timeout_stages': ['convert', 'compare'], 'mutations': args.mutations}, indent=2), encoding='utf-8')


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
