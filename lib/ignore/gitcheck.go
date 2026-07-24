// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package ignore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"

	"github.com/syncthing/syncthing/lib/locations"
)

const (
	minGitMajor = 2
	minGitMinor = 30
)

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

var (
	gitCheckOnce     sync.Once
	gitCheckErr      error
	checkedGitPath   string
	checkedAnchorDir string
)

// CheckGitAvailable verifies that a usable git binary is on PATH, that it's
// new enough, and that the anchor repository used for --no-index ignore
// matching can be created and actually works end to end. It's meant to be
// called once, early at process startup, so a missing or broken git
// installation fails loudly and immediately instead of surfacing later as a
// confusing per-folder error. Safe to call repeatedly (and from Matcher.Load
// as a defensive fallback) -- only the first call does real work.
func CheckGitAvailable() error {
	gitCheckOnce.Do(func() {
		checkedGitPath, checkedAnchorDir, gitCheckErr = checkGitAvailable()
	})
	return gitCheckErr
}

// gitPathAndAnchor returns the resolved git binary path and anchor
// repository directory, running CheckGitAvailable if it hasn't already run.
func gitPathAndAnchor() (gitPath, anchorDir string, err error) {
	if err := CheckGitAvailable(); err != nil {
		return "", "", err
	}
	return checkedGitPath, checkedAnchorDir, nil
}

func checkGitAvailable() (gitPath, anchorDir string, err error) {
	gitPath, err = exec.LookPath("git")
	if err != nil {
		return "", "", fmt.Errorf("git executable not found on PATH: gitignore-based ignore matching requires git to be installed: %w", err)
	}

	if err := checkGitVersion(gitPath); err != nil {
		return "", "", err
	}

	anchorDir = locations.Get(locations.GitShimAnchor)
	if err := ensureAnchorRepo(gitPath, anchorDir); err != nil {
		return "", "", fmt.Errorf("failed to set up git ignore-matching anchor repository at %s: %w", anchorDir, err)
	}

	if err := smokeTestCheckIgnore(gitPath, anchorDir); err != nil {
		return "", "", fmt.Errorf("git check-ignore smoke test failed: %w", err)
	}

	return gitPath, anchorDir, nil
}

func checkGitVersion(gitPath string) error {
	out, err := exec.Command(gitPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("failed to run %q --version: %w", gitPath, err)
	}
	m := gitVersionRe.FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("could not parse git version from %q", string(out))
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	if major < minGitMajor || (major == minGitMajor && minor < minGitMinor) {
		return fmt.Errorf("git version %d.%d is too old for gitignore-based ignore matching (need >= %d.%d)", major, minor, minGitMajor, minGitMinor)
	}
	return nil
}

func ensureAnchorRepo(gitPath, anchorDir string) error {
	if _, err := os.Stat(filepath.Join(anchorDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(anchorDir, 0o700); err != nil {
		return err
	}
	cmd := exec.Command(gitPath, "init", "-q", anchorDir)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func smokeTestCheckIgnore(gitPath, anchorDir string) error {
	scratch, err := os.MkdirTemp("", "syncthing-gitshim-smoketest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	if err := os.WriteFile(filepath.Join(scratch, gitignoreName), []byte("*.smoketest\n"), 0o600); err != nil {
		return err
	}

	proc, err := newCheckIgnoreProcess(gitPath, anchorDir, scratch)
	if err != nil {
		return err
	}
	defer proc.Close()

	matched, err := proc.query("file.smoketest", false)
	if err != nil {
		return err
	}
	if !matched.matched {
		return errors.New("expected smoke-test pattern to match, it did not")
	}

	notMatched, err := proc.query("file.keep", false)
	if err != nil {
		return err
	}
	if notMatched.matched {
		return errors.New("expected smoke-test non-matching path to not match, it did")
	}

	return nil
}
