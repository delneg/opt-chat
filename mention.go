package main

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The page's @ menu, as Claude Code's: the files and folders a partial path
// can complete to. Claude Code's own index (file_suggestions) is built for
// a project, and finds nothing in a home folder, where Opt works.

// fileSuggestion is one item: the path as the page puts it in the message,
// under ~ when it is, so that it attaches as a typed path does.
type fileSuggestion struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitzero"`
}

const suggestionsMax = 15

// fileSuggestions completes query, a path absolute, under ~, or relative to
// cwd: the entries of its folder that start with (then contain) its last
// part; and, for a name alone, files and folders under cwd whose names
// contain it, found within a quarter of a second.
func fileSuggestions(cwd, query string) []fileSuggestion {
	home, _ := os.UserHomeDir()
	show := func(p string, dir bool) fileSuggestion {
		if home != "" && strings.HasPrefix(p, home+"/") {
			p = "~" + strings.TrimPrefix(p, home)
		}
		if dir {
			p += "/"
		}
		return fileSuggestion{Path: p, Dir: dir}
	}
	if query == "~" {
		query = "~/"
	}
	dirPart, prefix := "", query // as typed: "." and ".." are prefixes to complete, not folders
	if i := strings.LastIndex(query, "/"); i >= 0 {
		dirPart, prefix = query[:i+1], query[i+1:]
	}
	dir := dirPart
	switch {
	case strings.HasPrefix(dirPart, "~/"):
		dir = filepath.Join(home, dirPart[2:])
	case !filepath.IsAbs(dirPart):
		dir = filepath.Join(cwd, dirPart)
	}
	lp := strings.ToLower(prefix)
	var starts, contains []fileSuggestion
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			name := strings.ToLower(e.Name())
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(lp, ".") {
				continue // hidden, unless asked for
			}
			s := show(filepath.Join(dir, e.Name()), isDir(dir, e))
			switch {
			case strings.HasPrefix(name, lp):
				starts = append(starts, s)
			case len(lp) >= 2 && strings.Contains(name, lp): // one letter, or ".", in a name says little
				contains = append(contains, s)
			}
		}
	}
	out := append(starts, contains...)
	if len(query) >= 3 && !strings.ContainsAny(query, "/~") {
		for _, s := range findNames(cwd, lp, 250*time.Millisecond, show) {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out[:min(len(out), suggestionsMax)]
}

// isDir follows a symlink to a folder.
func isDir(dir string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	fi, err := os.Stat(filepath.Join(dir, e.Name()))
	return err == nil && fi.IsDir()
}

// skipDirs are folders a search by name never enters: big, and never what
// the user means.
var skipDirs = map[string]bool{"node_modules": true, "Library": true, "vendor": true, "target": true, "dist": true, "build": true}

// findNames walks from root, breadth first, for names containing lname,
// until budget runs out; the closest first, the names it starts first.
func findNames(root, lname string, budget time.Duration, show func(string, bool) fileSuggestion) []fileSuggestion {
	type hit struct {
		s      fileSuggestion
		starts bool
		depth  int
	}
	var hits []hit
	deadline := time.Now().Add(budget)
	level := []string{root}
	for depth := 1; depth <= 8 && len(level) > 0 && len(hits) < 50; depth++ {
		var next []string
		for _, dir := range level {
			if time.Now().After(deadline) {
				goto done
			}
			es, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range es {
				name := e.Name()
				if strings.HasPrefix(name, ".") {
					continue
				}
				p := filepath.Join(dir, name)
				if e.IsDir() && !skipDirs[name] {
					next = append(next, p)
				}
				if l := strings.ToLower(name); strings.Contains(l, lname) && depth > 1 { // depth 1: listed already
					hits = append(hits, hit{show(p, e.IsDir()), strings.HasPrefix(l, lname), depth})
				}
			}
		}
		level = next
	}
done:
	slices.SortStableFunc(hits, func(a, b hit) int {
		if a.starts != b.starts {
			if a.starts {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.depth, b.depth)
	})
	out := make([]fileSuggestion, len(hits))
	for i, h := range hits {
		out[i] = h.s
	}
	return out
}
