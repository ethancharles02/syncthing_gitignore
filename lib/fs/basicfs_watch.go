// Copyright (C) 2016 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build !(solaris && !cgo) && !(darwin && !cgo) && !(darwin && kqueue)
// +build !solaris cgo
// +build !darwin cgo
// +build !darwin !kqueue

package fs

import (
	"context"
	"errors"
	"os"
	"unicode/utf8"

	"github.com/syncthing/notify"
)

// Notify does not block on sending to channel, so the channel must be buffered.
// The actual number is magic.
// Not meant to be changed, but must be changeable for tests
var backendBuffer = 500

func (f *BasicFilesystem) Watch(name string, ignore Matcher, ctx context.Context, ignorePerms bool) (<-chan Event, <-chan error, error) {
	watchPath, roots, err := f.watchPaths(name)
	if err != nil {
		return nil, nil, err
	}

	outChan := make(chan Event)
	backendChan := make(chan notify.EventInfo, backendBuffer)

	eventMask := subEventMask
	if !ignorePerms {
		eventMask |= permEventMask
	}

	absShouldIgnore := func(absPath string) bool {
		if !utf8.ValidString(absPath) {
			return true
		}

		rel, err := f.unrootedChecked(absPath, roots)
		if err != nil {
			return true
		}
		// Best-effort: the ignore matcher needs to know if this is a
		// directory to honor directory-only patterns correctly. If the
		// path is already gone, default to false -- it's the common case
		// and a wrong guess here only affects watch setup, not matching
		// during an actual scan.
		isDir := false
		if info, statErr := os.Lstat(absPath); statErr == nil {
			isDir = info.IsDir()
		}
		return ignore.Match(rel, isDir).CanSkipDir()
	}
	err = notify.WatchWithFilter(watchPath, backendChan, absShouldIgnore, eventMask)
	if err != nil {
		notify.Stop(backendChan)
		if reachedMaxUserWatches(err) {
			err = errors.New("failed to set up inotify handler. Please increase inotify limits, see https://docs.syncthing.net/users/faq.html#inotify-limits")
		}
		return nil, nil, err
	}

	errChan := make(chan error)
	go f.watchLoop(ctx, name, roots, backendChan, outChan, errChan, ignore)

	return outChan, errChan, nil
}

func (f *BasicFilesystem) watchLoop(ctx context.Context, name string, roots []string, backendChan chan notify.EventInfo, outChan chan<- Event, errChan chan<- error, ignore Matcher) {
	for {
		// Detect channel overflow
		if len(backendChan) == backendBuffer {
		outer:
			for {
				select {
				case <-backendChan:
				default:
					break outer
				}
			}
			// When next scheduling a scan, do it on the entire folder as events have been lost.
			outChan <- Event{Name: name, Type: NonRemove}
			l.Debugln(f.Type(), f.URI(), "Watch: Event overflow, send \".\"")
		}

		select {
		case ev := <-backendChan:
			evPath := ev.Path()

			if !utf8.ValidString(evPath) {
				l.Debugln(f.Type(), f.URI(), "Watch: Ignoring invalid UTF-8")
				continue
			}

			relPath, err := f.unrootedChecked(evPath, roots)
			if err != nil {
				select {
				case errChan <- err:
					l.Debugln(f.Type(), f.URI(), "Watch: Sending error", err)
				case <-ctx.Done():
				}
				notify.Stop(backendChan)
				l.Debugln(f.Type(), f.URI(), "Watch: Stopped due to", err)
				return
			}

			// Same best-effort isDir determination as absShouldIgnore above.
			isDir := false
			if info, statErr := os.Lstat(evPath); statErr == nil {
				isDir = info.IsDir()
			}
			if ignore.Match(relPath, isDir).IsIgnored() {
				l.Debugln(f.Type(), f.URI(), "Watch: Ignoring", relPath)
				continue
			}
			evType := f.eventType(ev.Event())
			select {
			case outChan <- Event{Name: relPath, Type: evType}:
				l.Debugln(f.Type(), f.URI(), "Watch: Sending", relPath, evType)
			case <-ctx.Done():
				notify.Stop(backendChan)
				l.Debugln(f.Type(), f.URI(), "Watch: Stopped")
				return
			}
		case <-ctx.Done():
			notify.Stop(backendChan)
			l.Debugln(f.Type(), f.URI(), "Watch: Stopped")
			return
		}
	}
}

func (*BasicFilesystem) eventType(notifyType notify.Event) EventType {
	if notifyType&rmEventMask != 0 {
		return Remove
	}
	return NonRemove
}
