// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package ignore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/locations"
)

// TestMain redirects the git-shim anchor/shadow directories into a temp dir
// for the whole test binary run, so `go test` doesn't write into the
// developer's real Syncthing config/data directory.
func TestMain(m *testing.M) {
	if err := locations.SetBaseDir(locations.DataBaseDir, mustTempDir()); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func mustTempDir() string {
	d, err := os.MkdirTemp("", "syncthing-ignore-test-")
	if err != nil {
		panic(err)
	}
	return d
}

// newTestFS returns a real, disk-backed filesystem rooted at a fresh
// t.TempDir(). Real git needs actual files on disk -- unlike the old
// gobwas/glob matcher, this package can no longer be tested against an
// in-memory FilesystemTypeFake.
func newTestFS(t *testing.T) fs.Filesystem {
	t.Helper()
	if err := CheckGitAvailable(); err != nil {
		t.Skipf("git not available, skipping: %v", err)
	}
	return fs.NewFilesystem(fs.FilesystemTypeBasic, t.TempDir())
}

func writeFile(t *testing.T, ffs fs.Filesystem, path, content string) {
	t.Helper()
	if dir := filepath.Dir(path); dir != "." {
		if err := ffs.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.WriteFile(ffs, path, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}
}

func mustMatch(t *testing.T, m *Matcher) *Matcher {
	t.Helper()
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBasicMatch(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "*.log\n!keep.log\n")
	writeFile(t, testFs, "app.log", "")
	writeFile(t, testFs, "keep.log", "")
	writeFile(t, testFs, "app.txt", "")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if !m.Match("app.log", false).IsIgnored() {
		t.Error("app.log should be ignored")
	}
	if m.Match("keep.log", false).IsIgnored() {
		t.Error("keep.log should not be ignored (negated)")
	}
	if m.Match("app.txt", false).IsIgnored() {
		t.Error("app.txt should not be ignored")
	}
}

func TestNestedDirectoryScoping(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "*.tmp\n")
	writeFile(t, testFs, "sub/.gitignore", "*.cache\n")
	writeFile(t, testFs, "sub/a.tmp", "")
	writeFile(t, testFs, "sub/a.cache", "")
	writeFile(t, testFs, "a.cache", "") // root has no *.cache rule

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if !m.Match("sub/a.tmp", false).IsIgnored() {
		t.Error("sub/a.tmp should be ignored by the root .gitignore")
	}
	if !m.Match("sub/a.cache", false).IsIgnored() {
		t.Error("sub/a.cache should be ignored by sub/.gitignore")
	}
	if m.Match("a.cache", false).IsIgnored() {
		t.Error("a.cache at root should NOT be ignored -- sub/.gitignore's rule doesn't scope upward")
	}
}

// TestNegationCannotReachThroughExcludedParent locks in real git's
// documented (and independently verified, see the design notes this was
// built from) rule: a nested !pattern cannot re-include a path whose parent
// directory is itself excluded by a higher-level .gitignore.
func TestNegationCannotReachThroughExcludedParent(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "node_modules/\n")
	writeFile(t, testFs, "node_modules/.gitignore", "!keep-me.txt\n")
	writeFile(t, testFs, "node_modules/keep-me.txt", "")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if !m.Match("node_modules/keep-me.txt", false).IsIgnored() {
		t.Error("node_modules/keep-me.txt should still be ignored: negation cannot reach through an excluded parent directory")
	}
}

func TestDirectoryOnlyPattern(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "build/\n")
	writeFile(t, testFs, "build", "") // a *file* named build, not the directory

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if m.Match("build", false).IsIgnored() {
		t.Error("a file named 'build' should not match the directory-only pattern 'build/'")
	}
	if !m.Match("build", true).IsIgnored() {
		t.Error("a directory named 'build' should match the directory-only pattern 'build/'")
	}
}

func TestCanSkipDir(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "node_modules/\n")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	res := m.Match("node_modules", true)
	if !res.IsIgnored() || !res.CanSkipDir() {
		t.Error("an ignored directory should always report CanSkipDir")
	}
}

