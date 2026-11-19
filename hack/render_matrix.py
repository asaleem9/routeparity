#!/usr/bin/env python3
"""Render measured matrix artifacts; never substitute a missing column with MATCH."""
import argparse
import json
from pathlib import Path

from run_matrix import IMPLEMENTATIONS, VIEWS


def cell(text):
    return str(text).replace("|", "\\|").replace("\n", " ").replace("\r", " ")


def load(directory):
    artifacts = []
    for implementation in IMPLEMENTATIONS:
        artifact = json.loads((directory / f"{implementation}.json").read_text())
        if artifact["schemaVersion"] != 1 or artifact["implementation"] != implementation:
            raise ValueError(f"invalid artifact for {implementation}")
        for view in VIEWS:
            report = artifact["views"][view]
            if report["candidate"]["implementation"] != implementation:
                raise ValueError(f"wrong candidate in {implementation}/{view}")
            if not report["summary"]["probes"] or report["summary"]["error"]:
                raise ValueError(f"failed measurement in {implementation}/{view}")
            if not report.get("coverage"):
                raise ValueError(f"missing coverage in {implementation}/{view}")
        artifacts.append(artifact)
    commits = {a.get("commit", "") for a in artifacts}
    if len(commits) != 1:
        raise ValueError("matrix columns were generated from different commits")
    return artifacts


def render(artifacts):
    lines = ["# Gateway compatibility matrix", "",
             "Measured against ingress-nginx with the repository's five-behaviours fixture and echo backends. "
             "A match covers only the probes shown. Rows group probes by the rule that generated them; "
             "a rejected route can therefore affect several rows. Unverified features and rejected routes remain listed below.", ""]
    if artifacts[0].get("commit"):
        lines += ["Source commit: `" + cell(artifacts[0]["commit"]) + "`", ""]
    for view in VIEWS:
        reports = [a["views"][view] for a in artifacts]
        lines += ["## " + {"literal": "Literal Gateway API routes", "converted": "ingress2gateway output (standard emitter)"}[view], ""]
        names = [f"{r['candidate']['implementation']} {r['candidate']['versions'][r['candidate']['implementation']]}" for r in reports]
        lines += ["| Rule | " + " | ".join(map(cell, names)) + " |", "|" + "---|" * (len(reports) + 1)]
        coverages = [{c["ruleID"]: c for c in r["coverage"]} for r in reports]
        for rule in sorted(set().union(*(set(c) for c in coverages))):
            cells = []
            for coverage in coverages:
                c = coverage.get(rule)
                if c is None or c["status"] == "not-exercised":
                    cells.append("not exercised")
                elif c["status"] == "differs":
                    cells.append(f"differs ({c['diffs']}/{c['probes']} probes)")
                elif c["status"] == "matches":
                    cells.append(f"matches ({c['probes']} probes)")
                else:
                    raise ValueError(f"unknown coverage status: {c['status']}")
            lines.append("| " + cell(rule) + " | " + " | ".join(cells) + " |")
        lines += [""]
        for name, report in zip(names, reports):
            s = report["summary"]
            lines += [f"### {cell(name)}", "",
                      f"{s['probes']} probes: {s['match']} match, {s['diff']} differ, {s['unrouted']} unrouted, "
                      f"{s['unverified']} unverified, {s['error']} errors.", "",
                      "Recorded: " + cell(report['candidate']['recordedAt']) + ".", "",
                      "Baseline: " + cell(report['baseline']['implementation']) + " " + cell(report['baseline']['versions']['ingress-nginx']) + ".", "",
                      "Tool build: " + cell(report['candidate']['toolVersion']) + ".", "",
                      "Versions: " + "; ".join(f"{cell(k)}: {cell(v)}" for k, v in sorted(report['candidate']['versions'].items())) + ".", ""]
            unknown = report.get("unverified", [])
            if unknown:
                lines += ["Not verified:", ""]
                # Both snapshots can carry the same limitation.
                entries = []
                for u in unknown:
                    text = f"{u['reason']}: {u['feature']}"
                    if u.get("object"):
                        o = u['object']
                        text += f" ({o['kind']}/{o.get('namespace', '')}/{o['name']})"
                    if u.get("detail"):
                        text += ". " + u['detail']
                    if text not in entries:
                        entries.append(text)
                lines += ["- " + cell(entry) for entry in entries] + [""]
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.write_text(render(load(args.directory)))
