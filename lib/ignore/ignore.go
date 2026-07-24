// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package ignore implements .gitignore-based ignore matching for a folder.
// Matching is delegated to a real `git check-ignore` subprocess run against
// a "shadow tree" of transformed copies of every real .gitignore found in
// the folder (see shadow.go and gitshim.go) -- this gives genuine, nested,
// per-directory git semantics rather than a hand-rolled approximation.
package ignore

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore/ignoreresult"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/osutil"
)

// A ParseError signifies an error with the contents of an ignore file.
type ParseError struct {
	inner error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse error: %v", e.inner)
}

func (e *ParseError) Unwrap() error {
	return e.inner
}

func IsParseError(err error) bool {
	var e *ParseError
	return errors.As(err, &e)
}

type Matcher struct {
	ffs        fs.Filesystem
	shadowRoot string

	withCache bool
	matches   *cache
	stop      chan struct{}
	mut       sync.Mutex

	discovered map[string]discoveredIgnore
	flagsTable map[string][]lineFlags
	rootLines  []string
	curHash    string
	proc       *checkIgnoreProcess
}

// An Option can be passed to New()
type Option func(*Matcher)

// WithCache enables or disables lookup caching. The default is disabled.
func WithCache(v bool) Option {
	return func(m *Matcher) {
		m.withCache = v
	}
}

func New(ffs fs.Filesystem, opts ...Option) *Matcher {
	m := &Matcher{
		ffs:  ffs,
		stop: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.withCache {
		go m.clean(2 * time.Hour)
	}
	return m
}

// shadowRootLocked computes and caches the shadow tree path on first use.
// Deliberately lazy, not done in New(): some callers construct a Matcher
// with a nil/placeholder filesystem and never call Load/Match on it (e.g.
// as a zero-value struct field), which must stay harmless exactly like it
// was before this filesystem-backed rewrite.
func (m *Matcher) shadowRootLocked() string {
	if m.shadowRoot == "" {
		m.shadowRoot = shadowRootFor(m.ffs)
	}
	return m.shadowRoot
}

func shadowRootFor(ffs fs.Filesystem) string {
	sum := sha256.Sum256([]byte(ffs.URI()))
	return filepath.Join(locations.Get(locations.GitShimShadow), hex.EncodeToString(sum[:])[:16])
}

// Load re-discovers every .gitignore in the folder and, if anything
// changed since the last Load, rebuilds the shadow tree and restarts the
// matching subprocess against it.
func (m *Matcher) Load() error {
	discovered, err := discoverGitignores(m.ffs)
	if err != nil {
		return err
	}

	m.mut.Lock()
	defer m.mut.Unlock()
	return m.applyDiscoveredLocked(discovered)
}

// Parse loads ignore patterns from an arbitrary reader as if it were the
// folder's root .gitignore. It does not discover nested files -- primarily
// useful in tests that want to set up patterns without touching disk.
func (m *Matcher) Parse(r io.Reader, _ string) error {
	lines, err := readLines(r)
	if err != nil {
		return err
	}

	m.mut.Lock()
	defer m.mut.Unlock()
	discovered := map[string]discoveredIgnore{
		gitignoreName: {relPath: gitignoreName, rawLines: lines},
	}
	return m.applyDiscoveredLocked(discovered)
}

func (m *Matcher) applyDiscoveredLocked(discovered map[string]discoveredIgnore) error {
	newHash := hashDiscovered(discovered)
	if newHash == m.curHash && m.proc != nil {
		return nil
	}

	gitPath, anchorDir, err := gitPathAndAnchor()
	if err != nil {
		return err
	}

	// The old subprocess must be closed before the shadow tree is rebuilt:
	// on Windows, a running git process holding the shadow directory open
	// (as its working directory / open file handles) blocks deletion of
	// files inside it, turning the os.RemoveAll below into an intermittent
	// "used by another process" error.
	if m.proc != nil {
		m.proc.Close()
		m.proc = nil
	}

	shadowRoot := m.shadowRootLocked()
	flagsTable, rootLines, err := rebuildShadowTree(shadowRoot, discovered)
	if err != nil {
		return err
	}

	proc, err := newCheckIgnoreProcess(gitPath, anchorDir, shadowRoot)
	if err != nil {
		return err
	}

	m.discovered = discovered
	m.flagsTable = flagsTable
	m.rootLines = rootLines
	m.curHash = newHash
	m.proc = proc
	if m.withCache {
		m.matches = newCache()
	}

	return nil
}

// Match matches file against the loaded ignore patterns. isDir must
// reflect whether file is actually a directory -- directory-only patterns
// (e.g. "build/") can only be honored if the caller says so, since the
// shadow tree behind this doesn't mirror real folder content for git to
// stat.
//
// The "file" parameter must be in the OS' native unicode format (NFD on
// macOS, NFC everywhere else), matching real usage in Syncthing where all
// entry points (scanning and protocol) already ensure this.
func (m *Matcher) Match(file string, isDir bool) (result ignoreresult.R) {
	switch {
	case fs.IsTemporary(file):
		return ignoreresult.IgnoreAndSkip
	case fs.IsInternal(file):
		return ignoreresult.IgnoreAndSkip
	case file == ".":
		return ignoreresult.NotIgnored
	}

	file = filepath.ToSlash(file)

	m.mut.Lock()
	defer m.mut.Unlock()

	if m.proc == nil {
		return ignoreresult.NotIgnored
	}

	cacheKey := file
	if isDir {
		cacheKey += "/"
	}
	if m.matches != nil {
		if res, ok := m.matches.get(cacheKey); ok {
			return res
		}
		defer func() {
			m.matches.set(cacheKey, result)
		}()
	}

	res, err := m.proc.query(file, isDir)
	if err != nil {
		// The persistent subprocess died or the pipe broke. Try once to
		// respawn against the current shadow tree rather than silently
		// failing open to NotIgnored -- if the respawn also fails, the
		// folder's health-error path is expected to surface it on the next
		// Load().
		if restartErr := m.restartProcLocked(); restartErr == nil {
			res, err = m.proc.query(file, isDir)
		}
		if err != nil {
			return ignoreresult.NotIgnored
		}
	}

	if !res.matched {
		return ignoreresult.NotIgnored
	}

	var lf lineFlags
	if flags, ok := m.flagsTable[res.source]; ok && res.line > 0 && res.line < len(flags) {
		lf = flags[res.line]
	}

	out := ignoreresult.Ignored
	if lf.deletable {
		out = out.WithDeletable()
	}
	if lf.caseFold {
		out = out.WithFoldCase()
	}
	if isDir {
		// Real git's rule "a file inside an excluded directory cannot be
		// re-included unless the directory itself is un-excluded" means
		// that once a directory query comes back ignored, it is always
		// safe to skip descending -- there is no negation pattern deeper
		// in the tree that could matter, because git itself would never
		// look for one either.
		out = out.WithSkipDir()
	}
	return out
}

func (m *Matcher) restartProcLocked() error {
	gitPath, anchorDir, err := gitPathAndAnchor()
	if err != nil {
		return err
	}
	proc, err := newCheckIgnoreProcess(gitPath, anchorDir, m.shadowRootLocked())
	if err != nil {
		return err
	}
	if m.proc != nil {
		m.proc.Close()
	}
	m.proc = proc
	return nil
}

// Lines returns the unprocessed lines of the folder's root .gitignore at
// last load. Nested .gitignore files elsewhere in the tree are matched
// against but not exposed here -- edit those directly with normal file
// tools, the same as any real git user would.
func (m *Matcher) Lines() []string {
	m.mut.Lock()
	defer m.mut.Unlock()
	return m.rootLines
}

// Patterns returns the non-blank, non-comment lines of the folder's root
// .gitignore.
func (m *Matcher) Patterns() []string {
	m.mut.Lock()
	defer m.mut.Unlock()
	patterns := make([]string, 0, len(m.rootLines))
	for _, line := range m.rootLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		patterns = append(patterns, trimmed)
	}
	return patterns
}

