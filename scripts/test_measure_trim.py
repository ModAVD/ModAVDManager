import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from measure_trim import (check_listeners, cleanup_streams, data_device, discard_stats,
                          measurement_lock, port_plan, trim_evidence)


def line(message, at=2, tid=8936, tag="vold"):
    return f" {at:.3f} 171 {tid} D {tag} : {message}\n"


class TrimEvidenceTest(unittest.TestCase):
    begin = line("BEGIN test", at=1, tag="ModAVDTrimMeasure")
    start = line("Starting trim of /data")
    success = line("Trimmed 134217728 bytes on /data in 120ms", at=3)
    failure = line("Trim failed on /data: Operation not supported on transport endpoint", at=3)

    def status(self, text):
        return trim_evidence(text, "test")["data_trim_status"]

    def test_explicit_return(self):
        self.assertEqual(self.status(self.begin + self.start + self.success), "observed_fitrim_return")

    def test_zero_bytes_still_explicit_return(self):
        self.assertEqual(self.status(self.begin + self.start + self.success.replace("134217728", "0")),
                         "observed_fitrim_return")

    def test_actual_failure_then_maintenance_completion(self):
        # Observed on the unmodified 0.37.1 package: sm exits 0 despite /data failure.
        text = self.begin + self.start + self.failure + line("idle maintenance completed", at=4)
        evidence = trim_evidence(text, "test")
        self.assertEqual(evidence["data_trim_status"], "failed")
        self.assertEqual(len(evidence["maintenance_completions"]), 1)

    def test_missing_or_ambiguous_evidence_is_unknown(self):
        cases = {
            "no log": "",
            "stable counters are not a signal": "SAMPLE 2.0\n253 32 vdc 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n" * 10,
            "old cycle": self.start + self.success + self.begin,
            "missing marker": self.start + self.success,
            "different marker": self.begin.replace("test", "other") + self.start + self.success,
            "missing start": self.begin + self.success,
            "maintenance only": self.begin + line("idle maintenance completed"),
            "start only": self.begin + self.start,
            "metadata only": self.begin + self.start.replace("/data", "/metadata") + self.success.replace("/data", "/metadata"),
            "data_mirror": self.begin + self.start + self.success.replace("/data", "/data_mirror"),
            "other thread": self.begin + self.start + self.success.replace("8936", "8937"),
            "other process": self.begin + self.start + self.success.replace("171", "172"),
            "other tag": self.begin + self.start + self.success.replace("vold", "app"),
            "return before start": self.begin + self.start + self.success.replace("3.000", "1.500"),
            "return earlier in log at same timestamp": self.begin + self.success.replace("3.000", "2.000") + self.start,
            "two starts": self.begin + self.start * 2 + self.success,
            "two returns": self.begin + self.start + self.success * 2,
            "contradictory result": self.begin + self.start + self.success + self.failure,
            "outside window": self.begin + line("END test", tag="ModAVDTrimMeasure") + self.start + self.success,
        }
        for name, text in cases.items():
            with self.subTest(name=name):
                self.assertEqual(self.status(text), "unknown")

    def test_data_mount_uses_major_minor_not_name(self):
        text = ("90 33 254:40 / /data rw - ext4 /dev/block/dm-40 rw\n"
                "94 92 254:40 /user /data_mirror/data_ce/null rw - ext4 /dev/block/dm-40 rw\n"
                "42 33 253:49 / /metadata rw - ext4 /dev/block/vdd rw\n")
        self.assertEqual(data_device(text), "254:40")
        for bad in ("", text.replace("/ /data ", "/ /data_mirror "),
                    text + "91 33 253:32 / /data rw - ext4 /dev/block/vdc rw\n"):
            with self.assertRaises(ValueError):
                data_device(bad)

    def test_diskstats_discard_columns(self):
        raw = "253 32 vdc " + " ".join(str(n) for n in range(1, 18)) + "\n"
        raw += "253 49 vdd " + "0 " * 17
        result = discard_stats(raw, {"253:32"})
        self.assertEqual(set(result), {"253:32"})
        self.assertEqual(result["253:32"]["discard"],
                         dict(completed=12, merged=13, sectors=14, milliseconds=15))
        self.assertIsNone(discard_stats("253 32 vdc " + "0 " * 11, {"253:32"})["253:32"]["discard"])
        self.assertEqual(discard_stats(raw, {"254:40"}), {})
        with self.assertRaises(ValueError):
            discard_stats(raw.replace("12", "-1"), {"253:32"})


