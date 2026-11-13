package intake

import (
	"crypto/sha256"
	"math/bits"
	"regexp"
	"time"
)

// The proof-of-work puzzle.
//
// The browser looks for a nonce such that the SHA-256 hash of
//
//	"muster-pow-v1" 00 day 00 target 00 change 00 reason 00 nonce
//
// starts with a set number of zero bits. 00 is a single zero byte, day
// is the date in UTC as 2006-01-02, and target, change and reason are
// exactly the strings submitted. The token it sends is day, a full stop
// and the nonce.
//
// The hash covers the submission, so a token cannot be reused for
// different text, and the day, so a token stops working: Muster accepts
// today's date and yesterday's, in UTC, and no other.
const powLabel = "muster-pow-v1"

var tokenForm = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})\.([0-9a-f]{1,32})$`)

const dayForm = "2006-01-02"

// Proven reports whether token solves the puzzle for this submission at
// the given difficulty.
func Proven(token, target, change, reason string, difficulty int, now time.Time) bool {
	m := tokenForm.FindStringSubmatch(token)
	if m == nil {
		return false
	}
	day, nonce := m[1], m[2]
	today := now.UTC()
	if day != today.Format(dayForm) && day != today.AddDate(0, 0, -1).Format(dayForm) {
		return false
	}
	return leadingZeroBits(puzzleHash(day, target, change, reason, nonce)) >= difficulty
}

func puzzleHash(day, target, change, reason, nonce string) [sha256.Size]byte {
	h := sha256.New()
	for _, part := range []string{powLabel, day, target, change, reason} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write([]byte(nonce))
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

func leadingZeroBits(sum [sha256.Size]byte) int {
	n := 0
	for _, b := range sum {
		if b != 0 {
			return n + bits.LeadingZeros8(b)
		}
		n += 8
	}
	return n
}
