package events

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeJSONLEvents writes seq-stamped bead.created events to path as plain JSONL,
// one per line — the on-disk shape of an active log or an in-flight rotating file.
func writeJSONLEvents(t *testing.T, path string, seqs ...uint64) {
	t.Helper()
	var b strings.Builder
	for _, s := range seqs {
		fmt.Fprintf(&b, `{"seq":%d,"type":%q,"subject":"s%d"}`+"\n", s, string(BeadCreated), s)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func seqsOf(evts []Event) []uint64 {
	out := make([]uint64, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.Seq)
	}
	return out
}

// TestReadFilteredWithInFlightIncludesRotatingFiles is the reader-level guard for
// the async-compression drop: a just-rotated active log is renamed to a plain
// events.jsonl.rotating-* file and gzipped in the background, so between the
// rename and the canonical .gz those events are visible to neither the archive
// walker nor the active-file scan. ReadFiltered misses them; ReadFilteredWithInFlight
// folds them back in, in seq order.
func TestReadFilteredWithInFlightIncludesRotatingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// A fully-compressed earlier rotation: seq 1 lives only in a canonical .gz.
	gzSrc := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z-seq-1-1")
	writeJSONLEvents(t, gzSrc, 1)
	gz := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 1))
	var stderr bytes.Buffer
	if err := gzipAndArchive(gzSrc, gz, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}

	// An in-flight rotation whose gzip has NOT finished: seq 2,3 live only in the
	// plain-JSONL rotating-* file (no .gz yet).
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-2-3"), 2, 3)

	// The fresh active file opened after that rotation.
	writeJSONLEvents(t, path, 4, 5)

	// Baseline: ReadFiltered lists only .gz archives + active, so it MISSES the
	// in-flight window (seq 2,3) — the exact drop this guards.
	base, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if got := seqsOf(base); !reflect.DeepEqual(got, []uint64{1, 4, 5}) {
		t.Fatalf("ReadFiltered seqs = %v, want [1 4 5] (the in-flight gap)", got)
	}

	// ReadFilteredWithInFlight folds the rotating window back in, in seq order.
	all, err := ReadFilteredWithInFlight(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFilteredWithInFlight: %v", err)
	}
	if got := seqsOf(all); !reflect.DeepEqual(got, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("ReadFilteredWithInFlight seqs = %v, want [1 2 3 4 5]", got)
	}

	// AfterSeq fully excludes the rotating window (last seq 3 <= 3) without opening it.
	after, err := ReadFilteredWithInFlight(path, Filter{AfterSeq: 3})
	if err != nil {
		t.Fatalf("ReadFilteredWithInFlight(AfterSeq=3): %v", err)
	}
	if got := seqsOf(after); !reflect.DeepEqual(got, []uint64{4, 5}) {
		t.Fatalf("ReadFilteredWithInFlight(AfterSeq=3) seqs = %v, want [4 5]", got)
	}
}

// TestReadFilteredWithInFlightDedupsArchiveRotatingOverlap covers the instant
// after gzipAndArchive renames the .gz into place but before it removes the
// source rotating file: both cover the same seq window and the merged read must
// emit each event once, not twice.
func TestReadFilteredWithInFlightDedupsArchiveRotatingOverlap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// The in-flight rotating file (stays on disk).
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-2-3"), 2, 3)
	// A canonical .gz for the SAME window, gzipped from a throwaway source so the
	// rotating file above is left in place — modeling the coexistence window.
	gzSrc := filepath.Join(dir, "gz-source.jsonl")
	writeJSONLEvents(t, gzSrc, 2, 3)
	gz := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 5, 0, 0, time.UTC), 2, 3))
	var stderr bytes.Buffer
	if err := gzipAndArchive(gzSrc, gz, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}
	writeJSONLEvents(t, path, 4)

	all, err := ReadFilteredWithInFlight(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFilteredWithInFlight: %v", err)
	}
	if got := seqsOf(all); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Fatalf("overlap seqs = %v, want [2 3 4] (deduped)", got)
	}
}

