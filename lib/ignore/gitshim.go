// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package ignore

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/syncthing/syncthing/lib/build"
)

// checkIgnoreProcess wraps a long-lived `git check-ignore --stdin -z -v
// --non-matching` subprocess against a shadow tree (see shadow.go). Queries
// are streamed one at a time and answered in order without needing to close
// stdin between them, so a single process serves an entire Load()
// generation instead of spawning per file or per directory.
type checkIgnoreProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

type checkIgnoreResult struct {
	matched bool
	source  string // shadow-tree-relative .gitignore path that matched, if any
	line    int    // 1-based line number within that file, if matched
}

func platformDefaultIgnoreCase() bool {
	return build.IsWindows || build.IsDarwin || build.IsIOS
}

// gitEnv deliberately excludes the host's ambient HOME/gitconfig/aliases:
// results should depend only on the shadow tree, not on whatever happens
// to be configured on the machine running Syncthing.
func gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
	}
}

func newCheckIgnoreProcess(gitPath, anchorDir, workDir string) (*checkIgnoreProcess, error) {
	cmd := exec.Command(gitPath,
		"-c", "core.excludesFile=",
		"-c", fmt.Sprintf("core.ignorecase=%v", platformDefaultIgnoreCase()),
		"-C", workDir,
		"--git-dir", anchorGitDir(anchorDir),
		"check-ignore", "--no-index", "--stdin", "-z", "-v", "--non-matching",
	)
	cmd.Env = gitEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &checkIgnoreProcess{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 4096)}, nil
}

// query asks whether candidate (forward-slashed, relative to the shadow
// tree root) is ignored. isDir must reflect whether candidate is actually a
// directory: the shadow tree only ever contains .gitignore files, never the
// real content, so git can't stat its way to an answer for directory-only
// patterns (e.g. "build/") -- the trailing slash on the query is the only
// signal it has.
func (p *checkIgnoreProcess) query(candidate string, isDir bool) (checkIgnoreResult, error) {
	q := candidate
	if isDir && !strings.HasSuffix(q, "/") {
		q += "/"
	}
	if _, err := io.WriteString(p.stdin, q+"\x00"); err != nil {
		return checkIgnoreResult{}, err
	}
	fields, err := p.readRecord()
	if err != nil {
		return checkIgnoreResult{}, err
	}
	// A non-empty source means git found a *governing* pattern for this
	// path -- but with -v that includes negation patterns, which un-ignore
	// rather than ignore. Verified empirically: "!keep.log" is reported
	// here exactly like a real match would be, just with a "!"-prefixed
	// pattern string; the true verdict has to be read off that prefix, not
	// off whether a source was reported at all.
	if fields[0] == "" || strings.HasPrefix(fields[2], "!") {
		return checkIgnoreResult{matched: false}, nil
	}
	line, _ := strconv.Atoi(fields[1])
	return checkIgnoreResult{matched: true, source: fields[0], line: line}, nil
}

// readRecord reads one 4-field NUL-delimited record: source, linenum,
// pattern, pathname. Verified empirically against git 2.55 -- for a
// non-match (with --non-matching set) the first three fields come back
// empty and only pathname is populated.
func (p *checkIgnoreProcess) readRecord() ([4]string, error) {
	var fields [4]string
	for i := range fields {
		s, err := p.stdout.ReadString(0)
		if err != nil {
			return fields, err
		}
		fields[i] = strings.TrimSuffix(s, "\x00")
	}
	return fields, nil
}

func (p *checkIgnoreProcess) Close() {
	p.stdin.Close()
	_ = p.cmd.Wait()
}

func anchorGitDir(anchor string) string {
	return anchor + string(os.PathSeparator) + ".git"
}
