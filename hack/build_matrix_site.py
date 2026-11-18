#!/usr/bin/env python3
"""Build the GitHub Pages matrix from complete measurements, with run history."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import shutil

from render_matrix import load, render
from run_matrix import IMPLEMENTATIONS, VIEWS

ASSETS = Path(__file__).resolve().parent.parent / "web" / "matrix"
RUN_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$")


def build(directory, output, run_id, previous=None, keep=14):
    if not RUN_ID.fullmatch(run_id):
        raise ValueError("invalid run ID")
    if keep < 1:
        raise ValueError("history retention must be positive")
    # Validate before modifying the output. A missing implementation, failed
    # transport or mismatched commit cannot be published as a complete matrix.
    artifacts = load(directory)
    recorded = max(a["views"][v]["candidate"]["recordedAt"] for a in artifacts for v in VIEWS)
    for a in artifacts:
        for v in VIEWS:
            for side in ("baseline", "candidate"):
                path = directory / f"{a['implementation']}-{v}-{side}.json"
                snapshot = json.loads(path.read_text())
                expected = "ingress-nginx" if side == "baseline" else a["implementation"]
                probe_ids = {p["id"] for p in snapshot.get("probes", [])}
                if (snapshot.get("schemaVersion") != 1 or not probe_ids or not snapshot.get("observations")
                        or snapshot.get("meta", {}).get("implementation") != expected
                        or snapshot.get("meta", {}).get("mode") != "lab"
                        or any(o.get("probeID") not in probe_ids for o in snapshot["observations"])):
                    raise ValueError(f"missing or inconsistent replayable snapshot: {path.name}")
    runs = []
    if previous and (previous / "runs.json").is_file():
        prior = json.loads((previous / "runs.json").read_text())
        if prior.get("schemaVersion") != 1:
            raise ValueError("unsupported history index")
        runs = [r for r in prior["runs"] if r["id"] != run_id]
        if any(not RUN_ID.fullmatch(r["id"]) for r in runs):
            raise ValueError("invalid historical run ID")
    output.mkdir(parents=True, exist_ok=True)
    for asset in ("index.html", "app.js", "style.css"):
        shutil.copyfile(ASSETS / asset, output / asset)
    (output / ".nojekyll").touch()
    history = output / "history"
    history.mkdir(exist_ok=True)
    for record in runs[:keep - 1]:
        source = previous / "history" / record["id"]
        # Archive paths are constructed from validated IDs; refuse links in
        # downloaded artifacts before publishing them as Pages content.
        if not source.is_dir() or source.is_symlink() or any(p.is_symlink() for p in source.rglob("*")):
            raise ValueError("invalid historical artifact directory")
        shutil.copytree(source, history / record["id"], dirs_exist_ok=True)
    current = history / run_id
    current.mkdir(exist_ok=True)
    raw = current / "artifacts"
    raw.mkdir(exist_ok=True)
    for impl in IMPLEMENTATIONS:
        shutil.copyfile(directory / f"{impl}.json", raw / f"{impl}.json")
        for view in VIEWS:
            for suffix in ("", "-baseline", "-candidate"):
                name = f"{impl}-{view}{suffix}.json"
                shutil.copyfile(directory / name, raw / name)
    (current / "matrix.json").write_text(json.dumps({"schemaVersion": 1, "artifacts": artifacts}, separators=(",", ":")) + "\n")
    (current / "README.md").write_text(render(artifacts))
    base = f"history/{run_id}/"
    record = {"id": run_id, "recordedAt": recorded, "commit": artifacts[0].get("commit", ""), "matrix": base + "matrix.json", "artifacts": base + "artifacts/", "report": base + "README.md"}
    records = [record] + runs[:keep - 1]
    # Remove runs outside retention only from this generated output tree.
    retained = {r["id"] for r in records}
    for child in history.iterdir():
        if child.is_dir() and child.name not in retained:
            shutil.rmtree(child)
    (output / "runs.json").write_text(json.dumps({"schemaVersion": 1, "runs": records}, indent=2) + "\n")
    return records


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--run-id", default="local-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"))
    parser.add_argument("--previous", type=Path)
    parser.add_argument("--keep", type=int, default=14)
    args = parser.parse_args()
    build(args.directory, args.output, args.run_id, args.previous, args.keep)
