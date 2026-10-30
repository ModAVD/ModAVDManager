# Reference for package avd's tests: AUTO-MAS's instance logic, copied from
# app/utils/common/emulator2/avd/instances.py (and the parts of constants.py, components.py and manager.py it uses)
# so it runs without the app package. Keep it a copy: when AUTO-MAS changes, copy again.
#
# The only change: create_instance_files takes created_at instead of calling datetime.now(), so the output can be
# compared byte for byte.
#
#   python -I instances_ref.py <job.json> <out.json>
#
# job: {"root": <dir>, "ops": [{"op": ..., ...}, ...]}. Each op's result (or error text) is recorded together with a
# snapshot of every file under the root (relative path -> base64 of the bytes).
import base64
import json
import os
import shutil
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

# ---- constants.py ----------------------------------------------------------------------------------------------

PORT_BASE = 20000
PORT_STEP = 10
MAX_NATIVE_INDEX = 9
RESERVED_NATIVE_INDEXES = frozenset({5})
AVD_NAME_PREFIX = "mas_"
AVD_DIR = "avd"
METADATA_FILE = "mas-avd.json"
SYSTEM_IMAGE_SYSDIR = "system-images\\android-34\\google_apis\\x86_64\\"
MEMORY_CHOICES_MB = (3072, 4096, 5120, 6144)
CPU_CHOICES = (2, 4, 6)
DEFAULT_MEMORY_MB = 6144
DEFAULT_CPU = 6
DEFAULT_DATA_PARTITION_GB = 64
DATA_PARTITION_RANGE_GB = (16, 512)
SCREEN_WIDTH, SCREEN_HEIGHT, SCREEN_DENSITY = 1280, 720, 240
FPS_CHOICES = (30, 60)
DEFAULT_FPS = 60


def console_port(native_index):
    return PORT_BASE + PORT_STEP * int(native_index)


def avd_name(native_index):
    return f"{AVD_NAME_PREFIX}{int(native_index)}"


def valid_native_index(native_index):
    return (
        0 <= native_index <= MAX_NATIVE_INDEX
        and native_index not in RESERVED_NATIVE_INDEXES
    )


# ---- components.py ---------------------------------------------------------------------------------------------


def avd_home(root):
    return Path(root) / AVD_DIR


class MetadataUnreadable(RuntimeError):
    pass


def read_metadata(root):
    try:
        return read_metadata_strict(root)
    except MetadataUnreadable:
        return {}


def read_metadata_strict(root):
    path = Path(root) / METADATA_FILE
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, ValueError) as e:
        raise MetadataUnreadable(f"{path} 读不出：{e}") from e
    if not isinstance(data, dict):
        raise MetadataUnreadable(f"{path} 不是 JSON 对象")
    return data


def write_metadata(root, data):
    path = Path(root) / METADATA_FILE
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(".json.tmp")
    temp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    replace_with_retry(temp, path)


def replace_with_retry(source, target):
    deadline = time.monotonic() + 3.0
    while True:
        try:
            os.replace(source, target)
            return
        except PermissionError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.05)


# ---- instances.py ----------------------------------------------------------------------------------------------

_CONFIG_TEMPLATE = (
    ("PlayStore.enabled", "no"),
    ("abi.type", "x86_64"),
    ("avd.ini.encoding", "UTF-8"),
    ("disk.cachePartition", "yes"),
    ("disk.cachePartition.size", "66MB"),
    ("fastboot.forceColdBoot", "yes"),
    ("fastboot.forceFastBoot", "no"),
    ("hw.accelerometer", "yes"),
    ("hw.arc", "false"),
    ("hw.audioInput", "no"),
    ("hw.audioOutput", "no"),
    ("hw.battery", "yes"),
    ("hw.camera.back", "none"),
    ("hw.camera.front", "none"),
    ("hw.cpu.arch", "x86_64"),
    ("hw.dPad", "no"),
    ("hw.gps", "no"),
    ("hw.gpu.enabled", "yes"),
    ("hw.gpu.mode", "host"),
    ("hw.gsmModem", "yes"),
    ("hw.gyroscope", "yes"),
    ("hw.initialOrientation", "landscape"),
    ("hw.keyboard", "yes"),
    ("hw.keyboard.lid", "yes"),
    ("hw.lcd.depth", "32"),
    ("hw.lcd.vsync", "60"),
    ("hw.mainKeys", "yes"),
    ("hw.sdCard", "no"),
    ("hw.sensors.orientation", "yes"),
    ("hw.sensors.proximity", "no"),
    ("hw.trackBall", "no"),
    ("hw.useext4", "yes"),
    ("showDeviceFrame", "no"),
    ("tag.display", "Google APIs"),
    ("tag.id", "google_apis"),
    ("target", "android-34"),
    ("vm.heapSize", "192M"),
)


