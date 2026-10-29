// Package symbols implements client-side symbol format detection and the
// streaming upload client for ShardPilot's symbol-upload endpoint
// (POST /api/v1/symbols/upload).
package symbols

import (
	"bytes"
	"path/filepath"
	"strings"
)

// Symbol format names, as sent in the endpoint's multipart `format` field.
// The set mirrors the endpoint's `format` enum: breakpad, dsym, pdb, elf,
// proguard, il2cpp_map.
const (
	FormatBreakpad  = "breakpad"
	FormatDSYM      = "dsym"
	FormatPDB       = "pdb"
	FormatELF       = "elf"
	FormatProguard  = "proguard"
	FormatIL2CPPMap = "il2cpp_map"
)

// SniffLen is how many leading file bytes DetectFormat needs. Every magic it
// recognizes sits within the first few dozen bytes; 1 KiB leaves generous
// room for BOM/whitespace prefixes on Breakpad text files.
const SniffLen = 1024

// KnownFormat reports whether the upload endpoint accepts format.
func KnownFormat(format string) bool {
	switch format {
	case FormatBreakpad, FormatDSYM, FormatPDB, FormatELF, FormatProguard, FormatIL2CPPMap:
		return true
	}
	return false
}

// RequiresDebugID reports whether an upload in format must carry an explicit
// debug_id. The endpoint cannot read one from a pdb yet; a proguard mapping
// carries the app build id instead; an il2cpp_map carries the debug id of the
// native module it maps. An elf can name one but need not: where the server
// derives it from the object's build-id note it does, and where it cannot it
// answers 400.
func RequiresDebugID(format string) bool {
	switch format {
	case FormatPDB, FormatProguard, FormatIL2CPPMap:
		return true
	}
	return false
}

// DetectFormat infers the symbol format from the file's leading bytes and, as
// a fallback, its name. head is the file's first SniffLen bytes (fewer when
// the file is shorter). It returns "" when nothing matched.
//
// It mirrors the server-side rule: content sniffing runs
// FIRST — magic bytes/headers are authoritative over whatever name the
// artifact carries (a Breakpad text symbol file is routinely named after its
// module, e.g. `libgame.so`) — and extension heuristics only break the tie
// when no known magic matched. proguard and il2cpp_map are deliberately
// never sniffed: an R8 mapping.txt is plain text with no magic and Unity's
// line-number map is generic JSON — both must be named via --format.
func DetectFormat(filename string, head []byte) string {
	trimmed := bytes.TrimLeft(head, "\ufeff\r\n\t ")
	if bytes.HasPrefix(trimmed, []byte("MODULE ")) {
		return FormatBreakpad
	}
	if bytes.HasPrefix(head, []byte("Microsoft C/C++ MSF")) {
		return FormatPDB
	}
	if looksLikeZip(head) || looksLikeMachO(head) {
		return FormatDSYM
	}
	if looksLikeELF(head) {
		return FormatELF
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".sym":
		return FormatBreakpad
	case ".pdb":
		return FormatPDB
	case ".dsym", ".zip":
		return FormatDSYM
	case ".so":
		return FormatELF
	}
	return ""
}

func looksLikeZip(data []byte) bool {
	return len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 0x03 && data[3] == 0x04
}

func looksLikeMachO(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	magics := [][]byte{
		{0xfe, 0xed, 0xfa, 0xce},
		{0xce, 0xfa, 0xed, 0xfe},
		{0xfe, 0xed, 0xfa, 0xcf},
		{0xcf, 0xfa, 0xed, 0xfe},
		{0xca, 0xfe, 0xba, 0xbe},
		{0xbe, 0xba, 0xfe, 0xca},
	}
	for _, magic := range magics {
		if bytes.Equal(data[:4], magic) {
			return true
		}
	}
	return false
}

func looksLikeELF(data []byte) bool {
	return len(data) >= 4 && data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F'
}
