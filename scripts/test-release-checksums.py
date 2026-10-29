#!/usr/bin/env python3
"""Exercise release packaging with fixture bytes, never a Go compiler."""

import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parent.parent
SHA256 = shutil.which("sha256sum")
if SHA256 is None:
    raise SystemExit("sha256sum is required for release checksum fixtures")

GO_FIXTURE = """#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
    if [ "$1" = "-o" ]; then
        shift
        mkdir -p "$(dirname "$1")"
        printf 'release fixture bytes\\n' > "$1"
        exit 0
    fi
    shift
done
echo 'fixture expected a build output path' >&2
exit 1
"""

SHA_FIXTURE = """#!/bin/sh
set -eu
case "$1" in
    -c|--check) exec {sha256} "$@" ;;
esac
if [ "$RELEASE_FIXTURE_BAD_DIGEST" = 1 ]; then
    for archive in "$@"; do
        printf '%064d  %s\\n' 0 "$archive"
    done
else
    exec {sha256} "$@"
fi
""".format(sha256=shlex.quote(SHA256))


def run_case(bad_digest):
    with tempfile.TemporaryDirectory(prefix="release-checksums-") as directory:
        root = Path(directory)
        for name in ("Makefile", "LICENSE", "NOTICE", "go.mod"):
            shutil.copyfile(ROOT / name, root / name)
        tools = root / "tools"
        tools.mkdir()
        for name, text in (("go", GO_FIXTURE), ("sha256sum", SHA_FIXTURE)):
            target = tools / name
            target.write_text(text, encoding="utf-8")
            target.chmod(0o755)
        env = os.environ.copy()
        env.pop("MAKEFLAGS", None)
        env.pop("MFLAGS", None)
        env["PATH"] = str(tools) + os.pathsep + env["PATH"]
        env["RELEASE_FIXTURE_BAD_DIGEST"] = "1" if bad_digest else "0"
        result = subprocess.run(
            ["make", "release-dryrun", "VERSION=v0.0.0-fixture"],
            cwd=root, env=env, text=True, stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, check=False,
        )
        if bad_digest:
            if result.returncode == 0 or result.stdout.count(": FAILED") != 6:
                print(result.stdout)
                raise SystemExit("FAIL: release dry run did not reject six wrong digests")
        elif result.returncode != 0 or "release-dryrun: OK" not in result.stdout:
            print(result.stdout)
            raise SystemExit("FAIL: healthy release fixture was refused")
        print("PASS: six wrong digests refused" if bad_digest else
              "PASS: six healthy fixture archives accepted")


if __name__ == "__main__":
    run_case(False)
    run_case(True)