@dataclass(frozen=True)
class AvdInstance:
    native_index: str
    name: str
    title: str
    config: dict

    @property
    def memory_mb(self):
        return parse_memory_mb(self.config.get("hw.ramSize"))

    @property
    def cpu(self):
        return _int_or_none(self.config.get("hw.cpu.ncore"))

    @property
    def fps(self):
        return _int_or_none(self.config.get("hw.lcd.vsync")) or DEFAULT_FPS

    @property
    def data_partition_gb(self):
        raw = (self.config.get("disk.dataPartition.size") or "").strip().upper()
        if raw.endswith("G"):
            return _int_or_none(raw[:-1])
        if raw.endswith("M"):
            value = _int_or_none(raw[:-1])
            return None if value is None else value // 1024
        return None


def _int_or_none(raw):
    try:
        return int(str(raw).strip())
    except (TypeError, ValueError):
        return None


def parse_memory_mb(raw):
    text = str(raw or "").strip().upper()
    if not text:
        return None
    if text.endswith("G"):
        value = _int_or_none(text[:-1])
        return None if value is None else value * 1024
    if text.endswith("M") or text.endswith("MB"):
        return _int_or_none(text.rstrip("B").rstrip("M"))
    return _int_or_none(text)


def read_ini(path):
    result = {}
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return result
    for line in text.splitlines():
        key, sep, value = line.partition("=")
        if sep and key.strip():
            result[key.strip()] = value.strip()
    return result


