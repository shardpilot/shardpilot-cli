package cli

import (
	"testing"

	"github.com/shardpilot/shardpilot-cli/internal/symbols"
)

// Every format that needs --debug-id has a message saying why, and no other
// format has one.
func TestDebugIDMessagesMatchTheRule(t *testing.T) {
	for _, format := range []string{symbols.FormatBreakpad, symbols.FormatDSYM, symbols.FormatPDB, symbols.FormatELF, symbols.FormatProguard, symbols.FormatIL2CPPMap} {
		_, hasMessage := debugIDRequired[format]
		if hasMessage != symbols.RequiresDebugID(format) {
			t.Errorf("%s: message present = %v, RequiresDebugID = %v", format, hasMessage, symbols.RequiresDebugID(format))
		}
	}
}