func TestReadFilteredWithInFlightKeepsLimitForStableArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	firstSource := filepath.Join(dir, "first-archive-source.jsonl")
	writeJSONLEvents(t, firstSource, 1, 2)
	firstArchive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 2))
	if err := gzipAndArchive(firstSource, firstArchive, &stderr); err != nil {
		t.Fatalf("gzip first archive: %v", err)
	}
	secondSource := filepath.Join(dir, "second-archive-source.jsonl")
	writeJSONLEvents(t, secondSource, 3, 4)
	secondArchive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 5, 0, 0, time.UTC), 3, 4))
	if err := gzipAndArchive(secondSource, secondArchive, &stderr); err != nil {
		t.Fatalf("gzip second archive: %v", err)
	}
	writeJSONLEvents(t, path, 5)

	got, err := ReadFilteredWithInFlight(path, Filter{Limit: 1})
	if err != nil {
		t.Fatalf("ReadFilteredWithInFlight: %v", err)
	}
	if seqs := seqsOf(got); !reflect.DeepEqual(seqs, []uint64{1}) {
		t.Fatalf("limited stable-archive seqs = %v, want [1]", seqs)
	}
}

func TestReadFilteredWithInFlightSurvivesRotatingPromotion(t *testing.T) {
	for _, timing := range []string{"between scans", "between list and open"} {
		t.Run(timing, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-1-2")
			archive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 5, 0, 0, time.UTC), 1, 2))
			writeJSONLEvents(t, rotating, 1, 2)
			writeJSONLEvents(t, path, 3)

			promoted := false
			promote := func() {
				if promoted {
					return
				}
				promoted = true
				var stderr bytes.Buffer
				if err := gzipAndArchive(rotating, archive, &stderr); err != nil {
					t.Fatalf("promote rotating file: %v (stderr %q)", err, stderr.String())
				}
			}

			previous := readRotationDir
			t.Cleanup(func() { readRotationDir = previous })
			readRotationDir = func(path string) ([]os.DirEntry, error) {
				switch timing {
				case "between scans":
					// ReadFiltered's archive snapshot has already completed. Promote
					// before the post-active snapshot so that snapshot sees only the
					// newly-installed archive and no rotating source.
					promote()
					return os.ReadDir(path)
				case "between list and open":
					// Return the stale rotating entry after promoting it. The reader
					// must open the derived archive fallback when the source vanishes.
					entries, err := os.ReadDir(path)
					promote()
					return entries, err
				default:
					t.Fatalf("unknown promotion timing %q", timing)
					return nil, nil
				}
			}

			got, err := ReadFilteredWithInFlight(path, Filter{})
			if err != nil {
				t.Fatalf("ReadFilteredWithInFlight: %v", err)
			}
			if seqs := seqsOf(got); !reflect.DeepEqual(seqs, []uint64{1, 2, 3}) {
				t.Fatalf("promotion-safe seqs = %v, want [1 2 3]", seqs)
			}
		})
	}
}

// seedRecorderWithRotation creates a fresh recorder, writes recordsBefore
// events, force-rotates, then writes recordsAfter events. Returns the
// directory holding the active log + archives; the recorder is
// closed via t.Cleanup so callers can read disk state directly.
func seedRecorderWithRotation(t *testing.T, recordsBefore, recordsAfter int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rec.Close()
	})
	for i := 0; i < recordsBefore; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("pre-%d", i)})
	}
	if recordsBefore > 0 {
		res, err := rec.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate: %v", err)
		}
		if res.Done != nil {
			<-res.Done
		}
	}
	for i := 0; i < recordsAfter; i++ {
		rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: fmt.Sprintf("post-%d", i)})
	}
	return dir
}

func TestReadAllSpansArchivesAndActive(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	got, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	// 4 pre-rotate + 1 anchor + 3 post-rotate = 8 events
	if len(got) != 8 {
		t.Fatalf("ReadAll returned %d events, want 8", len(got))
	}

	// Seqs are monotonically increasing.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not monotonic at index %d: %d <= %d", i, got[i].Seq, got[i-1].Seq)
		}
	}

	// First 4 should be pre-rotate events; index 4 the anchor; rest post-rotate.
	for i := 0; i < 4; i++ {
		if got[i].Type != BeadCreated || !strings.HasPrefix(got[i].Subject, "pre-") {
			t.Errorf("got[%d] = %+v, want pre-rotate BeadCreated", i, got[i])
		}
	}
	if got[4].Type != EventsRotated {
		t.Errorf("got[4].Type = %q, want %q (anchor)", got[4].Type, EventsRotated)
	}
	for i := 5; i < 8; i++ {
		if got[i].Type != BeadClosed || !strings.HasPrefix(got[i].Subject, "post-") {
			t.Errorf("got[%d] = %+v, want post-rotate BeadClosed", i, got[i])
		}
	}
}

