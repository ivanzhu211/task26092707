package indexer

import (
	"unicode"
	"unicode/utf8"
)

// tokenize splits content into terms: maximal runs of Unicode letters and
// digits, lowercased. Invalid UTF-8 bytes are skipped.
func tokenize(content []byte) []string {
	var terms []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			terms = append(terms, string(content[start:end]))
		}
		start = -1
	}
	for i := 0; i < len(content); {
		r, size := utf8.DecodeRune(content[i:])
		if r == utf8.RuneError && size <= 1 {
			flush(i)
			i++
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			lr := unicode.ToLower(r)
			if lr != r {
				// Lowercasing may change byte length; normalize by re-encoding.
				// Simplest correct approach: build terms as runes instead.
				return tokenizeRunes(content)
			}
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
		i += size
	}
	flush(len(content))
	return terms
}

// tokenizeRunes is the general path used when any rune changes under
// lowercasing (byte length may differ from the source encoding).
func tokenizeRunes(content []byte) []string {
	var terms []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			terms = append(terms, string(cur))
			cur = cur[:0]
		}
	}
	for len(content) > 0 {
		r, size := utf8.DecodeRune(content)
		if r == utf8.RuneError && size <= 1 {
			flush()
			content = content[1:]
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, unicode.ToLower(r))
		} else {
			flush()
		}
		content = content[size:]
	}
	flush()
	return terms
}
