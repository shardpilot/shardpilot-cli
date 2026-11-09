#!/usr/bin/env bash
# Read the staged publication tree with Git and Python's standard library.
set -euo pipefail
if [ -n "${NIGHTLY_COMPLETION_FILE:-}" ]; then
  printf 'RECEIPT scripts/check_public_surface.sh: null\n' >> "$NIGHTLY_COMPLETION_FILE" || exit 2
fi

exec python3 - "$0" <<'PY'
import json
import os
from pathlib import Path
import re
import subprocess
import sys

PRODUCER = "scripts/check_public_surface.sh"
ROOT = Path(sys.argv[1]).resolve().parents[1]
for variable in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"):
    os.environ.pop(variable, None)
PUBLIC_REPOS = {"shardpilot-cli", "shardpilot-defold", "shardpilot-go", "shardpilot-unity"}
REPO_PATH = re.compile(
    r"(?:\b(?:github\.com[/:]|api\.github\.com/repos/|raw\.githubusercontent\.com/)"
    r"|(?<![\w./-]))shardpilot/([a-z0-9_.-]+)"
)
DOC_PATH = re.compile(r"\b(?:docs/[a-z0-9_./-]+\.md|(?:architecture|engineering)/[a-z0-9_./-]+)")
DECISION = re.compile(r"\b(?:ADR|GAP)-[0-9]+", re.I)
TASK = re.compile(r"\b(?:T|DEC)-[0-9]+\b")
REVIEW_NOTE = re.compile(r"\brounds? [0-9]+(?:(?:,? (?:and|or)|,) [0-9]+)*:", re.I)
NAME_DECLARATION = re.compile(r"\bcodename[ \t]*[:=][ \t]*[a-z][a-z0-9_-]*")
PORT = re.compile(r"(?<![\w.])(localhost|127(?:\.\d{1,3}){3}|0\.0\.0\.0|\[(?:::1|(?:0{1,4}:){7}0{0,3}1)\]):(\d+)")


def categories(text, go_test=False):
    lower = text.lower()
    repos = (repo.rstrip(".,;:)").removesuffix(".git") for repo in REPO_PATH.findall(lower))
    if DOC_PATH.search(lower) or any(repo not in PUBLIC_REPOS for repo in repos):
        yield "internal-path"
    if NAME_DECLARATION.search(lower):
        yield "internal-name"
    if DECISION.search(text):
        yield "decision-reference"
    if TASK.search(text):
        yield "task-reference"
    if REVIEW_NOTE.search(text):
        yield "review-note"
    # The closed endpoint is a synthetic transport-failure control.
    if any(not (go_test and endpoint == ("127.0.0.1", "1")) for endpoint in PORT.findall(lower)):
        yield "loopback-port"


def git(*args):
    return subprocess.check_output(["git", "--no-replace-objects", *args], cwd=ROOT)


try:
    records = git("ls-files", "--stage", "--full-name", "-z").split(b"\0")
    if len(records) < 2 or records.pop() != b"":
        raise ValueError("empty or incomplete staged tree")
    planned = [f"entry-{i}" for i in range(1, len(records) + 1)]
    observed = []
    findings = 0
    for unit, record in zip(planned, records):
        header, filename = record.split(b"\t", 1)
        mode, blob, stage = header.decode("ascii").split()
        if stage != "0" or mode not in {"100644", "100755"}:
            raise ValueError(f"{unit} is unmerged or not a regular file")
        name = filename.decode("utf-8")
        content = git("cat-file", "blob", blob).decode("utf-8")
        if "\0" in content:
            raise ValueError(f"{unit} contains binary data")
        path_findings = list(categories(name))
        for category in path_findings:
            print(f"FAIL {unit} path: {category}", flush=True)
        findings += len(path_findings)
        label = unit if path_findings else json.dumps(name)
        for number, line in enumerate(content.splitlines(), 1):
            for category in categories(line, name.endswith("_test.go")):
                print(f"FAIL {label}:{number}: {category}", flush=True)
                findings += 1
        observed.append(unit)

    print(f"Public surface read {len(observed)} staged files; {findings} findings.", flush=True)
    channel = os.environ.get("NIGHTLY_COMPLETION_FILE")
    if channel:
        receipt = dict(form=1, producer=PRODUCER, planned=planned, observed=observed,
                       excluded=[], read=len(observed), expected=len(planned),
                       findings=findings, aborted=False, reasons=[])
        with open(channel, "a", encoding="utf-8") as output:
            output.write(f"RECEIPT {PRODUCER}: {json.dumps(receipt)}\n")
except (OSError, ValueError, subprocess.CalledProcessError):
    print("REFUSING: the staged tree or receipt could not be read or written.", file=sys.stderr)
    sys.exit(2)
sys.exit(int(findings > 0))
PY
