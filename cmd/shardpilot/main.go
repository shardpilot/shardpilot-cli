// Command shardpilot is the ShardPilot developer CLI.
//
// v0 ships `shardpilot symbols upload`: upload debug symbol files to
// ShardPilot's symbol-upload endpoint from developer workstations and CI
// pipelines.
package main

import (
	"os"

	"github.com/shardpilot/shardpilot-cli/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}
