"""Mutation checks for ModAVD#91/#72/#73, using Go overlays so source files stay untouched.

Run from any directory: python scripts/test_download_guards.py --go <go1.26.5/bin/go.exe> --out <result.json>
Each mutation must compile and fail the named test. A timeout or compiler failure is not a successful check.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]

# name, source, package, test, replacements (all must match exactly once)
CASES = [
    ("body stall", "internal/download/download.go", "download", "TestStallAndCancellation",
     [("r.timer.Reset(d)", "r.timer.Reset(time.Hour)")]),
    ("content range", "internal/download/download.go", "update", "TestUpdateRejectsWrongContentRange",
     [("status == http.StatusPartialContent && strings.HasPrefix(contentRange, fmt.Sprintf(\"bytes %d-\", existing))", "status == http.StatusPartialContent"),
      ('if contentRange != fmt.Sprintf("bytes %d-%d/%d", existing, expected-1, expected) {', 'if false {')]),
    ("redirect limit", "internal/download/download.go", "download", "TestRedirectLimit",
     [("len(via) >= MaxRedirects", "len(via) >= MaxRedirects+1")]),
    ("identity", "internal/download/download.go", "download", "TestFileProtocol",
     [('req.Header.Set("Accept-Encoding", "identity")', 'req.Header.Set("Accept-Encoding", "gzip")')]),
    ("compressed response", "internal/download/download.go", "download", "TestFileProtocol",
     [('ce != "" && !strings.EqualFold(ce, "identity")', 'false')]),
    ("416 keeps part", "internal/download/download.go", "components", "TestFetch416KeepsPart",
     [('fe.Status == http.StatusRequestedRangeNotSatisfiable {', 'fe.Status == http.StatusRequestedRangeNotSatisfiable { os.Remove(part)')]),
    ("200 truncates", "internal/download/download.go", "components", "TestFetchResumeAnswered200",
     [('flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC', 'flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND')]),
    ("200 progress resets", "internal/download/download.go", "download", "TestFileProtocol",
     [('case existing > 0:\n\t\texisting = 0', 'case existing > 0:\n\t\texisting = 3')]),
    ("manifest overall deadline", "internal/download/download.go", "components", "TestFetchManifestLimits",
     [('deadline := time.Now().Add(overall)', 'deadline := time.Now().Add(time.Hour)')]),
    ("manifest 16 MiB boundary", "internal/download/download.go", "components", "TestFetchManifestLimits",
     [('b.Len() > maxBytes', 'b.Len() >= maxBytes')]),
    ("CDK redaction", "internal/download/download.go", "download", "TestNetErrorRedactsCDK",
     [('strings.ReplaceAll(s, secret, "***")', 'strings.ReplaceAll(s, secret, secret)')]),
    ("response close", "internal/download/download.go", "download", "TestRejectedResponsesClose",
     [('r.Body.Close()', '// body close removed')]),
    ("fresh download", "internal/download/download.go", "download", "TestFileProtocol",
     [('o.Fresh || existing > expected', 'existing > expected')]),
    ("aside errors", "internal/components/install.go", "components", "TestAsideNameCollisionsAndError",
     [('} else if err != nil {\n\t\t\treturn "", err', '} else if err != nil {\n\t\t\tcontinue')]),
    ("old directories in errors", "internal/components/fetch.go", "components", "TestLaterFailureReportsOldDirectory",
     [('f.asides = append(f.asides, setAsideRel(f.sdk, aside))', 'f.asides = f.asides')]),
    ("local bad SHA-1", "internal/components/download.go", "components", "TestSingleMirrorBadLocalPart",
     [('if sha == c.SHA1 {', 'if sha == c.SHA1 || sha != "" {')]),
    ("retry interval", "internal/components/download.go", "components", "TestRetryDelayAndCancel",
     [('time.After(retryDelay)', 'time.After(0*retryDelay)')]),
    ("duplicate ZIP", "internal/components/install.go", "components", "TestDuplicateZipEntry",
     [('os.O_WRONLY|os.O_CREATE|os.O_EXCL', 'os.O_WRONLY|os.O_CREATE|os.O_TRUNC')]),
    ("remaining space", "internal/components/fetch.go", "components", "TestSpaceSubtractsDownloadedBytes",
     [('max(0, c.Size-have)', 'max(0, c.Size-0*have)')]),
    ("NO_PROXY with system proxy", "internal/hostproxy/hostproxy.go", "hostproxy", "TestOnlyNoProxyKeepsSystemProxy",
     [('envKeys[:4]', 'envKeys')]),
    ("timeout arithmetic", "internal/cli/commands.go", "cli", "TestComponentsArgumentsBeforeRoot",
     [('int64(n) > int64(t.max/time.Second)', 'time.Duration(n)*time.Second > t.max')]),
    ("component mapping", "internal/emu/check.go", "emu", "TestComponentStatusesEveryField",
     [('Build: c.Build,', 'Build: "",')]),
    ("keep stage on cancel", "internal/report/collect.go", "report", "TestCollectCanceledKeepsStage",
     [('if opt.KeepStage {\n\t\t\te.WithAdvice', 'if false {\n\t\t\te.WithAdvice')]),
    ("local ZIP time", "internal/report/collect.go", "report", "TestZipLocalWallClock",
     [('fi.ModTime().In(time.Local)', 'fi.ModTime().UTC()')]),
    ("join disk query", "internal/report/config.go", "report", "TestDriveFailureJoinsDiskProcess",
     [('defer func() { cancelDisk(); <-diskDone }()', 'defer func() { _ = cancelDisk }()')]),
    ("root short path", "internal/report/collect.go", "report", "TestCollectExpandsRootAndStage",
     [('rootDir = longPath(rootDir)', 'rootDir = rootDir')]),
    ("stage short path", "internal/report/collect.go", "report", "TestCollectExpandsRootAndStage",
     [('stageDir = longPath(stageDir)', 'stageDir = stageDir')]),
    ("adb logcat", "internal/report/adbproto.go", "report", "TestLogcatService",
     [("cmd = `export ANDROID_LOG_TAGS=''; exec logcat`", 'cmd = `export ANDROID_LOG_TAGS="\'\'"; exec logcat`')]),
    ("JSON license validation", "internal/cli/commands.go", "cli", "TestComponentsArgumentsBeforeRoot",
     [('if o.JSON && !o.AcceptLicense {', 'if false {')]),
    ("live text mode", "internal/report/collect.go", "report", "TestCollectMatchesScript",
     [('strings.ReplaceAll(s, "\\n", "\\r\\n")', 's')]),
    ("crash bytes", "internal/report/collect.go", "report", "TestCollectMatchesScript",
     [('copyShared(d.path, filepath.Join(c.stage, "crash", d.db, d.info.Name()), 0)', 'copyShared(d.path, filepath.Join(c.stage, "crash", d.db, d.info.Name()), 1)')]),
    ("WMI isolation", "internal/report/host_windows.go", "report", "TestCIMPartsApart",
     [('key := name + "|" + letter', 'key := "all|" + letter')]),
    ("cancel removes stage", "internal/report/collect.go", "report", "TestCollectCanceled",
     [('_ = os.RemoveAll(stage)', '// removal disabled')]),
    ("crash base dedup", "internal/report/collect.go", "report", "TestCollectMatchesScript",
     [('full = longPath(full)', 'full = full')]),
    ("complete update part", "internal/update/source.go", "update", "TestFullUpdatePartIsDownloadedAgain",
     [('st.Size() >= r.size', 'st.Size() > r.size')]),
    ("downloaded SHA-256", "internal/update/source.go", "update", "TestUpdateStillRejectsBadSourceDigest",
     [('r.sha256 != "" && hashOr(part) != r.sha256', 'false')]),
    ("packing context", "internal/report/collect.go", "report", "TestCollectCanceledDuringPacking",
     [('zipStage(ctx, zipPath, stage)', 'zipStage(context.Background(), zipPath, stage)')]),
    ("packing read cancellation", "internal/report/collect.go", "report", "TestCollectCanceledDuringPacking",
     [('io.Copy(w, zipReader{ctx: ctx, r: in})', 'io.Copy(w, in)')]),
    ("unfinished ZIP cleanup", "internal/report/collect.go", "report", "TestCollectCanceledDuringPacking",
     [('os.Remove(dst)', '_ = dst')]),
    ("ZIP close cancellation", "internal/report/collect.go", "report", "TestZipStageCanceledAtCompletion",
     [('err = ctx.Err()', 'err = nil')]),
]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    results = []
    # TemporaryDirectory owns exactly this freshly created directory. No repository path is deleted or edited.
    with tempfile.TemporaryDirectory(prefix="modavd-guard-overlays-") as directory:
        scratch = Path(directory)
        for index, (name, source, package, test, replacements) in enumerate(CASES):
            original = ROOT / source
            text = original.read_text(encoding="utf-8")
            for before, after in replacements:
                if text.count(before) != 1:
                    raise RuntimeError(f"{name}: expected one occurrence of {before!r}")
                text = text.replace(before, after)
            altered = scratch / f"mutation-{index}.go"
            altered.write_text(text, encoding="utf-8")
            overlay = scratch / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(original): str(altered)}}), encoding="utf-8")
            command = [args.go, "test", "-count=1", "-timeout=30s", "-overlay", str(overlay),
                       "./internal/" + package, "-run", "^" + test + "$"]
            run = subprocess.run(command, cwd=ROOT, text=True, encoding="utf-8", errors="replace",
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
            killed = run.returncode != 0 and f"--- FAIL: {test}" in run.stdout and "[build failed]" not in run.stdout
            results.append({"mutation": name, "test": test, "caught": killed, "output": run.stdout})
            print(f"{'caught' if killed else 'MISSED'}: {name} ({test})", flush=True)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return 0 if all(item["caught"] for item in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
