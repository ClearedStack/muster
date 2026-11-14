package intake

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ClearedStack/muster/internal/registry"
)

func FuzzStrip(f *testing.F) {
	seeds := []string{
		"",
		"Plain text with 2 < 5 and 9 > 3.",
		"<b>bold</b>",
		"<sc<script>ript>alert(1)</script>kept",
		"<!-- comment -->visible<!-- unclosed",
		"<style>body{display:none}</style>after",
		`<a href=">">inside</a>outside`,
		"<img src='x' onerror='alert(1)'>",
		"   padded \n text \t ",
		"<style>\xff\xfe</style>kept",
		"<style>ȺȺȺ</STYLE>kept",
		"<script>İİ</ScRiPt>after",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got := Strip(in)
		if len(got) > len(in) {
			t.Fatalf("Strip grew input from %d to %d bytes", len(in), len(got))
		}
		if strings.TrimSpace(got) != got {
			t.Fatalf("Strip(%q) = %q has untrimmed outer whitespace", in, got)
		}
		if strings.Contains(got, "<!--") {
			t.Fatalf("Strip(%q) = %q still contains an HTML comment start", in, got)
		}
		for i := 0; i+1 < len(got); i++ {
			if got[i] == '<' && tagStart(got[i+1]) {
				t.Fatalf("Strip(%q) = %q still holds a tag at byte %d", in, got, i)
			}
		}
		if again := Strip(got); again != got {
			t.Fatalf("Strip is not idempotent on %q: %q != %q", in, again, got)
		}
		if utf8.ValidString(in) && !utf8.ValidString(got) {
			t.Fatalf("Strip broke valid UTF-8 on %q -> %q", in, got)
		}
	})
}

func FuzzParseOwnership(f *testing.F) {
	seeds := []string{
		`{"elements":{}}`,
		`{"elements":{"a.b":{"repository":"sextant","file":"tools/x/rule.json","kind":"CONFIG"}}}`,
		`{"version":"v1","maxBytes":4096,"elements":{"command.hero.mission":{"ownerRepo":"clearedstack.com","filePath":"src/data/site.ts","editClass":"DATA"}}}`,
		`{"elements":{"a.b":{"repository":"sextant","file":"f","kind":"DATA","extra":1}}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		owners, err := ParseOwnership(data)
		if err != nil {
			return
		}
		for target, o := range owners {
			if len(target) == 0 || len(target) > MaxTargetBytes || !targetForm.MatchString(target) {
				t.Fatalf("accepted malformed target %q", target)
			}
			if !registry.IsRepository(o.Repository) {
				t.Fatalf("accepted unregistered repository %q", o.Repository)
			}
			if o.File == "" || !plain(o.File) {
				t.Fatalf("accepted invalid file %q", o.File)
			}
			if !editKinds[o.Kind] {
				t.Fatalf("accepted invalid edit kind %q", o.Kind)
			}
		}
		canonical, err := json.Marshal(struct {
			Elements Ownership `json:"elements"`
		}{Elements: owners})
		if err != nil {
			t.Fatalf("marshaling accepted ownership map: %v", err)
		}
		roundTrip, err := ParseOwnership(canonical)
		if err != nil {
			t.Fatalf("re-parsing canonical ownership map failed: %v", err)
		}
		if !reflect.DeepEqual(owners, roundTrip) {
			t.Fatalf("round-trip mismatch: got %+v, want %+v", roundTrip, owners)
		}
	})
}
