"""Measure trim requests on an already booted, dedicated test instance. Stdlib only."""
from __future__ import annotations

import argparse
from contextlib import contextmanager
import datetime as dt
import hashlib
import json
import os
import platform
from pathlib import Path
import re
import shlex
import subprocess
import threading
import time
import uuid


def port_plan(meta: dict, index: int) -> tuple[int, int, int]:
    """Reject metadata that could fall back to production ports in the manager."""
    base, port, script_port = (meta.get(key) for key in ("portBase", "adbServerPort", "scriptAdbServerPort"))
    if type(index) is not int or index not in set(range(10)) - {5}:
        raise ValueError("invalid instance index")
    if type(base) is not int or not 1100 <= base <= 65400 or base % 100:
        raise ValueError("portBase must be a multiple of 100 in 1100..65400")
    reserved = {5037, *range(5555, 5586), *range(20000, 20100)}
    if reserved.intersection(range(base, base + 100)):
        raise ValueError("the whole portBase segment must avoid production and adb scan ports")
    instance_ports = {base + 10 * i + offset for i in range(10) if i != 5 for offset in range(3)}
    for name, value in (("adbServerPort", port), ("scriptAdbServerPort", script_port)):
        if type(value) is not int or not 1024 <= value <= 65535 or value in reserved | instance_ports:
            raise ValueError(f"{name} must be explicit, private and outside instance ports")
    if port == script_port:
        raise ValueError("the two adb server ports must differ")
    return base, port, script_port


def listening_ports(ports: set[int]) -> list[dict]:
    # Only validated integers enter the PowerShell code. Root paths never do.
    command = "$ErrorActionPreference='Stop'; $ports=@(" + ",".join(map(str, sorted(ports))) + "); " + r"""
$listeners = @(
    Get-NetTCPConnection -State Listen | Where-Object { $_.LocalPort -in $ports } | ForEach-Object {
        [pscustomobject]@{ Port=$_.LocalPort; Pid=$_.OwningProcess; Protocol='tcp' }
    }
    Get-NetUDPEndpoint | Where-Object { $_.LocalPort -in $ports } | ForEach-Object {
        [pscustomobject]@{ Port=$_.LocalPort; Pid=$_.OwningProcess; Protocol='udp' }
    }
)
@($listeners | ForEach-Object {
    $owner = Get-CimInstance Win32_Process -Filter ('ProcessId = ' + $_.Pid)
    [pscustomobject]@{ port=$_.Port; pid=$_.Pid; protocol=$_.Protocol; executable=$owner.ExecutablePath }
}) | ConvertTo-Json -Compress
"""
    result = run(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command])
    if result["code"] != 0:
        raise RuntimeError("cannot inspect the test port segment: " + result["stderr"])
    rows = json.loads(result["stdout"] or "[]")
    return rows if isinstance(rows, list) else [rows]


def check_listeners(root: Path, base: int, port: int, script_port: int, index: int, listeners: list[dict]) -> list[dict]:
    """Allow only this root's qemu and adb on their assigned TCP ports."""
    qemu_ports = {base + 10 * index + offset for offset in range(3)}
    qemu_dir = root / "sdk/emulator/qemu/windows-x86_64"
    qemus = {(qemu_dir / name).resolve() for name in ("qemu-system-x86_64.exe", "qemu-system-x86_64-headless.exe")}
    adb = (root / "sdk/platform-tools/adb.exe").resolve()
    checked = []
    for listener in listeners:
        number = listener["port"]
        role = "qemu" if number in qemu_ports else "adb" if number in (port, script_port) else None
        path = listener.get("executable")
        if (listener["protocol"] != "tcp" or role is None or not path
                or Path(path).resolve() not in (qemus if role == "qemu" else {adb})):
            raise ValueError(f"test port {number}/{listener['protocol']} is occupied by an unverified process (PID {listener['pid']})")
        checked.append(dict(port=number, pid=listener["pid"], role=role, executable_name=Path(path).name))
    return checked


