// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package ignore

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/syncthing/syncthing/lib/fs"
)

const gitignoreName = ".gitignore"

// lineFlags captures the Syncthing-specific per-line metadata that plain
// git has no concept of. Indexed by 1-based line number within a given
// shadow .gitignore file (index 0 unused) so it can be recovered from a
// real git match via check-ignore -v's reported line number.
type lineFlags struct {
	protected bool
	caseFold  bool
}

// discoveredIgnore is one real .gitignore found during a Load() discovery
// pass, keyed by its slash-separated path relative to the folder root.
type discoveredIgnore struct {
	relPath  string
	rawLines []string
}

// discoverGitignores walks the whole folder looking for files literally
// named ".gitignore" at any depth. Every discovered file's content is read
// immediately (they're small; this is a rounding error next to Syncthing's
// real per-file hashing work), which both builds the shadow tree and gives
// us content-based change detection for free -- no separate mtime-tracking
// mechanism is needed, we just compare hashes across Load() calls.
func discoverGitignores(ffs fs.Filesystem) (map[string]discoveredIgnore, error) {
	found := make(map[string]discoveredIgnore)
	err := ffs.Walk(".", func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if info.IsDir() {
			if fs.IsInternal(path) {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Base(path) != gitignoreName {
			return nil
		}
		lines, rerr := readGitignoreLines(ffs, path)
		if rerr != nil {
			return rerr
		}
		rel := filepath.ToSlash(path)
		found[rel] = discoveredIgnore{relPath: rel, rawLines: lines}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

func readGitignoreLines(ffs fs.Filesystem, path string) ([]string, error) {
	fd, err := ffs.Open(path)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	var lines []string
	scanner := bufio.NewScanner(fd)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSpace(scanner.Text()))
	}
	return lines, scanner.Err()
}

// hashDiscovered produces a stable hash over the full discovered set,
// changing whenever any .gitignore is added, removed, or edited anywhere in
// the tree.
func hashDiscovered(discovered map[string]discoveredIgnore) string {
	keys := make([]string, 0, len(discovered))
	for k := range discovered {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		io.WriteString(h, k)
		h.Write([]byte{0})
		for _, line := range discovered[k].rawLines {
			io.WriteString(h, line)
			h.Write([]byte{0})
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// transformLine strips Syncthing's (?i)/(?d)/(?p) prefixes (real git has no
// concept of any of them) and returns the flags they carried alongside a
// line git can interpret natively. Prefixes may appear in any order and at
// most once each, mirroring the original hand-rolled parser's behavior.
// Blank lines and comments (both real "#..." and the old "//...", which
// becomes a harmless literal pattern under real git if not migrated) pass
// through unchanged since none of the prefixes match them.
//
// Ignored files are deletable by default (e.g. so a `build/` directory
// doesn't block removing a folder that was deleted elsewhere). (?p) marks a
// pattern as protected, opting it back out of that default -- useful for
// ignored-but-precious local content like secrets or machine-local config.
// (?d) is accepted as a harmless no-op for old ignore files written when
// deletable was opt-in.
func transformLine(line string) (shadowLine string, flags lineFlags) {
	body := line
	negated := false
	var seenNeg, seenFold, seenLegacyDel, seenProtect bool
loop:
	for {
		switch {
		case strings.HasPrefix(body, "!") && !seenNeg:
			seenNeg, negated = true, true
			body = body[1:]
		case strings.HasPrefix(body, "(?i)") && !seenFold:
			seenFold = true
			flags.caseFold = true
			body = body[4:]
		case strings.HasPrefix(body, "(?d)") && !seenLegacyDel:
			seenLegacyDel = true
			body = body[4:]
		case strings.HasPrefix(body, "(?p)") && !seenProtect:
			seenProtect = true
			flags.protected = true
			body = body[4:]
		default:
			break loop
		}
	}
	if flags.caseFold {
		body = asciiCaseInsensitiveExpand(body)
	}
	if negated {
		body = "!" + body
	}
	return body, flags
}

// asciiCaseInsensitiveExpand bracket-expands each ASCII letter outside of
// an existing [...] character class or a backslash escape (e.g. "abc" ->
// "[aA][bB][cC]"), giving git a pattern that matches case-insensitively
// without needing a per-pattern flag, which check-ignore has no way to
// accept. This is intentionally ASCII-only -- a narrower guarantee than the
// old matcher's full-Unicode case fold, accepted as reasonable given (?i)
// is almost always used for things like *.TMP or Thumbs.db.
func asciiCaseInsensitiveExpand(pattern string) string {
	var b strings.Builder
	inClass := false
	escaped := false
	for _, r := range pattern {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			b.WriteRune(r)
			escaped = true
		case r == '[':
			inClass = true
			b.WriteRune(r)
		case r == ']':
			inClass = false
			b.WriteRune(r)
		case inClass:
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			fmt.Fprintf(&b, "[%c%c]", r, r-32)
		case r >= 'A' && r <= 'Z':
			fmt.Fprintf(&b, "[%c%c]", r+32, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rebuildShadowTree wipes and rewrites the shadow tree at root from the
// discovered set. A full wipe-and-rewrite (rather than diffing against the
// previous generation) is simpler and cheap: shadow trees only ever contain
// small transformed .gitignore copies, never real folder content, and this
// only runs when hashDiscovered has already told the caller something
// changed.
func rebuildShadowTree(root string, discovered map[string]discoveredIgnore) (flagsTable map[string][]lineFlags, rootLines []string, err error) {
	if err := os.RemoveAll(root); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, nil, err
	}

	flagsTable = make(map[string][]lineFlags, len(discovered))

	for relPath, gi := range discovered {
		shadowLines := make([]string, len(gi.rawLines))
		flags := make([]lineFlags, len(gi.rawLines)+1) // 1-indexed, [0] unused
		for i, raw := range gi.rawLines {
			sl, lf := transformLine(raw)
			shadowLines[i] = sl
			flags[i+1] = lf
		}
		flagsTable[relPath] = flags

		shadowPath := filepath.Join(root, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(shadowPath), 0o700); err != nil {
			return nil, nil, err
		}
		content := strings.Join(shadowLines, "\n") + "\n"
		if err := os.WriteFile(shadowPath, []byte(content), 0o600); err != nil {
			return nil, nil, err
		}

		if relPath == gitignoreName {
			rootLines = gi.rawLines
		}
	}

	return flagsTable, rootLines, nil
}
