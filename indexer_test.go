package indexer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var errSimulatedCrash = errors.New("simulated crash")

// ---------- naive reference implementation ----------

// naiveReference scans the directory directly and builds a full index from
// scratch: term -> sorted paths, plus the expected file records.
func naiveReference(t *testing.T, dir string) (map[string][]string, []FileRecord) {
	t.Helper()
	postings := map[string]map[string]struct{}{}
	var recs []FileRecord
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".txt") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(content) {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		recs = append(recs, FileRecord{Path: rel, Size: info.Size(), Mtime: info.ModTime().Unix()})
		for _, term := range dedup(tokenize(string(content))) {
			set := postings[term]
			if set == nil {
				set = map[string]struct{}{}
				postings[term] = set
			}
			set[rel] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Path < recs[j].Path })
	out := map[string][]string{}
	for term, set := range postings {
		paths := make([]string, 0, len(set))
		for p := range set {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		out[term] = paths
	}
	return out, recs
}

// assertConverged compares the indexer against the naive reference: every
// term's search results must match, the indexer must not know extra terms,
// and Files() must mirror the surviving files on disk.
func assertConverged(t *testing.T, ix *Indexer, dir string) {
	t.Helper()
	wantPostings, wantRecs := naiveReference(t, dir)

	for term, want := range wantPostings {
		got := ix.Search(term)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Search(%q) = %v, want %v", term, got, want)
		}
	}
	// No extra terms may linger in the index.
	for term := range ix.idx.postings {
		if _, ok := wantPostings[term]; !ok {
			t.Fatalf("index has stale term %q with paths %v", term, ix.Search(term))
		}
	}

	gotRecs := ix.Files()
	if len(gotRecs) != len(wantRecs) {
		t.Fatalf("Files() returned %d records, want %d\ngot:  %v\nwant: %v", len(gotRecs), len(wantRecs), gotRecs, wantRecs)
	}
	for i := range wantRecs {
		g, w := gotRecs[i], wantRecs[i]
		if g.Path != w.Path || g.Size != w.Size || g.Mtime != w.Mtime {
			t.Fatalf("Files()[%d] = %+v, want %+v", i, g, w)
		}
	}
}

// ---------- scenario helpers ----------