@contextmanager
def measurement_lock(root: Path):
    lock = root / ".trim-measure.lock"
    # A failed exclusive open never removes another run's lock.
    fd = os.open(lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        yield fd
    finally:
        try:
            os.close(fd)
        finally:
            lock.unlink()


def cleanup_streams(streams):
    errors = []
    for process, thread in streams:
        try:
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        except Exception as error:
            errors.append(error)
        finally:
            thread.join(timeout=2)
    if errors:
        raise RuntimeError("failed to stop a measurement stream") from errors[0]


def data_device(mountinfo: str) -> str:
    mounts = [line.split() for line in mountinfo.splitlines()]
    devices = {m[2] for m in mounts if len(m) > 6 and m[3:5] == ["/", "/data"]}
    if len(devices) != 1 or not re.fullmatch(r"\d+:\d+", next(iter(devices), "")):
        raise ValueError("cannot identify the /data mount device")
    return devices.pop()


def discard_stats(text: str, devices: set[str]) -> dict:
    result = {}
    for line in text.splitlines():
        f = line.split()
        if len(f) < 3 or f"{f[0]}:{f[1]}" not in devices:
            continue
        # Linux diskstats fields 12..15 follow the original eleven I/O fields.
        # Old kernels have no discard fields. Do not turn missing counters into zero.
        if len(f) < 18:
            result[f"{f[0]}:{f[1]}"] = {"name": f[2], "discard": None}
        else:
            values = [int(x) for x in f[14:18]]
            if any(x < 0 for x in values):
                raise ValueError("negative diskstats counter")
            result[f"{f[0]}:{f[1]}"] = dict(name=f[2], discard=dict(zip(
                ("completed", "merged", "sectors", "milliseconds"), values)))
    return result


LOG = re.compile(r"^\s*(\d+\.\d+)\s+(\d+)\s+(\d+)\s+[VDIWEF]\s+(\S+)\s*:\s*(.*)$")


def trim_evidence(log: str, token: str) -> dict:
    """Require a /data start and explicit success on the same vold thread after our marker.

    An idle-maintenance completion or stable counters alone never establishes /data success.
    Exclusive access to this instance is a measurement precondition, not inferred from logs.
    """
    active = False
    starts, ends, failures, maintenance = [], [], [], []
    for line_number, line in enumerate(log.splitlines(), 1):
        match = LOG.fullmatch(line)
        if not match:
            continue
        stamp, pid, tid, tag, message = match.groups()
        event = {"uptime_seconds": float(stamp), "pid": pid, "tid": tid, "log_line": line_number, "line": line}
        if tag == "ModAVDTrimMeasure" and message == "BEGIN " + token:
            active = True
            starts, ends, failures, maintenance = [], [], [], []
        if tag == "ModAVDTrimMeasure" and message == "END " + token:
            active = False
        if not active or tag != "vold":
            continue
        if message == "Starting trim of /data":
            starts.append(event)
        if re.fullmatch(r"Trimmed \d+ bytes on /data in \d+(?:\.\d+)?ms", message):
            ends.append(event)
        if message.startswith("Trim failed on /data:"):
            failures.append(event)
        if message == "idle maintenance completed":
            maintenance.append(event)
    status = "unknown"
    if len(starts) == 1:
        start = starts[0]
        same = lambda e: ((e["pid"], e["tid"]) == (start["pid"], start["tid"])
                          and e["uptime_seconds"] >= start["uptime_seconds"] and e["log_line"] > start["log_line"])
        if len(failures) == 1 and same(failures[0]) and not ends:
            status = "failed"
        elif len(ends) == 1 and same(ends[0]) and not failures:
            status = "observed_fitrim_return"
    return dict(data_trim_status=status, data_starts=starts, data_returns=ends,
                data_failures=failures, maintenance_completions=maintenance)


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def run(argv: list[str], timeout: float = 30) -> dict:
    started = utc_now()
    before = time.perf_counter()
    try:
        p = subprocess.run(argv, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=timeout)
        return dict(sent_at=started, returned_at=utc_now(), round_trip_seconds=time.perf_counter() - before,
                    code=p.returncode, stdout=p.stdout, stderr=p.stderr)
    except subprocess.TimeoutExpired as e:
        return dict(sent_at=started, returned_at=utc_now(), round_trip_seconds=time.perf_counter() - before,
                    code=None, timeout=True, stdout=(e.stdout or b"").decode("utf-8", "replace"),
                    stderr=(e.stderr or b"").decode("utf-8", "replace"))


def measure(args) -> dict:
    root = args.root.resolve(strict=True)
    meta = json.loads((root / "mas-avd.json").read_text(encoding="utf-8-sig"))
    base, port, script_port = port_plan(meta, args.index)
    listeners = check_listeners(root, base, port, script_port, args.index,
                                listening_ports(set(range(base, base + 100)) | {port, script_port}))
    qemu_processes = {(row["pid"], row["executable_name"]) for row in listeners if row["role"] == "qemu"}
    if len(qemu_processes) != 1:
        raise ValueError("cannot identify one running qemu on this instance's ports")
    _, qemu_name = qemu_processes.pop()
    adb = [str(root / "sdk/platform-tools/adb.exe"), "-P", str(port), "-s", f"emulator-{base + 10 * args.index}"]
    shell = lambda script: adb + ["shell", "su 0 sh -c " + shlex.quote(script)]

    def checked(script):
        r = run(shell(script))
        if r["code"] != 0:
            raise RuntimeError(json.dumps(r))
        return r["stdout"]

    streams = []
    with measurement_lock(root) as fd:
        try:
            os.write(fd, str(os.getpid()).encode())
            mountinfo = checked("cat /proc/self/mountinfo")
            device = data_device(mountinfo)
            pending, graph = [device], {}
            while pending:
                dev = pending.pop()
                if dev in graph:
                    continue
                raw = checked(f"cat /sys/dev/block/{dev}/uevent; echo SLAVES; "
                              f"for s in /sys/dev/block/{dev}/slaves/*; do [ ! -e \"$s\" ] || cat \"$s/dev\"; done; "
                              f"echo LIMIT; cat /sys/dev/block/{dev}/queue/discard_max_bytes")
                props, rest = raw.split("SLAVES\n", 1)
                slaves, limit = rest.split("LIMIT\n", 1)
                children = slaves.split()
                if any(not re.fullmatch(r"\d+:\d+", s) for s in children):
                    raise ValueError("invalid sysfs slave device")
                graph[dev] = dict(uevent=props, slaves=children, discard_max_bytes=int(limit.strip()))
                pending.extend(children)
            before = checked("cat /proc/diskstats")
            report = dict(root_name=root.name, index=args.index, port_base=base, adb_server_port=port,
                          script_adb_server_port=script_port, port_listeners=listeners,
                          command_kind=args.command, write_mib=args.write_mib, started_at=utc_now(),
                          mountinfo=mountinfo, data_device=device, device_graph=graph,
                          boot_id=checked("cat /proc/sys/kernel/random/boot_id").strip(),
                          kernel=checked("uname -a"), diskstats_before=discard_stats(before, set(graph)))
            report["host"] = dict(platform=platform.platform(), python=platform.python_version())
            report["binaries"] = {}
            manager = args.manager or root / "tools/modavd.exe"
            for name, path in {"manager": manager, "adb": Path(adb[0]),
                               "qemu": root / "sdk/emulator/qemu/windows-x86_64" / qemu_name}.items():
                report["binaries"][name] = hashlib.sha256(path.read_bytes()).hexdigest()
            report["package_version"] = json.loads((root / "modavd-manifest.json").read_text(encoding="utf-8-sig"))["version"]
            report["dmesg_before"] = run(shell("dmesg"))
            token = uuid.uuid4().hex
            workdir = "/data/local/tmp/modavd-trim-measure-" + token
            # mkdir must succeed. Never reuse or recursively delete a preexisting directory.
            checked(f"mkdir {workdir}")
            try:
                if args.write_mib:
                    report["workload"] = run(shell(
                        f"dd if=/dev/urandom of={workdir}/payload bs=1048576 count={args.write_mib} conv=fsync && "
                        f"sync && du -k {workdir}/payload && rm {workdir}/payload && sync"), timeout=120)
                    if report["workload"]["code"] != 0:
                        raise RuntimeError("workload did not finish; see output")
                lines, samples = [], []

                def collect(argv, output):
                    p = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                         text=True, encoding="utf-8", errors="replace")
                    def read():
                        with p.stdout:
                            for line in p.stdout:
                                output.append(line)
                    t = threading.Thread(target=read, daemon=True)
                    t.start()
                    streams.append((p, t))
                    return p, t

                collect(shell("exec logcat -b main,system -v monotonic -T 1 -s vold:* ModAVDTrimMeasure:I"), lines)
                checked("log -t ModAVDTrimMeasure " + shlex.quote("BEGIN " + token))
                deadline = time.monotonic() + 5
                while not any("BEGIN " + token in x for x in lines):
                    if time.monotonic() >= deadline:
                        raise RuntimeError("logcat marker not received; no command issued")
                    time.sleep(0.02)
                # Finite guest sampler: a disconnected host cannot leave an unbounded guest loop.
                count = int(args.observe_seconds * 10)
                pattern = " || ".join(f'$1 ":" $2 == "{dev}"' for dev in graph)
                sampler, sample_thread = collect(shell(
                    f"for n in $(seq 1 {count}); do read up rest < /proc/uptime; echo SAMPLE $up; "
                    f"awk '{pattern}' /proc/diskstats; sleep 0.1; done"), samples)
                sample_deadline = time.monotonic() + 5
                while not any(x.startswith("SAMPLE ") for x in samples):
                    if time.monotonic() > sample_deadline:
                        raise RuntimeError("diskstats sampler not ready; no command issued")
                    time.sleep(0.02)
                if args.command == "cli":
                    command = [str(manager), "--root", str(root), "disk", str(args.index), "trim"]
                else:
                    # These uptime stamps bound the guest sm invocation, separate from host adb RTT.
                    command = shell("echo SM_SENT; cat /proc/uptime; sm idle-maint run; rc=$?; "
                                    "echo SM_RETURNED $rc; cat /proc/uptime; exit $rc")
                report["command"] = run(command, timeout=150)
                sampler.wait(timeout=args.observe_seconds + 15)
                sample_thread.join(timeout=2)
                report["sampler_code"] = sampler.returncode
                report["diskstats_samples"] = "".join(samples)
                report["diskstats_after"] = discard_stats(checked("cat /proc/diskstats"), set(graph))
                checked("log -t ModAVDTrimMeasure " + shlex.quote("END " + token))
                deadline = time.monotonic() + 5
                while not any("END " + token in x for x in lines) and time.monotonic() < deadline:
                    time.sleep(0.02)
                report["vold_log"] = "".join(lines)
                report["evidence"] = trim_evidence(report["vold_log"], token)
                report["dmesg_after"] = run(shell("dmesg"))
                report["same_boot"] = checked("cat /proc/sys/kernel/random/boot_id").strip() == report["boot_id"]
                if not report["same_boot"]:
                    report["evidence"]["data_trim_status"] = "unknown"
                report["finished_at"] = utc_now()
                return report
            finally:
                run(shell(f"rm -f {workdir}/payload; rmdir {workdir}"))
        finally:
            cleanup_streams(streams)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--index", type=int, default=0)
    parser.add_argument("--manager", type=Path, help="CLI built from the source under test")
    parser.add_argument("--command", choices=("cli", "sm"), default="cli")
    parser.add_argument("--write-mib", type=int, choices=range(0, 257), default=0, metavar="0..256")
    parser.add_argument("--observe-seconds", type=int, choices=range(2, 61), default=10, metavar="2..60")
    parser.add_argument("--exclusive-test-instance", action="store_true", required=True,
                        help="acknowledge this is a dedicated instance with no other maintenance, installs, updates or shutdown")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists():
        parser.error("output already exists")
    report = measure(args)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("x", encoding="utf-8") as output:
        output.write(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(dict(command=report["command_kind"], write_mib=report["write_mib"],
                         round_trip_seconds=report["command"]["round_trip_seconds"],
                         command_code=report["command"]["code"], **report["evidence"]), ensure_ascii=False))


if __name__ == "__main__":
    main()
