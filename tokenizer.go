package indexer

import (
	"strings"
	"unicode"
)

// tokenize splits text into lowercase terms on Unicode letter/digit boundaries.
func tokenize(text string) []string {
	var terms []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			terms = append(terms, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return terms
}
