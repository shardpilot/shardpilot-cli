package symbols

import "testing"

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		head     []byte
		want     string
	}{
		{
			name:     "breakpad module header",
			filename: "MyGame.sym",
			head:     []byte("MODULE windows x86_64 A1B2C3D4E5F60718293A4B5C6D7E8F90a MyGame.pdb\nFUNC 1000 20 0 main\n"),
			want:     FormatBreakpad,
		},
		{
			// The case the server's content-before-suffix rule exists
			// for: a Breakpad text file named after its module. The
			// extension says .so (elf lane) but the content is
			// authoritative.
			name:     "breakpad content named libgame.so",
			filename: "libgame.so",
			head:     []byte("MODULE Linux arm64 0123456789ABCDEF0123456789ABCDEF0 libgame.so\n"),
			want:     FormatBreakpad,
		},
		{
			name:     "breakpad after BOM and whitespace",
			filename: "unknown.bin",
			head:     []byte("\ufeff\r\n\t MODULE mac arm64 FFEEDDCC libgame.dylib\n"),
			want:     FormatBreakpad,
		},
		{
			name:     "pdb msf magic",
			filename: "GameAssembly.bin",
			head:     append([]byte("Microsoft C/C++ MSF 7.00\r\n"), 0x1a, 'D', 'S', 0x00),
			want:     FormatPDB,
		},
		{
			name:     "pdb magic beats .sym extension",
			filename: "misnamed.sym",
			head:     []byte("Microsoft C/C++ MSF 7.00\r\n"),
			want:     FormatPDB,
		},
		{
			name:     "zip archive is dsym",
			filename: "whatever.bin",
			head:     []byte{'P', 'K', 0x03, 0x04, 0x14, 0x00},
			want:     FormatDSYM,
		},
		{
			name:     "mach-o 64-bit little endian is dsym",
			filename: "MyApp",
			head:     []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00},
			want:     FormatDSYM,
		},
		{
			name:     "mach-o fat binary is dsym",
			filename: "MyApp",
			head:     []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00},
			want:     FormatDSYM,
		},
		{
			name:     "elf magic",
			filename: "libgame.so",
			head:     []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01},
			want:     FormatELF,
		},
		{
			name:     "elf magic beats .pdb extension",
			filename: "misnamed.pdb",
			head:     []byte{0x7f, 'E', 'L', 'F', 0x02},
			want:     FormatELF,
		},
		{
			name:     "extension tiebreak .sym",
			filename: "app.sym",
			head:     []byte("no magic here"),
			want:     FormatBreakpad,
		},
		{
			name:     "extension tiebreak .pdb",
			filename: "app.PDB",
			head:     []byte("no magic here"),
			want:     FormatPDB,
		},
		{
			name:     "extension tiebreak .dsym",
			filename: "MyApp.dSYM",
			head:     []byte("no magic here"),
			want:     FormatDSYM,
		},
		{
			name:     "extension tiebreak .zip",
			filename: "symbols.zip",
			head:     []byte("no magic here"),
			want:     FormatDSYM,
		},
		{
			name:     "extension tiebreak .so",
			filename: "libgame.so",
			head:     []byte("no magic here"),
			want:     FormatELF,
		},
		{
			// proguard is deliberately never sniffed: plain text, no
			// magic — must be named via --format.
			name:     "proguard mapping is not sniffed",
			filename: "mapping.txt",
			head:     []byte("com.example.Game -> a.a:\n    int score -> a\n"),
			want:     "",
		},
		{
			// il2cpp line-number maps are generic JSON — never sniffed.
			name:     "il2cpp map is not sniffed",
			filename: "LineNumberMappings.json",
			head:     []byte(`{"mappings":[]}`),
			want:     "",
		},
		{
			name:     "empty head unknown extension",
			filename: "mystery.bin",
			head:     nil,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectFormat(tc.filename, tc.head); got != tc.want {
				t.Fatalf("DetectFormat(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

// The format set is the endpoint's `format` enum, all six of it.
func TestKnownFormatsAreTheEndpointEnum(t *testing.T) {
	for _, format := range []string{FormatBreakpad, FormatDSYM, FormatPDB, FormatELF, FormatProguard, FormatIL2CPPMap} {
		if !KnownFormat(format) {
			t.Errorf("%s must be known", format)
		}
	}
	if KnownFormat("minidump") || KnownFormat("") {
		t.Error("minidump and the empty string are not symbol formats")
	}
}

func TestRequiresDebugID(t *testing.T) {
	for format, want := range map[string]bool{
		FormatPDB:       true,
		FormatProguard:  true,
		FormatIL2CPPMap: true,
		FormatELF:       false,
		FormatBreakpad:  false,
		FormatDSYM:      false,
	} {
		if got := RequiresDebugID(format); got != want {
			t.Errorf("RequiresDebugID(%s) = %v, want %v", format, got, want)
		}
	}
}