def write_ini(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(
        "".join(f"{key}={value}\n" for key, value in data.items()), encoding="utf-8"
    )
    temp.replace(path)


def ini_path(root, native_index):
    return avd_home(root) / f"{avd_name(native_index)}.ini"


def avd_dir(root, native_index):
    return avd_home(root) / f"{avd_name(native_index)}.avd"


def config_path(root, native_index):
    return avd_dir(root, native_index) / "config.ini"


def list_instances(root):
    home = avd_home(root)
    found = {}
    if not home.is_dir():
        return {}
    for ini in home.glob(f"{AVD_NAME_PREFIX}*.ini"):
        suffix = ini.stem[len(AVD_NAME_PREFIX) :]
        if not suffix.isdecimal():
            continue
        index = int(suffix)
        config_file = avd_dir(root, index) / "config.ini"
        if not config_file.is_file():
            continue
        config = read_ini(config_file)
        name = avd_name(index)
        title = config.get("avd.ini.displayname") or name
        found[index] = AvdInstance(str(index), name, title, config)
    return {str(index): found[index] for index in sorted(found)}


def instance_meta(root, native_index):
    instances = read_metadata(root).get("instances") or {}
    meta = instances.get(str(native_index)) if isinstance(instances, dict) else None
    return dict(meta) if isinstance(meta, dict) else {}


def update_instance_meta(root, native_index, **changes):
    data = read_metadata(root)
    instances = data.get("instances")
    if not isinstance(instances, dict):
        instances = {}
    meta = instances.get(str(native_index))
    meta = dict(meta) if isinstance(meta, dict) else {}
    meta.update(changes)
    instances[str(native_index)] = meta
    data["instances"] = instances
    write_metadata(root, data)
    return meta


def remove_instance_meta(root, native_index):
    data = read_metadata(root)
    instances = data.get("instances")
    if isinstance(instances, dict) and str(native_index) in instances:
        del instances[str(native_index)]
        write_metadata(root, data)


def display_changes(config):
    width = _int_or_none(config.get("hw.lcd.width")) or 0
    height = _int_or_none(config.get("hw.lcd.height")) or 0
    if height > width:
        width, height = SCREEN_HEIGHT, SCREEN_WIDTH
    else:
        width, height = SCREEN_WIDTH, SCREEN_HEIGHT
    return {
        "hw.lcd.width": str(width),
        "hw.lcd.height": str(height),
        "hw.lcd.density": str(SCREEN_DENSITY),
        "hw.mainKeys": "yes",
    }


def apply_display(root, native_index):
    path = config_path(root, native_index)
    config = read_ini(path)
    if not config:
        raise RuntimeError(f"实例 {avd_name(native_index)} 的配置读不出")
    changes = display_changes(config)
    if all(config.get(key) == value for key, value in changes.items()):
        return False
    config.update(changes)
    write_ini(path, config)
    return True


def memory_for(meta):
    explicit = meta.get("memoryMb")
    if isinstance(explicit, int) and explicit in MEMORY_CHOICES_MB:
        return explicit
    return DEFAULT_MEMORY_MB


def validate_options(memory_mb, cpu, data_partition_gb):
    memory = DEFAULT_MEMORY_MB if memory_mb is None else int(memory_mb)
    cores = DEFAULT_CPU if cpu is None else int(cpu)
    data_gb = (
        DEFAULT_DATA_PARTITION_GB
        if data_partition_gb is None
        else int(data_partition_gb)
    )
    if memory not in MEMORY_CHOICES_MB:
        raise ValueError(
            "魔改 AVD 的内存只能选 "
            + " / ".join(f"{value // 1024} GB" for value in MEMORY_CHOICES_MB)
        )
    if cores not in CPU_CHOICES:
        raise ValueError(
            "魔改 AVD 的 CPU 核数只能选 " + " / ".join(str(v) for v in CPU_CHOICES)
        )
    low, high = DATA_PARTITION_RANGE_GB
    if not low <= data_gb <= high:
        raise ValueError(f"数据盘上限只能在 {low}–{high} GB 之间")
    return memory, cores, data_gb


def validate_fps(fps):
    value = DEFAULT_FPS if fps is None else int(fps)
    if value not in FPS_CHOICES:
        raise ValueError(
            "魔改 AVD 的帧率只能选 " + " / ".join(str(v) for v in FPS_CHOICES)
        )
    return value


def create_instance_files(
    root, native_index, *, title, memory_mb, cpu, data_partition_gb, fps=DEFAULT_FPS, created_at
):
    if not valid_native_index(native_index):
        raise ValueError(f"原生索引 {native_index} 不可用")
    if ini_path(root, native_index).exists() or avd_dir(root, native_index).exists():
        raise RuntimeError(f"实例 {avd_name(native_index)} 已存在")

    name = avd_name(native_index)
    directory = avd_dir(root, native_index)
    config = dict(_CONFIG_TEMPLATE)
    config.update(
        {
            "avd.id": name,
            "avd.name": name,
            "avd.ini.displayname": title or name,
            "disk.dataPartition.size": f"{data_partition_gb}G",
            "hw.cpu.ncore": str(cpu),
            "hw.ramSize": f"{memory_mb}M",
            "hw.lcd.width": str(SCREEN_WIDTH),
            "hw.lcd.height": str(SCREEN_HEIGHT),
            "hw.lcd.density": str(SCREEN_DENSITY),
            "hw.lcd.vsync": str(fps),
            "image.sysdir.1": SYSTEM_IMAGE_SYSDIR,
        }
    )
    directory.mkdir(parents=True)
    write_ini(directory / "config.ini", config)
    write_ini(
        ini_path(root, native_index),
        {
            "avd.ini.encoding": "UTF-8",
            "path": str(directory),
            "path.rel": f"avd\\{name}.avd",
            "target": "android-34",
        },
    )
    update_instance_meta(
        root,
        native_index,
        memoryMb=memory_mb,
        initialized=False,
        createdAt=created_at,
    )
    return AvdInstance(str(native_index), name, title or name, config)


def write_instance_config(root, native_index, changes):
    path = config_path(root, native_index)
    config = read_ini(path)
    if not config:
        raise RuntimeError(f"实例 {avd_name(native_index)} 的配置读不出")
    config.update(changes)
    write_ini(path, config)


def delete_instance_files(root, native_index):
    directory = avd_dir(root, native_index)
    if directory.exists():
        shutil.rmtree(directory)
    ini_path(root, native_index).unlink(missing_ok=True)
    remove_instance_meta(root, native_index)


# ---- manager.py ------------------------------------------------------------------------------------------------


def create_instance(root, index, title, memory_mb, cpu, data_gb, fps, created_at):
    """AvdManager.create_instance with a given index (the busy-port search is tested separately)."""
    memory, cores, data = validate_options(memory_mb, cpu, data_gb)
    refresh = validate_fps(fps)
    create_instance_files(
        root, int(index), title=title, memory_mb=memory, cpu=cores, data_partition_gb=data, fps=refresh,
        created_at=created_at,
    )
    return str(index)


def write_instance_settings(root, idx, changes):
    """AvdManager.write_instance_settings for cpu / memoryMb / fps (no display keys, no expected values)."""
    instance = list_instances(root).get(str(idx))
    if instance is None:
        raise RuntimeError(f"魔改 AVD 实例 {idx} 不存在")
    fps = validate_fps(changes["fps"]) if "fps" in changes else None
    memory, cpu, _ = validate_options(
        changes.get("memoryMb", memory_for(instance_meta(root, idx))),
        changes.get("cpu", instance.cpu),
        instance.data_partition_gb,
    )
    config_changes = {"hw.ramSize": f"{memory}M", "hw.cpu.ncore": str(cpu)}
    if fps is not None:
        config_changes["hw.lcd.vsync"] = str(fps)
    write_instance_config(root, idx, config_changes)
    update_instance_meta(root, idx, memoryMb=memory)
    return {k: v for k, v in changes.items() if k in ("cpu", "memoryMb", "fps")}


# ---- driver ----------------------------------------------------------------------------------------------------


def snapshot(root):
    tree = {}
    for dirpath, _dirs, files in os.walk(root):
        for f in files:
            full = os.path.join(dirpath, f)
            rel = os.path.relpath(full, root)
            with open(full, "rb") as fh:
                tree[rel] = base64.b64encode(fh.read()).decode("ascii")
    return tree


def describe(root):
    out = []
    for idx, inst in list_instances(root).items():
        display = display_changes(inst.config)
        out.append(
            {
                "index": int(idx),
                "name": inst.name,
                "title": inst.title,
                "memory_mb": inst.memory_mb,
                "cpu": inst.cpu,
                "fps": inst.fps,
                "data_partition_gb": inst.data_partition_gb,
                "display": [int(display["hw.lcd.width"]), int(display["hw.lcd.height"]),
                            int(display["hw.lcd.density"])],
                "memory_for": memory_for(instance_meta(root, idx)),
                "keys": list(inst.config.keys()),
            }
        )
    return out


def run(root, op):
    kind = op["op"]
    if kind == "create":
        return create_instance(
            root, op["index"], op.get("title"), op.get("memory_mb"), op.get("cpu"), op.get("data_gb"),
            op.get("fps"), op["created_at"],
        )
    if kind == "update":
        return write_instance_settings(root, op["index"], op["changes"])
    if kind == "apply_display":
        return apply_display(root, op["index"])
    if kind == "delete":
        return delete_instance_files(root, op["index"])
    if kind == "list":
        return describe(root)
    if kind == "validate":
        return [*validate_options(op.get("memory_mb"), op.get("cpu"), op.get("data_gb")), validate_fps(op.get("fps"))]
    raise SystemExit(f"unknown op {kind}")


def main():
    job = json.load(open(sys.argv[1], encoding="utf-8"))
    root = job["root"]
    results = []
    for op in job["ops"]:
        try:
            result, error = run(root, op), None
        except (ValueError, RuntimeError, OSError) as e:
            result, error = None, str(e)
        results.append({"result": result, "error": error, "tree": snapshot(root)})
    with open(sys.argv[2], "w", encoding="utf-8") as fh:
        json.dump(results, fh, ensure_ascii=False)


main()
