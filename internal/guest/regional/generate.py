#!/usr/bin/env python3
# Copyright 2026 jinghero. All rights reserved.
"""Build the checked-in regional helper using a local JDK and the pinned Google D8 jar."""

import argparse
import hashlib
from pathlib import Path
import shutil
import subprocess
import tempfile

R8_URL = 'https://dl.google.com/dl/android/maven2/com/android/tools/r8/8.3.37/r8-8.3.37.jar'
R8_SHA256 = '59753e70a74f918389cc87f1b7d66b5c0862932559167425708ded159e3de439'

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--r8', required=True, type=Path, help=R8_URL)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    jar = args.r8.resolve(strict=True)
    if hashlib.sha256(jar.read_bytes()).hexdigest() != R8_SHA256:
        raise SystemExit('D8 jar does not match the pinned Google R8 8.3.37')
    here = Path(__file__).resolve().parent
    with tempfile.TemporaryDirectory(prefix='modavd-regional-') as temp:
        work = Path(temp)
        subprocess.run(['javac', '--release', '8', '-Xlint:-options', '-g:none', '-d', temp, str(here/'Regional.java')], check=True)
        subprocess.run(['java', '-cp', str(jar), 'com.android.tools.r8.D8', '--release', '--min-api', '34', '--no-desugaring', '--output', temp, str(work/'org/modavd/regional/Regional.class')], check=True)
        built = (work/'classes.dex').read_bytes()
        target = here/'classes.dex'
        if args.check:
            if built != target.read_bytes():
                raise SystemExit('classes.dex differs from Regional.java; regenerate it')
        else:
            target.write_bytes(built)
        print(f'classes.dex: {len(built)} bytes, sha256 {hashlib.sha256(built).hexdigest()}')

if __name__ == '__main__':
    main()
