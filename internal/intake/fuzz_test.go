package intake

import (
	"crypto/sha256"
	"encoding/json"
	"math/bits"
	"reflect"
	"strings"
	"testing"
	"time"
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

func FuzzVerifyPoW(f *testing.F) {
	f.Add("2026-01-05.000f4240", "command.hero.mission", "Updated text", "Typo fix", 4)
	f.Add("2026-01-04.a", "app.muster.manifest", "memory: 48Mi", "", 0)
	f.Add("bad-token", "a.b", "c", "r", 20)
	f.Add("2026-01-05.GGGG", "a.b", "c", "r", 1)
	f.Add("2026-01-03.00", "a.b", "c", "r", 0)

	now := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

	f.Fuzz(func(t *testing.T, token, target, change, reason string, difficulty int) {
		got := Proven(token, target, change, reason, difficulty, now)
		want := referenceProven(token, target, change, reason, difficulty, now)
		if got != want {
			t.Fatalf("Proven(%q, %q, %q, %q, %d) = %v, want %v", token, target, change, reason, difficulty, got, want)
		}
	})
}

func referenceProven(token, target, change, reason string, difficulty int, now time.Time) bool {
	day, nonce, ok := strings.Cut(token, ".")
	if !ok || len(day) != 10 || len(nonce) < 1 || len(nonce) > 32 {
		return false
	}
	for i := 0; i < len(day); i++ {
		if i == 4 || i == 7 {
			if day[i] != '-' {
				return false
			}
			continue
		}
		if day[i] < '0' || day[i] > '9' {
			return false
		}
	}
	for i := 0; i < len(nonce); i++ {
		if (nonce[i] < '0' || nonce[i] > '9') && (nonce[i] < 'a' || nonce[i] > 'f') {
			return false
		}
	}
	if day != "2026-01-05" && day != "2026-01-04" {
		return false
	}
	raw := strings.Join([]string{"muster-pow-v1", day, target, change, reason, nonce}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	zeros := 0
	for _, b := range sum {
		if b != 0 {
			zeros += bits.LeadingZeros8(b)
			break
		}
		zeros += 8
	}
	_ = now
	return zeros >= difficulty
}

func FuzzParseOwnership(f *testing.F) {
	seeds := []string{
		`{"elements":{}}`,
		`{"elements":{"a.b":{"repository":"sextant","file":"tools/x/rule.json","kind":"CONFIG"}}}`,
		`{"version":"v1","maxBytes":4096,"elements":{"command.hero.mission":{"ownerRepo":"clearedstack.com","filePath":"src/data/site.ts","editClass":"DATA"}}}`,
		`{"repos":{"muster":{"mirror":true}},"workers":{"muster":"muster"},"paths":[{"prefix":"src/","repo":"clearedstack.com"}],"elements":{"app.muster.manifest":{"repository":"muster","file":"app.yaml","kind":"CONFIG"}}}`,
		`{"elements":{"a.b":{"repository":"sextant","file":"../bad","kind":"DATA"}}}`,
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
			if !ValidRelativePath(o.File) {
				t.Fatalf("accepted invalid relative path %q", o.File)
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
