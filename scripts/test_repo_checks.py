import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import repo_checks


class RepoChecksTest(unittest.TestCase):
    def test_allowed_identities(self):
        self.assertTrue(repo_checks.allowed("qiyinxi", "qiyinxi@foxmail.com"))
        self.assertTrue(repo_checks.allowed("bot", "123+bot[bot]@users.noreply.github.com"))
        self.assertTrue(repo_checks.allowed("someone", "someone@users.noreply.github.com"))
        self.assertFalse(repo_checks.allowed("ai", "noreply@openai.com"))

    def test_commit_and_coauthor(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            subprocess.run(["git", "init", "-q"], cwd=root, check=True)
            subprocess.run(["git", "config", "user.name", "qiyinxi"], cwd=root, check=True)
            subprocess.run(["git", "config", "user.email", "qiyinxi@foxmail.com"], cwd=root, check=True)
            (root / "x").write_text("x")
            subprocess.run(["git", "add", "x"], cwd=root, check=True)
            env = dict(os.environ, GIT_AUTHOR_NAME="qiyinxi", GIT_AUTHOR_EMAIL="qiyinxi@foxmail.com")
            subprocess.run(["git", "commit", "-qm", "ok\n\nCo-authored-by: bot <123+bot[bot]@users.noreply.github.com>"], cwd=root, env=env, check=True)
            commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
            self.assertEqual(repo_checks.check_commit(commit, str(root)), [])

    def test_rejects_ai_trailer(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            subprocess.run(["git", "init", "-q"], cwd=root, check=True)
            subprocess.run(["git", "config", "user.name", "qiyinxi"], cwd=root, check=True)
            subprocess.run(["git", "config", "user.email", "qiyinxi@foxmail.com"], cwd=root, check=True)
            (root / "x").write_text("x")
            subprocess.run(["git", "add", "x"], cwd=root, check=True)
            subprocess.run(["git", "commit", "-qm", "bad\n\nCo-authored-by: bot <noreply@openai.com>"], cwd=root, check=True)
            commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
            self.assertTrue(repo_checks.check_commit(commit, str(root)))


if __name__ == "__main__":
    unittest.main()
