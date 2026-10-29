from pathlib import Path
import yaml

VISIBILITY_RUNNER = "${{ fromJSON(github.event.repository.private && '[\"self-hosted\",\"Linux\",\"X64\"]' || '[\"ubuntu-24.04\"]') }}"
def check(workflows):
    total = 0
    for name, workflow in workflows:
        if not workflow["jobs"]: raise ValueError(f"{name}: no jobs")
        for job, config in workflow["jobs"].items():
            if config.get("runs-on") not in (VISIBILITY_RUNNER, "ubuntu-24.04"):
                raise ValueError(f"{name}/{job}: runner policy refuses {config.get('runs-on')!r}")
            total += 1
    if not total: raise ValueError("no workflow jobs")
    return total

for runner in (VISIBILITY_RUNNER, "ubuntu-24.04"):
    assert check([("healthy", {"jobs": {"test": {"runs-on": runner}}})]) == 1
for runner in (["self-hosted", "Linux", "X64"], "self-hosted", {"group": "private"}, "${{ matrix.runner }}"):
    try:
        check([("refusal", {"jobs": {"test": {"runs-on": runner}}})])
    except ValueError: continue
    raise AssertionError(f"unsafe runner accepted: {runner}")
files = sorted(p for p in Path(".github/workflows").iterdir() if p.suffix in (".yml", ".yaml"))
print(f"Runner policy: {check((str(p), yaml.safe_load(p.read_text())) for p in files)} jobs; 2 healthy and 4 refusal fixtures passed")
