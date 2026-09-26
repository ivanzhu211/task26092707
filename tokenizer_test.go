package indexer

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Hello, World!", []string{"hello", "world"}},
		{"foo123bar 456", []string{"foo123bar", "456"}},
		{"Äpfel ÜMLAUT straße", []string{"äpfel", "ümlaut", "straße"}},
		{"中文分词 test", []string{"中文分词", "test"}},
		{"a-b_c.d", []string{"a", "b", "c", "d"}},
		{"", nil},
		{"---", nil},
		{"İstanbul", []string{"istanbul"}},
	}
	for _, c := range cases {
		if got := tokenize([]byte(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// Invalid UTF-8 bytes are skipped, not fatal.
	if got := tokenize([]byte{'a', 0xff, 'b'}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("tokenize with invalid byte = %v", got)
	}
}
