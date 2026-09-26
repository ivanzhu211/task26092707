package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// naiveScan is an independent reference implementation: it walks src, reads
// every .txt file and builds the full term->paths index plus file records
// straight from the current directory content.
func naiveScan(t *testing.T, src string) (map[string][]string, []FileRecord) {
	t.Helper()
	termSets := map[string]map[string]bool{}
	var recs []FileRecord
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || filepath.Ext(d.Name()) != ".txt" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, term := range tokenize(content) {
			if termSets[term] == nil {
				termSets[term] = map[string]bool{}
			}
			termSets[term][rel] = true
		}
		recs = append(recs, FileRecord{Path: rel, Size: info.Size(), Mtime: info.ModTime().Unix()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	terms := map[string][]string{}
	for term, set := range termSets {
		paths := make([]string, 0, len(set))
		for p := range set {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		terms[term] = paths
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Path < recs[j].Path })
	return terms, recs
}

// verifyAgainstDisk asserts that Search results for every term match a naive
// full scan, and that Files() exactly matches the files on disk.
func verifyAgainstDisk(t *testing.T, ix *Indexer, src string) {
	t.Helper()
	wantTerms, wantRecs := naiveScan(t, src)
	for term, want := range wantTerms {
		if got := ix.Search(term); !reflect.DeepEqual(got, want) {
			t.Errorf("Search(%q) = %v, want %v", term, got, want)
		}
	}
	for _, absent := range []string{"nosuchterm", "qxzj"} {
		if got := ix.Search(absent); len(got) != 0 {
			t.Errorf("Search(%q) = %v, want empty", absent, got)
		}
	}
	gotRecs := ix.Files()
	if len(gotRecs) != len(wantRecs) {
		t.Fatalf("Files() has %d records, want %d", len(gotRecs), len(wantRecs))
	}
	for i, want := range wantRecs {
		got := gotRecs[i]
		if got.Path != want.Path || got.Size != want.Size || got.Mtime != want.Mtime {
			t.Errorf("Files()[%d] = %+v, want path/size/mtime of %+v", i, got, want)
		}
	}
}

func writeFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// setupScenario creates the baseline directory: 9 files with old, stable mtimes.
func setupScenario(t *testing.T, src string) {
	t.Helper()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 9; i++ {
		content := fmt.Sprintf("alpha beta gamma file%02d shared", i)
		writeFile(t, filepath.Join(src, fmt.Sprintf("dir%d/file%02d.txt", i%3, i)), content, old)
	}
	// A non-.txt file that must be ignored.
	writeFile(t, filepath.Join(src, "notes.md"), "alpha beta ignored", old)
}

// mutateScenario applies a mix of 3 additions, 3 modifications, 3 deletions.
func mutateScenario(t *testing.T, src string) {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("dir%d/new%02d.txt", i, i)),
			fmt.Sprintf("alpha delta fresh%02d shared", i), now)
	}
	for i := 3; i < 6; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("dir%d/file%02d.txt", i%3, i)),
			fmt.Sprintf("alpha epsilon modified%02d", i), now)
	}
	for i := 6; i < 9; i++ {
		if err := os.Remove(filepath.Join(src, fmt.Sprintf("dir%d/file%02d.txt", i%3, i))); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCrashHelper runs in a subprocess and simulates power loss by exiting
// at the Nth crash point. It never runs as part of a normal `go test`.
func TestCrashHelper(t *testing.T) {
	if os.Getenv("CRASH_HELPER") != "1" {
		t.Skip("helper process only")
	}
	n, _ := strconv.Atoi(os.Getenv("CRASH_AFTER"))
	count := 0
	crashHook = func(point string) {
		count++
		if count == n {
			os.Exit(2) // simulated power loss: no defers, no cleanup
		}
	}
	ix, err := Open(os.Getenv("SRC"), os.Getenv("IDX"), os.Getenv("DB"))
	if err != nil {
		os.Exit(1)
	}
	bs, _ := strconv.Atoi(os.Getenv("BATCH"))
	ix.batchSize = bs
	if err := ix.Reindex(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestCrashConvergence kills Reindex at every write-gap and mid-batch point,
// then verifies that a fresh Open plus one uninterrupted Reindex converges to
// the same result as a naive full index of the directory.
func TestCrashConvergence(t *testing.T) {
	const batch = 3
	for k := 1; ; k++ {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		idx := filepath.Join(root, "idx")
		db := filepath.Join(root, "files.db")

		setupScenario(t, src)
		ix, err := Open(src, idx, db)
		if err != nil {
			t.Fatal(err)
		}
		ix.batchSize = batch
		if err := ix.Reindex(); err != nil {
			t.Fatal(err)
		}
		ix.Close()
		mutateScenario(t, src)

		cmd := exec.Command(os.Args[0], "-test.run=TestCrashHelper", "-test.v")
		cmd.Env = append(os.Environ(),
			"CRASH_HELPER=1",
			"CRASH_AFTER="+strconv.Itoa(k),
			"SRC="+src, "IDX="+idx, "DB="+db,
			"BATCH="+strconv.Itoa(batch),
		)
		runErr := cmd.Run()
		if runErr == nil {
			break // k exceeded the number of crash points: all were tested
		}
		if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
			t.Fatalf("k=%d: helper failed unexpectedly: %v", k, runErr)
		}

		// Recover: fresh Open, memory rebuilt from disk only.
		ix2, err := Open(src, idx, db)
		if err != nil {
			t.Fatalf("k=%d: reopen: %v", k, err)
		}
		ix2.batchSize = batch
		if err := ix2.Reindex(); err != nil {
			t.Fatalf("k=%d: recovery reindex: %v", k, err)
		}
		verifyAgainstDisk(t, ix2, src)
		ix2.Close()
		t.Logf("k=%d: crash point verified", k)
	}
}
