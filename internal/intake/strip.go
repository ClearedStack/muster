package intake

import "strings"

// Strip removes all markup and scripts from a piece of proposal text and
// trims the white space around what is left.
//
// The rules, applied from left to right:
//
//  1. A comment, from "<!--" to "-->", is removed.
//  2. A script or style element is removed with everything inside it.
//  3. Anything shaped like a tag is removed: "<" followed by a letter,
//     "/", "!" or "?", up to the next ">" that is not inside quotes.
//  4. Whatever rule 1, 2 or 3 starts and the text never finishes is
//     removed to the end.
//  5. A "<" followed by anything else is ordinary text and is kept.
//
// Removing one tag can leave the pieces of another side by side, so the
// rules are applied again until nothing changes. Each pass only removes
// text, so this always ends.
func Strip(s string) string {
	for {
		next := stripOnce(s)
		if next == s {
			return strings.TrimSpace(next)
		}
		s = next
	}
}

func stripOnce(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '<' {
			out.WriteByte(c)
			i++
			continue
		}
		rest := s[i:]
		if strings.HasPrefix(rest, "<!--") {
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return out.String()
			}
			i += 4 + end + 3
			continue
		}
		if len(rest) < 2 || !tagStart(rest[1]) {
			out.WriteByte(c)
			i++
			continue
		}
		end := tagEnd(rest)
		if end < 0 {
			return out.String()
		}
		name := tagName(rest)
		i += end + 1
		if rest[1] != '/' && (name == "script" || name == "style") {
			skip := closing(s[i:], name)
			if skip < 0 {
				return out.String()
			}
			i += skip
		}
	}
	return out.String()
}

func letter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func tagStart(c byte) bool { return letter(c) || c == '/' || c == '!' || c == '?' }

// tagEnd returns the index of the ">" that closes the tag at the start of
// s, skipping any ">" inside a quoted value, or -1 if there is none.
func tagEnd(s string) int {
	var quote byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

// tagName returns the lower-case name of the tag at the start of s.
func tagName(s string) string {
	i := 1
	if i < len(s) && s[i] == '/' {
		i++
	}
	j := i
	for j < len(s) && (letter(s[j]) || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	return strings.ToLower(s[i:j])
}

// closing returns how much of s to skip to get past the closing tag of a
// script or style element, or -1 if s never closes it.
func closing(s, name string) int {
	lower := strings.ToLower(s)
	want := "</" + name
	from := 0
	for {
		at := strings.Index(lower[from:], want)
		if at < 0 {
			return -1
		}
		at += from
		after := at + len(want)
		if after < len(s) && (letter(s[after]) || s[after] >= '0' && s[after] <= '9') {
			// A longer name, such as </scripts>; keep looking.
			from = after
			continue
		}
		end := strings.IndexByte(s[after:], '>')
		if end < 0 {
			return -1
		}
		return after + end + 1
	}
}
