package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// imagePaths reads paths the way terminals drop them.
func TestImagePaths(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.png", "my shot.JPG", "q'd.gif"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	os.Mkdir(filepath.Join(dir, "d.png"), 0o700)
	a, spaced, quoted := filepath.Join(dir, "a.png"), filepath.Join(dir, "my shot.JPG"), filepath.Join(dir, "q'd.gif")
	msg := "see " + a + ", " + dir + `/my\ shot.JPG and '` + spaced + `' "` + quoted + `" ` + dir + "/d.png " + dir + "/none.png notes.txt"
	if got := imagePaths(msg); !slices.Equal(got, []string{a, spaced, spaced, quoted}) {
		t.Fatalf("imagePaths = %q", got)
	}
}
