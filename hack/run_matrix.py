#!/usr/bin/env python3
"""Measure both route sources for one implementation; retain replayable evidence."""
import argparse
import json
import os
from pathlib import Path
import subprocess

IMPLEMENTATIONS = ("envoy-gateway", "nginx-gateway-fabric", "istio")
VIEWS = ("literal", "converted")


def run(implementation, output, binary):
    output.mkdir(parents=True, exist_ok=True)
    cluster = "routeparity-matrix-" + implementation
    artifact_path = output / f"{implementation}.json"
    artifact_path.unlink(missing_ok=True)
    artifact = {"schemaVersion": 1, "implementation": implementation, "commit": os.environ.get("GITHUB_SHA", ""), "views": {}}
    try:
        for view in VIEWS:
            stem = output / f"{implementation}-{view}"
            command = [binary, "lab", "run", "-f", "examples/five-behaviours/", "--implementation", implementation,
                       "--name", cluster, "--keep", "-o", "json", "--save-baseline", str(stem) + "-baseline.json",
                       "--save-candidate", str(stem) + "-candidate.json"]
            if view == "literal":
                command += ["--gateway", "examples/five-behaviours-gateway/"]
            # DIFF (1) and incomplete under strict (2) are measurements. A tool
            # error (3), invalid JSON or zero probes must not become a column.
            with stem.with_suffix(".json").open("w") as stream:
                result = subprocess.run(command, stdout=stream, check=False)
            if result.returncode not in (0, 1, 2):
                raise RuntimeError(f"{implementation}/{view}: tool exited {result.returncode}")
            report = json.loads(stem.with_suffix(".json").read_text())
            if report["summary"]["probes"] == 0 or report["summary"]["error"]:
                raise RuntimeError(f"{implementation}/{view}: incomplete transport measurement: {report['summary']}")
            artifact["views"][view] = report
        artifact_path.write_text(json.dumps(artifact, indent=2) + "\n")
    finally:
        subprocess.run([binary, "lab", "down", "--name", cluster], check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("implementation", choices=IMPLEMENTATIONS)
    parser.add_argument("--output", type=Path, default=Path("matrix"))
    parser.add_argument("--binary", default="./bin/routeparity")
    args = parser.parse_args()
    run(args.implementation, args.output, args.binary)
