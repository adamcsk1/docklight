import subprocess
import unittest
from unittest.mock import patch

from ci_targets import changed_targets, select_targets


class TargetTests(unittest.TestCase):
    def test_paths(self):
        cases = [
            (["cmd/docklight/static/app.js"], ["ui"]),
            (["cmd/docklight-agent/main.go"], ["agent"]),
            (["cmd/docklight/ui.go", "cmd/docklight-agent/main.go"], ["ui", "agent"]),
            (["internal/management/types.go"], ["ui", "agent"]),
            (["go.mod"], ["ui", "agent"]),
            (["go.sum"], ["ui", "agent"]),
            (["Dockerfile"], ["ui", "agent"]),
            ([".dockerignore"], ["ui", "agent"]),
            ([".github/workflows/ci.yml"], ["ui", "agent"]),
            (["scripts/ci_targets.py"], ["ui", "agent"]),
            (["new-package/build.go"], ["ui", "agent"]),
            (["README.md", "docs/docker.md", "docker-compose.development.yml", "scripts/ui-smoke.cjs"], []),
            (["cmd/docklight/ui.go", "docs/docker.md"], ["ui"]),
            ([], []),
        ]
        for paths, expected in cases:
            with self.subTest(paths=paths):
                self.assertEqual(select_targets(paths), expected)

    @patch("ci_targets.subprocess.check_output")
    def test_push_compares_entire_pushed_range(self, diff):
        diff.return_value = b"cmd/docklight/deleted.go\0cmd/docklight-agent/renamed.go\0"
        self.assertEqual(changed_targets({"before": "base"}, "refs/heads/main", "head"), ["ui", "agent"])
        self.assertEqual(diff.call_args.args[0], ["git", "diff", "--name-only", "--no-renames", "-z", "base..head", "--"])

    @patch("ci_targets.subprocess.check_output")
    def test_pull_request_uses_merge_base_and_head(self, diff):
        diff.return_value = b"cmd/docklight/static/app.css\0"
        event = {"pull_request": {"base": {"sha": "base"}, "head": {"sha": "pr-head"}}}
        self.assertEqual(changed_targets(event, "refs/pull/1/merge", "merge-commit"), ["ui"])
        self.assertIn("base...pr-head", diff.call_args.args[0])

    @patch("ci_targets.subprocess.check_output")
    def test_tags_and_missing_history_build_both(self, diff):
        for event, ref in [({}, "refs/tags/v1.0.0"), ({"before": "0" * 40}, "refs/heads/main"), ({}, "refs/heads/main")]:
            self.assertEqual(changed_targets(event, ref, "head"), ["ui", "agent"])
        diff.assert_not_called()
        diff.side_effect = subprocess.CalledProcessError(128, "git")
        self.assertEqual(changed_targets({"before": "missing"}, "refs/heads/main", "head"), ["ui", "agent"])


if __name__ == "__main__":
    unittest.main()
