"""Check resolution regressions with Go source overlays, without editing the checkout.

python scripts/test_resolution_guards.py --go <go.exe> --out <result.json>
Each variant must compile and fail its named behavior test.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
CASES = [
    ("new default", "internal/avd/avd.go", "avd", "TestResolutionCreateSwitchAndReboot",
     "DefaultResolution = 1080", "DefaultResolution = 720"),
    ("1080 density", "internal/avd/avd.go", "avd", "TestResolutionCreateSwitchAndReboot",
     "width, height, density = 1920, 1080, 280", "width, height, density = 1920, 1080, 240"),
    ("portrait recognition", "internal/avd/avd.go", "avd", "TestResolutionLegacy",
     "(w == 1920 && h == 1080) || (w == 1080 && h == 1920)", "w == 1920 && h == 1080"),
    ("orientation", "internal/avd/avd.go", "avd", "TestResolutionCreateSwitchAndReboot",
     "if h > w {", "if false && h > w {"),
    ("update persistence", "internal/avd/avd.go", "avd", "TestResolutionCreateSwitchAndReboot",
     "changes = append(changes, displayChanges(in.Config, resolution)...)", "_ = resolution"),
    ("pre-boot preset", "internal/avd/avd.go", "avd", "TestResolutionCreateSwitchAndReboot",
     "displayChanges(cfg, resolutionFor(cfg))", "displayChanges(cfg, 720)"),
    ("CLI create wiring", "internal/cli/commands.go", "cli", "TestBuildCreateRequest",
     'req.Resolution = intPtrOpt(inv, "resolution")', "req.Resolution = nil"),
    ("CLI update wiring", "internal/cli/commands.go", "cli", "TestBuildUpdateRequest",
     'req.Resolution = intPtrOpt(inv, "resolution")', "req.Resolution = nil"),
    ("MCP wiring", "internal/mcp/tools.go", "mcp", "TestToolCalls",
     'a.optInt("resolution")', 'a.optInt("omitted_resolution")'),
    ("HTTP wiring", "internal/ui/handlers.go", "ui", "TestCreateUpdateDelete",
     "Resolution: body.Resolution", "Resolution: nil"),
    ("live display bounds", "internal/ui/device.go", "ui", "TestInputUsesLiveDisplayAfterPresetChange",
     "wd, ht := display.Width, display.Height", "wd, ht := in.Config.Width, in.Config.Height; _ = display"),
]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    results = []
    with tempfile.TemporaryDirectory(prefix="resolution-guards-") as directory:
        scratch = Path(directory)
        for n, (name, source, package, test, before, after) in enumerate(CASES):
            original = ROOT / source
            text = original.read_text("utf-8")
            assert before in text, name
            changed = scratch / f"variant-{n}.go"
            changed.write_text(text.replace(before, after), "utf-8")
            overlay = scratch / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(original): str(changed)}}), "utf-8")
            run = subprocess.run([args.go, "test", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-count=1", "-timeout=45s",
                                  "-overlay", str(overlay), "./internal/" + package, "-run", "^" + test + "$"],
                                 cwd=ROOT, capture_output=True, text=True, encoding="utf-8", timeout=90)
            output = run.stdout + run.stderr
            caught = run.returncode != 0 and f"--- FAIL: {test}" in output and "[build failed]" not in output
            results.append({"variant": name, "test": test, "caught": caught, "output": output})
            print(f"{'caught' if caught else 'MISSED'}: {name}", flush=True)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", "utf-8")
    return 0 if all(r["caught"] for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