func writeFile(t *testing.T, root, rel, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// setupCrashScenario creates a source directory, indexes a baseline, then
// applies a mix of additions, modifications and deletions. It returns the
// paths needed to Open the indexer.
func setupCrashScenario(t *testing.T) (sourceDir, indexDir, dbPath string) {
	t.Helper()
	base := t.TempDir()
	sourceDir = filepath.Join(base, "src")
	indexDir = filepath.Join(base, "idx")
	dbPath = filepath.Join(base, "files.db")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	old := time.Unix(time.Now().Unix()-3600, 0)
	// Baseline: 250 files across subdirectories.
	for i := 0; i < 250; i++ {
		rel := fmt.Sprintf("dir%d/file%03d.txt", i%5, i)
		content := fmt.Sprintf("alpha beta gamma doc%03d shared common", i)
		writeFile(t, sourceDir, rel, content, old)
	}

	ix, err := Open(sourceDir, indexDir, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	assertConverged(t, ix, sourceDir)
	ix.Close()

	// Changes: 120 additions, 80 modifications, 70 deletions.
	newer := time.Unix(time.Now().Unix()-1800, 0)
	for i := 0; i < 120; i++ {
		rel := fmt.Sprintf("dir%d/new%03d.txt", i%5, i)
		content := fmt.Sprintf("delta epsilon fresh%03d shared", i)
		writeFile(t, sourceDir, rel, content, old)
	}
	for i := 0; i < 80; i++ {
		rel := fmt.Sprintf("dir%d/file%03d.txt", i%5, i)
		content := fmt.Sprintf("zeta eta modified%03d common", i)
		writeFile(t, sourceDir, rel, content, newer)
	}
	for i := 100; i < 170; i++ {
		rel := fmt.Sprintf("dir%d/file%03d.txt", i%5, i)
		if err := os.Remove(filepath.Join(sourceDir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	// Ignored files: non-.txt and invalid UTF-8 must be skipped everywhere.
	writeFile(t, sourceDir, "notes.md", "alpha beta ignored", old)
	bad := filepath.Join(sourceDir, "bad.txt")
	if err := os.WriteFile(bad, []byte{'a', 0xff, 0xfe, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	return sourceDir, indexDir, dbPath
}

func reindexExpectCrash(t *testing.T, ix *Indexer) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected simulated crash, Reindex returned normally")
		}
		if err, ok := r.(error); !ok || !errors.Is(err, errSimulatedCrash) {
			panic(r)
		}
	}()
	ix.Reindex()
}

// recoverAndConverge opens a fresh Indexer on the same directories (all
// in-memory state from the crashed instance is gone) and runs one full
// Reindex, then verifies convergence against the naive reference.
func recoverAndConverge(t *testing.T, sourceDir, indexDir, dbPath string) {
	t.Helper()
	ix, err := Open(sourceDir, indexDir, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	assertConverged(t, ix, sourceDir)
}

// ---------- test 1: crash convergence ----------

func TestCrashConvergence(t *testing.T) {
	type crashAt struct {
		name string
		set  func(fire *int) // installs the hook; *fire counts hook invocations
	}
	points := []crashAt{}
	pointsFor := map[string][]int{
		pointBeforeCommit: {0, 1}, // first and second batch
		pointAfterCommit:  {0, 1},
		pointBeforeSync:   {0}, // once per scan
		pointAfterSync:    {0},
	}
	for _, p := range []string{pointBeforeCommit, pointAfterCommit, pointBeforeSync, pointAfterSync} {
		p := p
		for _, nth := range pointsFor[p] {
			nth := nth
			points = append(points, crashAt{
				name: fmt.Sprintf("%s#%d", p, nth),
				set: func(fire *int) {
					crashHook = func(point string) {
						if point == p {
							if *fire == nth {
								panic(errSimulatedCrash)
							}
							*fire++
						}
					}
				},
			})
		}
	}
	// Mid-batch crash: panic while reading files for a segment, before any
	// commit of that batch.
	points = append(points, crashAt{
		name: "midBatch",
		set: func(fire *int) {
			readHook = func(path string) {
				if *fire == 150 { // well into the second batch of changes
					panic(errSimulatedCrash)
				}
				*fire++
			}
		},
	})

	for _, tc := range points {
		t.Run(tc.name, func(t *testing.T) {
			crashHook = nil
			readHook = nil
			t.Cleanup(func() { crashHook = nil; readHook = nil })

			sourceDir, indexDir, dbPath := setupCrashScenario(t)

			ix, err := Open(sourceDir, indexDir, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			fire := 0
			tc.set(&fire)
			reindexExpectCrash(t, ix)
			// Deliberately do not Close ix: a crash loses all memory state.
			crashHook = nil
			readHook = nil

			recoverAndConverge(t, sourceDir, indexDir, dbPath)
		})
	}
}

// ---------- test 2: same-second rewrite ----------

func TestSameSecondRewrite(t *testing.T) {
	crashHook = nil
	readHook = nil
	t.Cleanup(func() { crashHook = nil; readHook = nil })

	base := t.TempDir()
	sourceDir := filepath.Join(base, "src")
	indexDir := filepath.Join(base, "idx")
	dbPath := filepath.Join(base, "files.db")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	stableMtime := time.Unix(now-3600, 0) // far older than any scan start
	freshMtime := time.Unix(now, 0)

	writeFile(t, sourceDir, "stable.txt", "alpha stable", stableMtime)
	writeFile(t, sourceDir, "fresh.txt", "hello world", freshMtime) // 11 bytes

	ix, err := Open(sourceDir, indexDir, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	// During the first scan, right after fresh.txt has been read, overwrite
	// it with different equal-length content and restore its mtime.
	rewritten := false
	readHook = func(path string) {
		if path != "fresh.txt" || rewritten {
			return
		}
		rewritten = true
		writeFile(t, sourceDir, "fresh.txt", "world peace", freshMtime) // also 11 bytes
	}
	if err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	if !rewritten {
		t.Fatal("fresh.txt was never read during the first scan")
	}

	// The first scan committed the old content.
	if got := ix.Search("hello"); !reflect.DeepEqual(got, []string{"fresh.txt"}) {
		t.Fatalf("after scan 1, Search(hello) = %v", got)
	}
	if got := ix.Search("peace"); len(got) != 0 {
		t.Fatalf("after scan 1, Search(peace) = %v", got)
	}

	// Second scan: observe exactly which files get read.
	var reads []string
	readHook = func(path string) { reads = append(reads, path) }
	if err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	readHook = nil

	// The silent same-second rewrite must have been picked up.
	if got := ix.Search("peace"); !reflect.DeepEqual(got, []string{"fresh.txt"}) {
		t.Fatalf("after scan 2, Search(peace) = %v", got)
	}
	if got := ix.Search("hello"); len(got) != 0 {
		t.Fatalf("after scan 2, Search(hello) = %v", got)
	}

	// fresh.txt was recent enough to be re-read...
	if !contains(reads, "fresh.txt") {
		t.Fatalf("scan 2 did not re-read fresh.txt; reads = %v", reads)
	}
	// ...but the old, untouched stable.txt must not have been read.
	if contains(reads, "stable.txt") {
		t.Fatalf("scan 2 unnecessarily re-read stable.txt; reads = %v", reads)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
