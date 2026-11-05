import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from measure_trim import data_device, discard_stats, trim_evidence


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


if __name__ == "__main__":
    unittest.main()
