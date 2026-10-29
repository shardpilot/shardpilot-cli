# Repository guidance

Use Go 1.27.3. Run `make fmt`, `make ci` and `make release-dryrun` before publishing. Roll the changelog version before tagging and verify the explicit release version stamp in CI.

Keep public workflow jobs on hosted Ubuntu runners and run `scripts/check-workflow-runners.py` in CI. Keep all six release archive digests verified against `SHA256SUMS`, with checksum verification fixtures in CI and matching install instructions.
