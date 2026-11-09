# Repository guidance

Use Go 1.27.3. Run `make fmt`, `make ci` and `make release-dryrun` before publishing. Roll the changelog version before tagging and verify the explicit release version stamp in CI.

Keep public workflow jobs on hosted Ubuntu runners and run `scripts/check-workflow-runners.py` in CI. Keep all six release archive digests verified against `SHA256SUMS`, with checksum verification fixtures in CI and matching install instructions.
Preserve the static analyzer controls in checkouts with their inputs. Native controls share project flags, exhaustive blocks without `-d`, and NilAway findings and errors stay report-only with explicit JSON status. Interrupt controls run in `make static-checks-test` there.
Keep the executable public surface check in the staged source snapshot. Use public repository references and generic disclosure shapes in its rules and synthetic values in its fixtures. Run `make public-surface-check` and `make export-dryrun` before publishing.