func TestReadFilteredSkipsNonOverlappingArchives(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	// AfterSeq=4 → archive (seqs 1..4) is fully excluded; only anchor + post events.
	got, err := ReadFiltered(path, Filter{AfterSeq: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("ReadFiltered(AfterSeq=4) returned %d events, want 4 (anchor + 3 post)", len(got))
	}
	for _, e := range got {
		if e.Seq <= 4 {
			t.Errorf("event seq %d should be > 4", e.Seq)
		}
	}
}

func TestReadFilteredAcrossArchivesAppliesPredicates(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	got, err := ReadFiltered(path, Filter{Type: BeadCreated})
	if err != nil {
		t.Fatal(err)
	}
	// 4 pre-rotate BeadCreated; 0 anchor (different type); 0 post-rotate (BeadClosed)
	if len(got) != 4 {
		t.Fatalf("ReadFiltered(Type=BeadCreated) returned %d events, want 4", len(got))
	}
	for _, e := range got {
		if e.Type != BeadCreated {
			t.Errorf("Type = %q, want %q", e.Type, BeadCreated)
		}
	}
}

func TestReadFilteredAcrossArchivesAppliesLimit(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	got, err := ReadFiltered(path, Filter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ReadFiltered(Limit=2) returned %d events, want 2", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("got seqs = [%d,%d], want [1,2]", got[0].Seq, got[1].Seq)
	}
}

func TestReadLatestSeqSpansArchiveOnlyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z-seq-100-102")
	const body = `{"seq":100,"type":"x"}
{"seq":101,"type":"y"}
{"seq":102,"type":"z"}
`
	if err := os.WriteFile(rotating, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 100, 102))
	var stderr bytes.Buffer
	if err := gzipAndArchive(rotating, dest, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}

	seq, err := ReadLatestSeq(path)
	if err != nil {
		t.Fatalf("ReadLatestSeq: %v", err)
	}
	if seq != 102 {
		t.Fatalf("ReadLatestSeq archive-only = %d, want 102", seq)
	}
}

func TestReadAllSurvivesMultipleRotations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	const rotations = 3
	const perBatch = 3
	for r := 0; r < rotations; r++ {
		for i := 0; i < perBatch; i++ {
			rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("r%d-%d", r, i)})
		}
		res, err := rec.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate r=%d: %v", r, err)
		}
		if res.Done != nil {
			<-res.Done
		}
	}
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "tail"})

	// Expect 3 archives (3 rotations) — each with 3 batch events; each
	// rotation also writes 1 anchor to the next active file.
	// Total visible to ReadAll:
	//   3 rotations × 3 events = 9 batch events
	//   3 anchors (from rotations 1, 2, 3)
	//   1 tail = 13 events
	got, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 13 {
		t.Fatalf("ReadAll across %d rotations returned %d events, want 13", rotations, len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not monotonic at index %d: %d <= %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
	// Last event is the tail.
	if got[len(got)-1].Subject != "tail" {
		t.Errorf("last event subject = %q, want %q", got[len(got)-1].Subject, "tail")
	}
}

func TestReadFilteredHandlesMissingArchiveDir(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir", "events.jsonl")

	got, err := ReadFiltered(missing, Filter{})
	if err != nil {
		t.Errorf("ReadFiltered missing dir: %v", err)
	}
	if got != nil {
		t.Errorf("got %d events, want nil", len(got))
	}
}

func TestReadFilteredIgnoresUnrelatedFiles(t *testing.T) {
	dir := seedRecorderWithRotation(t, 3, 2)
	path := filepath.Join(dir, "events.jsonl")
	// Drop a foreign file in the dir to confirm the archive walker
	// doesn't trip on it.
	if err := os.WriteFile(filepath.Join(dir, "scratch.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl.archive-bogus.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("ReadFiltered returned %d events, want 6 (3 pre + 1 anchor + 2 post)", len(got))
	}
}

// TestReadFilteredTailCrossesArchiveBoundary is the reader-level fix for
// ga-96zjze: a sparse type whose active-file occurrences don't fill the
// requested tail must extend into archives (newest first), not stop short.
func TestReadFilteredTailCrossesArchiveBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	// 20 pre-rotate events; every 4th (i=0,4,8,12,16 -> seq 1,5,9,13,17) is the
	// sparse type under test, mirroring convoy.closed's low density in the
	// original repro.
	for i := 0; i < 20; i++ {
		typ := BeadClosed
		if i%4 == 0 {
			typ = ConvoyClosed
		}
		rec.Record(Event{Type: typ, Actor: "human", Subject: fmt.Sprintf("s%d", i)})
	}
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done
	}
	// Active file after rotation: anchor (seq 21) + 2 more, only one matching.
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "post-0"})   // seq 22
	rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: "post-1"}) // seq 23
	rec.Close()                                                              //nolint:errcheck // test cleanup

	// Active file alone has exactly 1 match (seq 23) — short of limit=3, so
	// this must reach into the archive for the newest 2 of its 5 matches
	// (seq 13, 17), not the oldest 2 (seq 1, 5).
	got, err := ReadFilteredTail(path, Filter{Type: ConvoyClosed}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if seqs := seqsOf(got); !reflect.DeepEqual(seqs, []uint64{13, 17, 23}) {
		t.Fatalf("tail seqs = %v, want [13 17 23] (newest 3 matches, ascending — not the oldest 3)", seqs)
	}
}