func TestDeletableFlag(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "(?d)*.bak\n*.tmp\n")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	del := m.Match("x.bak", false)
	if !del.IsIgnored() || !del.IsDeletable() {
		t.Error("x.bak should be ignored and deletable")
	}
	notDel := m.Match("x.tmp", false)
	if !notDel.IsIgnored() || notDel.IsDeletable() {
		t.Error("x.tmp should be ignored but not deletable")
	}
}

func TestCaseFoldFlag(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "(?i)thumbs.db\n")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	for _, name := range []string{"thumbs.db", "Thumbs.db", "THUMBS.DB"} {
		res := m.Match(name, false)
		if !res.IsIgnored() {
			t.Errorf("%s should be ignored via case-insensitive match", name)
		}
		if !res.IsCaseFolded() {
			t.Errorf("%s match should report IsCaseFolded", name)
		}
	}
	if m.Match("thumbs.dbx", false).IsIgnored() {
		t.Error("thumbs.dbx should not match")
	}
}

func TestHashStableAndChanges(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "*.log\n")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	h1 := m.Hash()
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	if m.Hash() != h1 {
		t.Error("hash should be stable across reloads with no changes")
	}

	writeFile(t, testFs, ".gitignore", "*.log\n*.tmp\n")
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	if m.Hash() == h1 {
		t.Error("hash should change after editing .gitignore")
	}
}

func TestReloadPicksUpNewNestedFile(t *testing.T) {
	testFs := newTestFS(t)
	writeFile(t, testFs, ".gitignore", "*.log\n")

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if m.Match("sub/x.cache", false).IsIgnored() {
		t.Fatal("sub/x.cache should not be ignored yet")
	}

	writeFile(t, testFs, "sub/.gitignore", "*.cache\n")
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}

	if !m.Match("sub/x.cache", false).IsIgnored() {
		t.Error("sub/x.cache should be ignored after the new nested .gitignore is picked up")
	}
}

func TestInternalAndTemporarySkip(t *testing.T) {
	testFs := newTestFS(t)
	// No .gitignore at all -- .stfolder/temp files should still be skipped
	// via fs.IsInternal/fs.IsTemporary short-circuits before any git call.
	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if !m.Match(".stfolder", true).IsIgnored() {
		t.Error(".stfolder should always be ignored (internal)")
	}
}

func TestWriteIgnoresRoundtrip(t *testing.T) {
	testFs := newTestFS(t)
	content := []string{"*.log", "!keep.log"}
	if err := WriteIgnores(testFs, gitignoreName, content); err != nil {
		t.Fatal(err)
	}

	m := mustMatch(t, New(testFs))
	defer m.Stop()

	if !m.Match("app.log", false).IsIgnored() {
		t.Error("app.log should be ignored after WriteIgnores")
	}
	got := m.Lines()
	if strings.Join(got, "\n") != strings.Join(content, "\n") {
		t.Errorf("Lines() = %v, want %v", got, content)
	}

	if err := WriteIgnores(testFs, gitignoreName, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := testFs.Lstat(gitignoreName); !fs.IsNotExist(err) {
		t.Error("WriteIgnores with empty content should remove the file")
	}
}

func TestParseFromReader(t *testing.T) {
	testFs := newTestFS(t)
	m := New(testFs)
	defer m.Stop()

	if err := m.Parse(strings.NewReader("*.log\n"), gitignoreName); err != nil {
		t.Fatal(err)
	}
	if !m.Match("app.log", false).IsIgnored() {
		t.Error("app.log should be ignored after Parse")
	}
}

func TestAsciiCaseInsensitiveExpand(t *testing.T) {
	cases := map[string]string{
		"abc":      "[aA][bB][cC]",
		"*.TMP":    "*.[tT][mM][pP]",
		"[Aa]bc":   "[Aa][bB][cC]",
		`foo\[bar`: `[fF][oO][oO]\[[bB][aA][rR]`,
	}
	for in, want := range cases {
		if got := asciiCaseInsensitiveExpand(in); got != want {
			t.Errorf("asciiCaseInsensitiveExpand(%q) = %q, want %q", in, got, want)
		}
	}
}
