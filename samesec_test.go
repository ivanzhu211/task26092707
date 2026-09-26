package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSameSecondRewrite covers the 1-second mtime resolution hazard: a file
// read by Reindex is then overwritten with different content of the same
// length and its mtime is restored, so neither mtime nor size betrays the
// change. The next Reindex must still pick it up, while a stable file whose
// mtime is far older than the scan must not be read at all.
func TestSameSecondRewrite(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	idx := filepath.Join(root, "idx")
	db := filepath.Join(root, "files.db")

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	stablePath := filepath.Join(src, "stable.txt")
	victimPath := filepath.Join(src, "victim.txt")
	writeFile(t, stablePath, "alpha omega stable", old)
	writeFile(t, victimPath, "foo bar quux", time.Now().Truncate(time.Second))

	ix, err := Open(src, idx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	if got := ix.Search("bar"); len(got) != 1 || got[0] != "victim.txt" {
		t.Fatalf("Search(bar) = %v, want [victim.txt]", got)
	}

	// Same-length rewrite, mtime restored to its previous value.
	victimInfo, err := os.Stat(victimPath)
	if err != nil {
		t.Fatal(err)
	}
	origMtime := victimInfo.ModTime()
	newContent := []byte("foo baz quux") // same length as "foo bar quux"
	if len(newContent) != int(victimInfo.Size()) {
		t.Fatalf("test bug: replacement length %d != %d", len(newContent), victimInfo.Size())
	}
	if err := os.WriteFile(victimPath, newContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(victimPath, origMtime, origMtime); err != nil {
		t.Fatal(err)
	}

	// Observe which files the second scan reads.
	var reads []string
	readHook = func(path string) { reads = append(reads, path) }
	defer func() { readHook = nil }()
	if err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}

	if got := ix.Search("baz"); len(got) != 1 || got[0] != "victim.txt" {
		t.Errorf("Search(baz) = %v, want [victim.txt] after same-second rewrite", got)
	}
	if got := ix.Search("bar"); len(got) != 0 {
		t.Errorf("Search(bar) = %v, want empty after rewrite", got)
	}

	readSet := map[string]bool{}
	for _, r := range reads {
		readSet[r] = true
	}
	if !readSet[victimPath] {
		t.Errorf("victim.txt was not re-read despite possible same-second rewrite")
	}
	if readSet[stablePath] {
		t.Errorf("stable.txt was read, but its mtime is far older than the previous scan start")
	}
	verifyAgainstDisk(t, ix, src)
}
