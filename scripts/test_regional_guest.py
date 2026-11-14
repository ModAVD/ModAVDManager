#!/usr/bin/env python3
# Copyright 2026 jinghero. All rights reserved.
"""Verify Windows regional settings on a fresh, disposable Android 14 root with its own copied SDK."""

import argparse
import json
import os
from pathlib import Path
import subprocess

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--legacy-cli', required=True, type=Path)
    parser.add_argument('--lock', required=True, type=Path, help='exclusive handle shared with other native acceptance tasks')
    parser.add_argument('--go', default='go')
    args = parser.parse_args()
    source = Path(__file__).resolve().parent.parent
    test_root = args.root.resolve(strict=True)
    if not test_root.is_relative_to(source/'out'):
        raise SystemExit('test root must be under this checkout/out')
    meta = json.loads((test_root/'mas-avd.json').read_text('utf-8-sig'))
    assert meta.get('shareDefaultAdbServer') is False, 'must explicitly opt out of default 5037 registration'
    assert meta.get('portBase') == 34100 and meta.get('adbServerPort') == 34198 and meta.get('scriptAdbServerPort') == 34199
    assert not meta.get('sdkRoot') and (test_root/'sdk/emulator/emulator.exe').is_file(), 'copy the SDK into the test root'
    sdk = test_root/'sdk'
    assert sdk.resolve(strict=True).is_relative_to(test_root), 'SDK must not be linked to a shared installation'
    for path in sdk.rglob('*'):
        if path.is_symlink() or getattr(path, 'is_junction', lambda: False)():
            assert path.resolve(strict=True).is_relative_to(test_root), 'SDK links must remain in this test root'
    assert not list((test_root/'avd').glob('mas_*')), 'use a fresh root without instances'
    env = os.environ.copy()
    temp = test_root.parent/'regional-env'
    for name in ['tmp', 'local', 'go-cache']:
        (temp/name).mkdir(parents=True, exist_ok=True)
    env.update(TEMP=str(temp/'tmp'), TMP=str(temp/'tmp'), LOCALAPPDATA=str(temp/'local'), GOCACHE=str(temp/'go-cache'),
               MODAVD_REGIONAL_ROOT=str(test_root), MODAVD_REGIONAL_LOCK=str(args.lock.resolve()),
               MODAVD_REGIONAL_LEGACY_CLI=str(args.legacy_cli.resolve(strict=True)))
    (test_root/'tools').mkdir(exist_ok=True)
    subprocess.run([args.go, 'build', '-trimpath', '-buildvcs=false', '-o', str(test_root/'tools/modavd.exe'), '.'], cwd=source, env=env, check=True)
    # The Go test acquires FileShare.None before its first native action and releases it after cleanup.
    subprocess.run([args.go, 'test', './internal/emu', '-run', '^TestRegionalGuest$', '-count=1', '-v', '-timeout', '30m'], cwd=source, env=env, check=True)

if __name__ == '__main__':
    main()
