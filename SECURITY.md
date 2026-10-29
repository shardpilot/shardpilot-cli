# Security Policy

The ShardPilot CLI is pre-v1 software: the command surface and flags may
change before v1.

## Reporting a vulnerability

Email **security@shardpilot.com**. That address is published on ShardPilot's
own security page and does not depend on this repository's settings or on you
already having a contact here. You may also use this repository's private
security-advisory flow if it is enabled for you.

Please do not open a public issue for a suspected vulnerability.

Include the CLI version, your OS and architecture, and the smallest
reproduction you have.

## What this CLI handles

`symbols upload` reads an API key with the `symbols:write` scope from the
environment variable **`SHARDPILOT_SYMBOLS_KEY`**, and streams the debug symbol
files you name to the endpoint you configure. It writes no credentials to disk
and keeps no state between runs.

**There is deliberately no `--key` or `--token` flag, and that is a security
decision rather than an omission.** A credential on a command line is visible
to every other process through the process table, is written to your shell
history, and in CI usually reaches a build log. The environment variable is the
only interface, so there is nothing to misuse.

## Supported versions

Pre-v1, only the latest release is supported. Fixes land on the next release
rather than being backported.