class MeasurementResourcesTest(unittest.TestCase):
    def test_wait_failure_releases_lock_and_cleans_remaining_stream(self):
        stuck = Mock()
        stuck.poll.return_value = None
        stuck.wait.side_effect = subprocess.TimeoutExpired("adb", 5)
        stopped, stuck_thread, stopped_thread = Mock(), Mock(), Mock()
        stopped.poll.return_value = None
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaisesRegex(RuntimeError, "failed to stop"):
                with measurement_lock(root) as fd:
                    cleanup_streams([(stuck, stuck_thread), (stopped, stopped_thread)])
            stuck.terminate.assert_called_once()
            stuck.kill.assert_called_once()
            stopped.terminate.assert_called_once()
            stopped.wait.assert_called_once_with(timeout=5)
            stuck_thread.join.assert_called_once_with(timeout=2)
            stopped_thread.join.assert_called_once_with(timeout=2)
            self.assertFalse((root / ".trim-measure.lock").exists())
            with self.assertRaises(OSError):
                os.fstat(fd)
            with measurement_lock(root):
                self.assertTrue((root / ".trim-measure.lock").exists())

    def test_wait_timeout_kills_and_reaps(self):
        process, thread = Mock(), Mock()
        process.poll.return_value = None
        process.wait.side_effect = [subprocess.TimeoutExpired("adb", 5), 0]
        cleanup_streams([(process, thread)])
        process.kill.assert_called_once()
        self.assertEqual(process.wait.call_count, 2)

    def test_existing_lock_is_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / ".trim-measure.lock"
            path.write_text("another run", encoding="utf-8")
            with self.assertRaises(FileExistsError):
                with measurement_lock(root):
                    self.fail("entered another run's lock")
            self.assertEqual(path.read_text(encoding="utf-8"), "another run")


class PortIsolationTest(unittest.TestCase):
    meta = dict(portBase=27400, adbServerPort=27498, scriptAdbServerPort=27499)

    def test_explicit_private_plan(self):
        self.assertEqual(port_plan(self.meta, 0), (27400, 27498, 27499))
        self.assertEqual(port_plan(dict(self.meta, portBase=65400), 9), (65400, 27498, 27499))

    def test_invalid_or_shared_ports(self):
        cases = [dict(self.meta, **{key: value})
                 for key in self.meta for value in (None, True, "27400", 27400.0, 0, -1, 65536)]
        cases += [dict(self.meta, portBase=base) for base in (1000, 20000, 20001, 5000, 5500, 65401, 65500)]
        cases += [dict(self.meta, **{key: value})
                  for key in ("adbServerPort", "scriptAdbServerPort")
                  for value in (5037, 5555, 5585, 20048, 20049, 20050, 20091, 27400, 27401, 27492)]
        cases.append(dict(self.meta, scriptAdbServerPort=27498))
        for meta in cases:
            with self.subTest(meta=meta), self.assertRaises(ValueError):
                port_plan(meta, 0)
        for index in (-1, 5, 10, True):
            with self.subTest(index=index), self.assertRaises(ValueError):
                port_plan(self.meta, index)

    def test_entire_segment_and_process_ownership(self):
        root = Path(tempfile.gettempdir()) / "trim-test-root"
        qemu = root / "sdk/emulator/qemu/windows-x86_64/qemu-system-x86_64.exe"
        adb = root / "sdk/platform-tools/adb.exe"
        listener = lambda port, path, protocol="tcp": dict(port=port, pid=1234, protocol=protocol, executable=str(path))
        good = [listener(p, qemu) for p in (27400, 27401, 27402)] + [listener(p, adb) for p in (27498, 27499)]
        checked = check_listeners(root, 27400, 27498, 27499, 0, good)
        self.assertEqual(len(checked), 5)
        self.assertTrue(all("executable" not in row for row in checked))
        headless = listener(27400, qemu.with_name("qemu-system-x86_64-headless.exe"))
        self.assertEqual(check_listeners(root, 27400, 27498, 27499, 0, [headless])[0]["role"], "qemu")
        bad = [listener(27497, adb), listener(27410, qemu), listener(27498, qemu),
               listener(27499, adb, "udp"), listener(27400, root.parent / "other-root/qemu.exe"),
               dict(port=27400, pid=1234, protocol="tcp", executable=None)]
        for row in bad:
            with self.subTest(listener=row), self.assertRaisesRegex(ValueError, "unverified process"):
                check_listeners(root, 27400, 27498, 27499, 0, [row])


if __name__ == "__main__":
    unittest.main()
