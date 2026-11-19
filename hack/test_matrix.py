import copy
import json
from pathlib import Path
import tempfile
import unittest

from render_matrix import load, render
from build_matrix_site import build
from run_matrix import IMPLEMENTATIONS, VIEWS


def artifact(implementation):
    report = {
        "baseline": {"implementation": "ingress-nginx", "versions": {"ingress-nginx": "v1.15.1"}},
        "candidate": {"implementation": implementation, "toolVersion": "test", "recordedAt": "2026-11-16T20:00:00Z",
                      "versions": {implementation: "v1", "gateway-api": "v1.6.3"}},
        "summary": {"probes": 2, "match": 1, "diff": 1, "unrouted": 0, "unverified": 0, "error": 0},
        "coverage": [{"ruleID": "NGX-NORM-001", "status": "matches", "probes": 1, "diffs": 0},
                     {"ruleID": "NGX-REGEX-001", "status": "not-exercised", "probes": 0, "diffs": 0}],
        "unverified": [{"reason": "not-loaded", "feature": "route", "detail": "UnsupportedValue: /x|y"}],
    }
    return {"schemaVersion": 1, "implementation": implementation, "commit": "abc", "views": {v: copy.deepcopy(report) for v in VIEWS}}


class MatrixTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        for impl in IMPLEMENTATIONS:
            self.write(impl, artifact(impl))
            for view in VIEWS:
                report = artifact(impl)["views"][view]
                (self.path / f"{impl}-{view}.json").write_text(json.dumps(report))
                for side in ("baseline", "candidate"):
                    (self.path / f"{impl}-{view}-{side}.json").write_text(json.dumps({"schemaVersion": 1, "meta": {"mode": "lab", "implementation": "ingress-nginx" if side == "baseline" else impl}, "probes": [{"id": "sample"}], "observations": [{"probeID": "sample"}]}))

    def write(self, implementation, data):
        (self.path / f"{implementation}.json").write_text(json.dumps(data))

    def test_preserves_limitations_and_unexercised_rules(self):
        result = render(load(self.path))
        self.assertIn("not exercised | not exercised", result)
        self.assertIn("not-loaded: route. UnsupportedValue: /x\\|y", result)
        self.assertIn("matches (1 probes)", result)
        self.assertIn("Recorded: 2026-11-16", result)

    def test_missing_implementation_is_an_error(self):
        (self.path / f"{IMPLEMENTATIONS[1]}.json").unlink()
        with self.assertRaises(FileNotFoundError):
            load(self.path)

    def test_failed_or_misidentified_columns_are_rejected(self):
        impl = IMPLEMENTATIONS[0]
        for change in ("errors", "empty", "wrong-implementation", "missing-coverage", "different-commit"):
            with self.subTest(change=change):
                data = artifact(impl)
                report = data["views"]["converted"]
                if change == "errors":
                    report["summary"]["error"] = 1
                elif change == "empty":
                    report["summary"]["probes"] = 0
                elif change == "wrong-implementation":
                    report["candidate"]["implementation"] = "other"
                elif change == "missing-coverage":
                    report["coverage"] = []
                else:
                    data["commit"] = "different"
                self.write(impl, data)
                with self.assertRaises(ValueError):
                    load(self.path)

    def test_site_preserves_history_and_downloads(self):
        previous = self.path / "previous"
        build(self.path, previous, "run-1")
        current = self.path / "site"
        records = build(self.path, current, "run-2", previous)
        self.assertEqual([r["id"] for r in records], ["run-2", "run-1"])
        for record in records:
            payload = json.loads((current / record["matrix"]).read_text())
            self.assertEqual(len(payload["artifacts"]), len(IMPLEMENTATIONS))
            for impl in IMPLEMENTATIONS:
                self.assertTrue((current / record["artifacts"] / f"{impl}-literal-baseline.json").is_file())
        self.assertTrue((current / "index.html").is_file())
        final = self.path / "trimmed"
        build(self.path, final, "run-3", current, keep=1)
        self.assertFalse((final / "history" / "run-1").exists())
        self.assertEqual(len(json.loads((final / "runs.json").read_text())["runs"]), 1)

    def test_site_rejects_missing_replay_snapshot(self):
        (self.path / f"{IMPLEMENTATIONS[0]}-literal-baseline.json").unlink()
        output = self.path / "site"
        with self.assertRaises(FileNotFoundError):
            build(self.path, output, "run-1")
        self.assertFalse(output.exists())

    def test_history_cannot_escape_output(self):
        with self.assertRaises(ValueError):
            build(self.path, self.path / "site", "../escape")
        previous = self.path / "previous"
        previous.mkdir()
        (previous / "runs.json").write_text(json.dumps({"schemaVersion": 1, "runs": [{"id": "../../escape"}]}))
        with self.assertRaises(ValueError):
            build(self.path, self.path / "site", "run-1", previous)


if __name__ == "__main__":
    unittest.main()