// TestReadFilteredTailFewerMatchesThanLimit is the explicit pagination
// correctness check called for on ga-96zjze: requesting more matches than
// exist anywhere in retained history (including zero) must return exactly
// what exists — not panic, truncate wrong, or duplicate — not merely be fast.
func TestReadFilteredTailFewerMatchesThanLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: "c1"}) // seq 1
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "n1"})   // seq 2
	rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: "c2"}) // seq 3
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done
	}
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "n2"}) // seq 5, no more matches anywhere
	rec.Close()                                                        //nolint:errcheck // test cleanup

	// Total ConvoyClosed matches across all retained history = 2. Requesting
	// 5 must return exactly those 2, in order, not hang and not error.
	got, err := ReadFilteredTail(path, Filter{Type: ConvoyClosed}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Subject != "c1" || got[1].Subject != "c2" {
		t.Fatalf("got %+v, want exactly [c1 c2] (exhausted history, fewer than limit)", got)
	}

	// A type with zero occurrences anywhere: the unavoidable full-history
	// case. Must terminate cleanly with an empty result, not hang.
	none, err := ReadFilteredTail(path, Filter{Type: "totally.absent.type"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("got %d events for a never-emitted type, want 0", len(none))
	}
}

// TestReadFilteredTailDoesNotOpenUnneededOlderArchives proves the early-exit
// is real, not just correct: this is ga-96zjze's actual complaint (a sparse
// type forcing a full decompress of every archive) rendered as a test that
// fails loudly if the bound regresses. The oldest archive is replaced with
// garbage that errors on open; if the newest archive plus the active file
// already satisfy the request, the corrupted archive must never be touched.
func TestReadFilteredTailDoesNotOpenUnneededOlderArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	// Archive 1 (oldest): seq 1..5, all matching — would satisfy the request
	// on its own, which is exactly why it must never be opened.
	for i := 0; i < 5; i++ {
		rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: fmt.Sprintf("old-%d", i)})
	}
	res1, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate 1: %v", err)
	}
	if res1.Done != nil {
		<-res1.Done
	}

	// Archive 2 (newest): seq 6..9 (anchor, match, match, noise).
	rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: "new-0"})
	rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: "new-1"})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "new-2"})
	res2, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate 2: %v", err)
	}
	if res2.Done != nil {
		<-res2.Done
	}

	rec.Record(Event{Type: ConvoyClosed, Actor: "human", Subject: "active"}) // seq 11
	rec.Close()                                                              //nolint:errcheck // test cleanup

	archives, err := archiveFilesIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 {
		t.Fatalf("want 2 archives, got %d", len(archives))
	}
	oldest := filepath.Join(dir, archives[0].Basename) // ascending FirstSeq: [0] is the oldest
	if err := os.WriteFile(oldest, []byte("not a gzip file"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Active (1 match) + newest archive's 2 matches satisfy limit=2 without
	// ever needing the corrupted oldest archive.
	got, err := ReadFilteredTail(path, Filter{Type: ConvoyClosed}, 2)
	if err != nil {
		t.Fatalf("ReadFilteredTail errored — it must not have opened the corrupted older archive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Subject != "new-1" || got[1].Subject != "active" {
		t.Fatalf("subjects = [%s %s], want [new-1 active]", got[0].Subject, got[1].Subject)
	}
}
