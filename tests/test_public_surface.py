"""Exercise the staged gate and its completion receipt in an isolated tree."""
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

gate = Path("scripts/check_public_surface.sh")
cases = {
    "clean": "Clone https://github.com/shardpilot/shardpilot-cli.git and see cmd/shardpilot/main.go. Use https://[2001:db8::1]:443/ in examples. Encode UTF-8; n := t-1 rounds down around 2:30 GMT-5 in a T-shirt; the round 10 is next.",
    "public-sentence": "See https://github.com/shardpilot/shardpilot-cli. Then clone it.",
    "public-api-raw": "See https://api.github.com/repos/shardpilot/shardpilot-cli and https://raw.githubusercontent.com/shardpilot/shardpilot-cli/main/README.md.",
    "internal-path": "See https://github.com/shard" "pilot/fabricated-review-module/ for details.",
    "path-dot": "See https://github.com/shard" "pilot/shardpilot-cli.fabricated/ for details.",
    "path-underscore": "See https://github.com/shard" "pilot/shardpilot-cli_fabricated/ for details.",
    "path-api": "See https://api.github.com/repos/shard" "pilot/fabricated-review-module.",
    "path-raw": "See https://raw.githubusercontent.com/shard" "pilot/fabricated-review-module/main/README.md.",
    "internal-name": "Set code" "name: cedar-comet.",
    "task-reference": "Fixed in T-" "0.",
    "task-reference-decision": "As DEC-" "0 says.",
    "review-note": "// Round" " 1: a redirect is refused.",
    "review-note-list": "// Round" "s 1 and 3: every request is bounded.",
    "review-note-series": "Round" "s 2, 3 and 5: refused.",
    "review-note-oxford": "Round" "s 1, 3, and 5: refused.",
    "loopback-port": "Try http://local" "host:18080.",
    "loopback-port-expanded": "Try http://[" "0:0:0:0:0:0:0:1]:18080/.",
}
with tempfile.TemporaryDirectory(prefix="surface-fixtures-") as folder:
    root = Path(folder) / "repo"
    (root / gate.parent).mkdir(parents=True)
    shutil.copyfile(gate, root / gate)
    subprocess.run(["git", "init", "-q"], cwd=root, check=True)
    channel = Path(folder) / "receipt"
    for kind, content in cases.items():
        (root / "sample.txt").write_text(content + "\n")
        subprocess.run(["git", "add", "--all"], cwd=root, check=True)
        result = subprocess.run(["env", f"NIGHTLY_COMPLETION_FILE={channel}", "bash", str(gate)],
                                cwd=root, capture_output=True, text=True)
        expected = int(kind not in {"clean", "public-sentence", "public-api-raw"})
        category = "internal-path" if kind.startswith("path-") else kind.removesuffix("-expanded").removesuffix("-decision").removesuffix("-list").removesuffix("-series").removesuffix("-oxford")
        assert result.returncode == expected, result.stdout + result.stderr
        failures = [line for line in result.stdout.splitlines() if line.startswith("FAIL ")]
        assert len(failures) == expected and (not expected or failures[0].endswith(category)), result.stdout
        receipt = json.loads(channel.read_text().splitlines()[-1].split(": ", 1)[1])
        assert receipt["planned"] == receipt["observed"] and receipt["read"] == receipt["expected"] == 2
        assert receipt["findings"] == expected and not receipt["aborted"]
        print(f"{kind}: exit {result.returncode}, {expected} findings, complete receipt")