func (m *Matcher) String() string {
	return fmt.Sprintf("Matcher/%v@%p", m.Patterns(), m)
}

func (m *Matcher) Hash() string {
	m.mut.Lock()
	defer m.mut.Unlock()
	return m.curHash
}

func (m *Matcher) Stop() {
	close(m.stop)
	m.mut.Lock()
	if m.proc != nil {
		m.proc.Close()
	}
	m.mut.Unlock()
}

func (m *Matcher) clean(d time.Duration) {
	t := time.NewTimer(d / 2)
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.mut.Lock()
			if m.matches != nil {
				m.matches.clean(d)
			}
			t.Reset(d / 2)
			m.mut.Unlock()
		}
	}
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSpace(scanner.Text()))
	}
	return lines, scanner.Err()
}

// WriteIgnores is a convenience function to avoid code duplication. Unlike
// the old .stignore, a .gitignore is normal tracked content -- real git
// doesn't hide it either -- so it's written as a plain file with no
// filesystem.Hide call.
func WriteIgnores(filesystem fs.Filesystem, path string, content []string) error {
	if len(content) == 0 {
		err := filesystem.Remove(path)
		if fs.IsNotExist(err) {
			return nil
		}
		return err
	}

	fd, err := osutil.CreateAtomicFilesystem(filesystem, path)
	if err != nil {
		return err
	}

	wr := osutil.LineEndingsWriter(fd)
	for _, line := range content {
		fmt.Fprintln(wr, line)
	}

	return fd.Close()
}
