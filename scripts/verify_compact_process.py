"""Check scaled process deadlines and fault detection without editing source."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


def replace_once(source: str, old: str, new: str) -> str:
    if source.count(old) != 1:
        raise RuntimeError(f"expected one source marker: {old!r}")
    return source.replace(old, new, 1)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--case", choices=["scaled-deadline", "overflow", "lost-cancel", "lost-growth", "lost-skip", "measure-after-flush"])
    args = parser.parse_args()
    src = Path(__file__).resolve().parent.parent
    target = src / "internal/emu/compact_policy.go"
    original = target.read_text(encoding="utf-8")
    args.out.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOTOOLCHAIN": "local", "GOPROXY": "off"}
    cases = [
        ("scaled-deadline", replace_once(original,
            "work := max(virtual, allocated, onDisk)",
            "return 3 * time.Second\n\twork := max(virtual, allocated, onDisk)"),
         "^TestCompactBudgetShortDeadline$", True),
        ("overflow", replace_once(original, "return time.Duration(math.MaxInt64)", "return floor"),
         "^TestCompactTimeout/saturate$", False),
        ("lost-cancel", replace_once(original, "return context.WithCancel(ctx)",
            "return context.WithCancel(context.Background())"), "^TestCompactBudgetContext$", False),
        ("lost-growth", replace_once(original, "OnDiskDeltaBytes: after - before",
            "OnDiskDeltaBytes: reclaimable(before, after)"), "^TestCompactResultGrowth$", False),
    ]
    disk = src / "internal/emu/disk.go"
    disk_source = disk.read_text(encoding="utf-8")
    cases.append(("lost-skip", replace_once(disk_source,
        "if estimate != nil && *estimate < threshold {", "if false {"),
        "^TestCompactBudgetThresholdPipeline$", False))
    cases.append(("measure-after-flush", replace_once(disk_source,
        "func syncFile(path string) error {", """func syncFile(path string) error {
	defer func() {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil { panic(err) }
		if _, err := f.Write(bytes.Repeat([]byte{0x97}, 1<<20)); err != nil { f.Close(); panic(err) }
		if err := f.Close(); err != nil { panic(err) }
	}()
"""), "^TestCompactBudgetMeasuresAfterSync$", True))
    # Each case runs independently from the committed source. A compile error
    # does not count as detecting a regression; the test itself must fail.
    with tempfile.TemporaryDirectory(prefix="compact-overlay-", dir=args.out) as tmp:
        tmp = Path(tmp)
        for name, changed, pattern, want_pass in cases:
            if args.case and args.case != name:
                continue
            source = disk if name in ("lost-skip", "measure-after-flush") else target
            replacement = tmp / f"{name}.go"
            replacement.write_text(changed, encoding="utf-8")
            overlay = tmp / f"{name}.json"
            overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}), encoding="utf-8")
            run_env = {**env, "MODAVD_TEST_COMPACT_SHORT_BUDGET": "1"} if name == "scaled-deadline" else env
            run = subprocess.run([args.go, "test", f"-overlay={overlay}", "-count=1", "-v",
                                  f"-run={pattern}", "./internal/emu"],
                                 cwd=src, env=run_env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 encoding="utf-8", errors="replace", timeout=90)
            (args.out / f"{name}.log").write_text(run.stdout, encoding="utf-8")
            detected = "--- FAIL:" in run.stdout and run.returncode != 0
            if (want_pass and run.returncode != 0) or (not want_pass and not detected):
                raise RuntimeError(f"{name}: unexpected result\n{run.stdout}")
            print(f"{name}: {'passed' if want_pass else 'regression detected'}")


if __name__ == "__main__":
    main()
