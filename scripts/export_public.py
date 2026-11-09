"""Check the source snapshot in a disposable checkout without publishing it."""
from io import BytesIO
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

root = Path(__file__).resolve().parents[1]

def git(*args):
    return subprocess.check_output(["git", *args], cwd=root)

tree = git("write-tree").decode().strip()
excluded = git("show", f"{tree}:EXPORT-EXCLUDE").decode().splitlines()
paths = git("ls-tree", "-rz", "--name-only", tree).decode().split("\0")[:-1]
expected = set(paths) - set(excluded)
archive = git("archive", tree, "--", ".", *(f":(exclude,literal){name}" for name in excluded))
with tempfile.TemporaryDirectory(prefix="public-export-") as folder, tarfile.open(fileobj=BytesIO(archive)) as snapshot:
    members = {entry.name for entry in snapshot.getmembers() if not entry.isdir()}
    if members != expected:
        raise SystemExit("Export does not match the staged path list")
    snapshot.extractall(folder, filter="data")
    gate = Path(folder) / "scripts/check_public_surface.sh"
    if not gate.is_file() or not os.access(gate, os.X_OK):
        raise SystemExit("Export is missing its executable public surface check")
    subprocess.run(["git", "init", "-q"], cwd=folder, check=True)
    subprocess.run(["git", "add", "--force", "--all"], cwd=folder, check=True)
    plan = subprocess.check_output(["make", "-n", "ci"], cwd=folder, text=True)
    if "go test ./..." not in plan or any(path in plan for path in excluded):
        raise SystemExit("Exported CI plan is missing Go tests or references an excluded path")
    print("Exported make ci plan checked against the staged inputs", flush=True)
    print(f"Export contains {gate.relative_to(folder)} (executable, {len(members)} files)", flush=True)
    subprocess.run([str(gate)], cwd=folder, check=True)
