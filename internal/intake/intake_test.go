package intake

import (
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestStripSavedExamples(t *testing.T) {
	var file struct {
		Cases []struct {
			Name string `json:"name"`
			In   string `json:"in"`
			Out  string `json:"out"`
		} `json:"cases"`
	}
	load(t, "testdata/strip.json", &file)
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range file.Cases {
		got := Strip(c.In)
		if got != c.Out {
			t.Errorf("%s:\n  in: %q\n got: %q\nwant: %q", c.Name, c.In, got, c.Out)
		}
		// Stripping what was already stripped changes nothing.
		if again := Strip(got); again != got {
			t.Errorf("%s: stripping twice gave %q, then %q", c.Name, got, again)
		}
	}
}

// Whatever goes in, nothing shaped like a tag comes out.
func TestStripLeavesNoTag(t *testing.T) {
	pieces := []string{"<", ">", "/", "!", "--", "script", "b", "\"", "'", " ", "x", "?", "style", "=", "<!--", "-->", "</script>"}
	var build func(prefix string, depth int)
	build = func(prefix string, depth int) {
		out := Strip(prefix)
		for i := 0; i+1 < len(out); i++ {
			if out[i] == '<' && tagStart(out[i+1]) {
				t.Fatalf("Strip(%q) = %q, which still holds a tag", prefix, out)
			}
		}
		if depth == 0 {
			return
		}
		for _, p := range pieces {
			build(prefix+p, depth-1)
		}
	}
	build("", 4)
}

func TestStripScriptAndStyleByteOffsets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"invalid UTF-8 inside style", "<style>\xff\xfe</style>kept", "kept"},
		{"expanding UTF-8 rune U+023A inside style", "<style>\u023a\u023a\u023a</STYLE>kept", "kept"},
		{"expanding UTF-8 rune U+0130 inside script", "<script>\u0130\u0130</ScRiPt>after", "after"},
	}
	for _, tc := range cases {
		if got := Strip(tc.in); got != tc.want {
			t.Errorf("%s: Strip(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestProofOfWorkSavedExamples(t *testing.T) {
	var file struct {
		Cases []struct {
			Name   string `json:"name"`
			Token  string `json:"token"`
			Target string `json:"target"`
			Change string `json:"change"`
			Reason string `json:"reason"`
			Bits   int    `json:"bits"`
			Now    string `json:"now"`
			Valid  bool   `json:"valid"`
		} `json:"cases"`
	}
	load(t, "testdata/proof_of_work.json", &file)
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range file.Cases {
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := Proven(c.Token, c.Target, c.Change, c.Reason, c.Bits, now); got != c.Valid {
			t.Errorf("%s: Proven = %v, want %v", c.Name, got, c.Valid)
		}
	}
}

func TestSourceKeySavedExamples(t *testing.T) {
	var file struct {
		Secret string `json:"secret"`
		Cases  []struct {
			Name   string `json:"name"`
			Day    string `json:"day"`
			Source string `json:"source"`
			Key    string `json:"key"`
		} `json:"cases"`
	}
	load(t, "testdata/source_keys.json", &file)
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range file.Cases {
		addr, err := netip.ParseAddr(c.Source)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := SourceKey([]byte(file.Secret), c.Day, addr); got != c.Key {
			t.Errorf("%s: key = %s, want %s", c.Name, got, c.Key)
		}
	}
}

func TestOwnershipRefusals(t *testing.T) {
	good := `{"elements":{"a.b":{"repository":"sextant","file":"tools/x/rule.json","kind":"CONFIG"}}}`
	if _, err := ParseOwnership([]byte(good)); err != nil {
		t.Fatalf("a good map was refused: %v", err)
	}
	connEnvelope := `{"version":"v1","maxBytes":4096,"elements":{"command.hero.mission":{"ownerRepo":"clearedstack.com","filePath":"src/data/site.ts","editClass":"DATA"},"tool.slug.page":{"ownerRepo":"clearedstack.com","filePath":"src/app/tools/[slug]/page.tsx","editClass":"CONFIG"}}}`
	gotConn, err := ParseOwnership([]byte(connEnvelope))
	if err != nil {
		t.Fatalf("Conn envelope refused: %v", err)
	}
	if gotConn["command.hero.mission"] != (Owner{Repository: "clearedstack.com", File: "src/data/site.ts", Kind: "DATA"}) {
		t.Fatalf("Conn envelope decoded as %+v", gotConn["command.hero.mission"])
	}
	bellbuoyEnvelope := `{"repos":{"muster":{"mirror":true}},"workers":{"muster":"muster","conn":"clearedstack.com"},"paths":[{"prefix":"src/","repo":"clearedstack.com"}],"elements":{"app.muster.manifest":{"repository":"muster","file":"app.yaml","kind":"CONFIG"}}}`
	gotBellbuoy, err := ParseOwnership([]byte(bellbuoyEnvelope))
	if err != nil {
		t.Fatalf("Bellbuoy envelope refused: %v", err)
	}
	if gotBellbuoy["app.muster.manifest"] != (Owner{Repository: "muster", File: "app.yaml", Kind: "CONFIG"}) {
		t.Fatalf("Bellbuoy envelope decoded as %+v", gotBellbuoy["app.muster.manifest"])
	}
	bad := map[string]string{
		"not JSON":                    `elements`,
		"no elements":                 `{}`,
		"an unknown top-level field":  `{"elements":{},"owner":"made-up"}`,
		"an unknown field in a row":   `{"elements":{"a.b":{"repository":"sextant","file":"f","kind":"DATA","note":"x"}}}`,
		"duplicate repository alias":  `{"elements":{"a.b":{"repository":"sextant","ownerRepo":"sextant","file":"f","kind":"DATA"}}}`,
		"unknown envelope version":    `{"version":"v2","elements":{"a.b":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"maxBytes above 4096":         `{"maxBytes":4097,"elements":{"a.b":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"unregistered repo in repos":  `{"repos":{"made-up":{"mirror":true}},"elements":{"a.b":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"unregistered worker slug":    `{"workers":{"made-up":"sextant"},"elements":{"a.b":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"a repository not registered": `{"elements":{"a.b":{"repository":"made-up","file":"f","kind":"DATA"}}}`,
		"a kind that does not exist":  `{"elements":{"a.b":{"repository":"sextant","file":"f","kind":"OTHER"}}}`,
		"a kind in lower case":        `{"elements":{"a.b":{"repository":"sextant","file":"f","kind":"data"}}}`,
		"no file":                     `{"elements":{"a.b":{"repository":"sextant","file":"","kind":"DATA"}}}`,
		"an absolute file path":       `{"elements":{"a.b":{"repository":"sextant","file":"/etc/passwd","kind":"DATA"}}}`,
		"a parent traversal path":     `{"elements":{"a.b":{"repository":"sextant","file":"tools/../secret.json","kind":"DATA"}}}`,
		"a dot segment path":          `{"elements":{"a.b":{"repository":"sextant","file":"./rule.json","kind":"DATA"}}}`,
		"a file path with spaces":     `{"elements":{"a.b":{"repository":"sextant","file":"tools/my rule.json","kind":"DATA"}}}`,
		"a file path too long":        `{"elements":{"a.b":{"repository":"sextant","file":"` + strings.Repeat("a", MaxFileBytes+1) + `","kind":"DATA"}}}`,
		"an empty target":             `{"elements":{"":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"a target with a line break":  `{"elements":{"a\nb":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"a target with a slash":       `{"elements":{"a/b":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"a target in upper case":      `{"elements":{"A.b":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"a target that is too long":   `{"elements":{"` + strings.Repeat("a", MaxTargetBytes+1) + `":{"repository":"sextant","file":"f","kind":"DATA"}}}`,
		"two values":                  good + good,
	}
	for name, text := range bad {
		if _, err := ParseOwnership([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
