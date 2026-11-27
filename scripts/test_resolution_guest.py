"""Run display regression checks on an isolated Windows root with a private SDK.

python scripts/test_resolution_guest.py --root C:/resolution-lab --legacy-cli C:/old/tools/modavd.exe --out C:/evidence/display.json

The root must start without instances, use non-default private ports, and contain a
complete SDK. The script keeps its instances for inspection and shuts them down.
Only query forms of wm size/density are used. No guest display overrides are set.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import subprocess


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--legacy-cli", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    meta = json.loads((root / "mas-avd.json").read_text("utf-8"))
    assert meta.get("portBase") != 20000 and meta.get("portBase"), "use a private port plan"
    assert meta.get("adbServerPort") not in (None, 20050, 5037)
    assert meta.get("scriptAdbServerPort") not in (None, 20049, 5037)
    assert not meta.get("sdkRoot"), "SDK must be copied into this isolated root"
    assert (root / "sdk/emulator/emulator.exe").is_file()
    assert not list((root / "avd").glob("mas_*")), "use a fresh root without instances"
    cli = root / "tools/modavd.exe"
    records: list[dict] = []
    args.out.parent.mkdir(parents=True, exist_ok=True)

    def call(*command: str, exe: Path = cli) -> dict:
        run = subprocess.run([str(exe), "--root", str(root), "--json", *command],
                             capture_output=True, text=True, encoding="utf-8", timeout=1500)
        records.append({"command": list(command), "legacy": exe != cli,
                        "code": run.returncode, "stdout": run.stdout, "stderr": run.stderr})
        args.out.write_text(json.dumps(records, ensure_ascii=False, indent=2) + "\n", "utf-8")
        assert run.returncode == 0, (command, run.stdout, run.stderr)
        result = json.loads(run.stdout)
        assert result["ok"], result
        return result["data"]

    def observe(index: int, width: int, height: int, dpi: int, stage: str) -> None:
        call("start", str(index), "--headless", "--no-bridge")
        output = call("shell", str(index), "wm size; wm density; dumpsys display")["output"]
        assert f"Physical size: {width}x{height}" in output, output
        assert f"Physical density: {dpi}" in output, output
        assert "Override size:" not in output and "Override density:" not in output, output
        # WindowManager query plus the guest's actual DisplayInfo, independent of config.ini.
        assert re.search(rf"(?:real|supportedModes=.*?width=)\s*{width}(?:\s*x\s*|, height=){height}", output), output
        print(f"{stage}: guest {width}x{height} / DPI {dpi}", flush=True)
        call("stop", str(index))

    try:
        call("create", "--index", "0", "--memory", "3G", "--cpu", "2", "--no-bridge")
        observe(0, 1920, 1080, 280, "new default")
        call("start", "0", "--headless", "--no-bridge")
        update = call("set", "0", "--resolution", "720")
        assert update["applies_next_boot"] and update["instance"]["config"]["resolution"] == 720, update
        live = call("shell", "0", "wm size; wm density")["output"]
        assert "Physical size: 1920x1080" in live and "Physical density: 280" in live, live
        print("running change: guest stays 1920x1080 / DPI 280 until restart", flush=True)
        call("stop", "0")
        observe(0, 1280, 720, 240, "running change after restart")
        call("set", "0", "--resolution", "1080")
        observe(0, 1920, 1080, 280, "restored 1080")
        call("create", "--index", "1", "--memory", "3G", "--cpu", "2", "--no-bridge", exe=args.legacy_cli.resolve(strict=True))
        observe(1, 1280, 720, 240, "old 720 first upgraded boot")
        observe(1, 1280, 720, 240, "old 720 repeated boot")
        call("set", "1", "--resolution", "1080")
        observe(1, 1920, 1080, 280, "720 to 1080")
        call("set", "1", "--resolution", "720")
        observe(1, 1280, 720, 240, "1080 to 720")
        call("create", "--index", "2", "--resolution", "720", "--memory", "3G", "--cpu", "2", "--no-bridge")
        observe(2, 1280, 720, 240, "new explicit 720")
        # Seed a portrait legacy configuration, then change the preset through the CLI.
        cfg_path = root / "avd/mas_2.avd/config.ini"
        cfg = cfg_path.read_text("utf-8").replace("hw.lcd.width=1280", "hw.lcd.width=720").replace("hw.lcd.height=720", "hw.lcd.height=1280")
        cfg_path.write_text(cfg, "utf-8")
        call("set", "2", "--resolution", "1080")
        observe(2, 1080, 1920, 280, "portrait 720 to 1080")
        observe(2, 1080, 1920, 280, "portrait 1080 repeated boot")
        call("set", "2", "--resolution", "720")
        observe(2, 720, 1280, 240, "portrait 1080 to 720")
    finally:
        call("stop", "--all")


if __name__ == "__main__":
    main()
