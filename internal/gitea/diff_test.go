package gitea

import (
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

// A diff with two files, multiple hunks, and a mix of additions, deletions,
// and context lines.
const sampleDiff = `diff --git a/foo.go b/foo.go
index 1111111..2222222 100644
--- a/foo.go
+++ b/foo.go
@@ -1,4 +1,5 @@
 package foo

-import "fmt"
+import "errors"
+import "log"

@@ -10,3 +11,4 @@ func A() {
 	a := 1
 	b := 2
+	c := 3
 	_ = a
diff --git a/bar.go b/bar.go
index 3333333..4444444 100644
--- a/bar.go
+++ b/bar.go
@@ -5,2 +5,2 @@ func B() {
-	old := 1
+	new := 1
 	_ = old
`

func TestParseUnifiedDiff_TwoFilesMixed(t *testing.T) {
	dm := ParseUnifiedDiff(sampleDiff)

	if len(dm.Files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(dm.Files), keys(dm.Files))
	}

	foo, ok := dm.Files["foo.go"]
	if !ok {
		t.Fatalf("foo.go missing from diff map")
	}
	bar, ok := dm.Files["bar.go"]
	if !ok {
		t.Fatalf("bar.go missing from diff map")
	}

	// ----- foo.go, hunk 1: @@ -1,4 +1,5 @@ -----
	// new side:
	//   1 " package foo"   (ctx) -> new 1, old 1
	//   2 " "              (ctx) -> new 2, old 2
	//   3 "-import \"fmt\"" (del) -> old 3
	//   3 "+import \"errors\"" (add) -> new 3
	//   4 "+import \"log\""    (add) -> new 4
	//   5 " "              (ctx) -> new 5, old 4
	wantFooNew := []int{1, 2, 3, 4, 5}
	wantFooOld := []int{1, 2, 3, 4}
	// hunk 2: @@ -10,3 +11,4 @@
	//   11 " a := 1" ctx -> new 11, old 10
	//   12 " b := 2" ctx -> new 12, old 11
	//   13 "+c := 3" add -> new 13
	//   14 " _ = a"  ctx -> new 14, old 12
	wantFooNew = append(wantFooNew, 11, 12, 13, 14)
	wantFooOld = append(wantFooOld, 10, 11, 12)

	assertLines(t, "foo.go new", foo.NewLines, wantFooNew)
	assertLines(t, "foo.go old", foo.OldLines, wantFooOld)

	// Lines outside the diff must not be present.
	for _, n := range []int{6, 7, 8, 9, 10, 15} {
		if foo.NewLines[n] {
			t.Errorf("foo.go new line %d should NOT be in diff", n)
		}
	}

	// ----- bar.go: @@ -5,2 +5,2 @@ -----
	//   5 "-old := 1" del -> old 5
	//   5 "+new := 1" add -> new 5
	//   6 " _ = old"  ctx -> new 6, old 6
	assertLines(t, "bar.go new", bar.NewLines, []int{5, 6})
	assertLines(t, "bar.go old", bar.OldLines, []int{5, 6})
}

func TestParseUnifiedDiff_NewFile(t *testing.T) {
	const d = `diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..abcdef0
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,3 @@
+line one
+line two
+line three
`
	dm := ParseUnifiedDiff(d)
	fd, ok := dm.Files["new.txt"]
	if !ok {
		t.Fatalf("new.txt missing")
	}
	assertLines(t, "new.txt new", fd.NewLines, []int{1, 2, 3})
	if len(fd.OldLines) != 0 {
		t.Errorf("new file should have no old lines, got %v", fd.OldLines)
	}
}

func TestParseUnifiedDiff_DeletedFile(t *testing.T) {
	const d = `diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index abcdef0..0000000
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-bye one
-bye two
`
	dm := ParseUnifiedDiff(d)
	fd, ok := dm.Files["gone.txt"]
	if !ok {
		t.Fatalf("gone.txt missing (should key by old path on deletion)")
	}
	assertLines(t, "gone.txt old", fd.OldLines, []int{1, 2})
	if len(fd.NewLines) != 0 {
		t.Errorf("deleted file should have no new lines, got %v", fd.NewLines)
	}
}

func TestParseHunkHeader(t *testing.T) {
	cases := []struct {
		in         string
		oldS, newS int
		ok         bool
	}{
		{"@@ -1,4 +1,5 @@", 1, 1, true},
		{"@@ -10,3 +11,4 @@ func A() {", 10, 11, true},
		{"@@ -5 +6 @@", 5, 6, true},     // counts omitted
		{"@@ -0,0 +1,3 @@", 0, 1, true}, // new file
		{"not a hunk", 0, 0, false},
	}
	for _, tc := range cases {
		o, n, ok := parseHunkHeader(tc.in)
		if ok != tc.ok || (ok && (o != tc.oldS || n != tc.newS)) {
			t.Errorf("parseHunkHeader(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tc.in, o, n, ok, tc.oldS, tc.newS, tc.ok)
		}
	}
}

func TestPosition_RoundTrip(t *testing.T) {
	dm := ParseUnifiedDiff(sampleDiff)
	// foo.go new line 13 was an added line.
	newPos, oldPos, ok := dm.Position("foo.go", 13, model.SideNew)
	if !ok || newPos != 13 || oldPos != 0 {
		t.Errorf("Position(foo.go,13,NEW) = (%d,%d,%v), want (13,0,true)", newPos, oldPos, ok)
	}
	// bar.go old line 5 was a removed line.
	newPos, oldPos, ok = dm.Position("bar.go", 5, model.SideOld)
	if !ok || newPos != 0 || oldPos != 5 {
		t.Errorf("Position(bar.go,5,OLD) = (%d,%d,%v), want (0,5,true)", newPos, oldPos, ok)
	}
	// A line outside the diff.
	if _, _, ok := dm.Position("foo.go", 100, model.SideNew); ok {
		t.Errorf("Position(foo.go,100,NEW) should be ok=false")
	}
}

// --- helpers ---

func assertLines(t *testing.T, label string, got map[int]bool, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d lines %v, want %d %v", label, len(got), sortedKeys(got), len(want), want)
		return
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s: missing line %d (have %v)", label, w, sortedKeys(got))
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// simple insertion sort (small maps)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
