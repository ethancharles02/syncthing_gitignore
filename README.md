[![Syncthing][14]][15]

---

[![MPLv2 License](https://img.shields.io/badge/license-MPLv2-blue.svg?style=flat-square)](https://www.mozilla.org/MPL/2.0/)
[![CII Best Practices](https://bestpractices.coreinfrastructure.org/projects/88/badge)](https://bestpractices.coreinfrastructure.org/projects/88)
[![Go Report Card](https://goreportcard.com/badge/github.com/syncthing/syncthing)](https://goreportcard.com/report/github.com/syncthing/syncthing)

## This fork: real `.gitignore` support

This is a personal fork of Syncthing that replaces the built-in `.stignore`
ignore mechanism with real, nested `.gitignore` semantics, matched by
shelling out to an actual `git` binary rather than a hand-rolled matcher.
Everything below the "Goals" heading is unmodified upstream documentation;
this section covers what's different here.

**Requirements**: `git` >= 2.30 must be installed and on `PATH` on every
machine running this build. Syncthing checks this at startup and refuses to
start (with a clear error, logged and non-zero exit) if `git` is missing or
too old — it will not silently fall back to old behavior.

**What changed from stock Syncthing:**

- Ignore files are named `.gitignore`, not `.stignore`, and any directory in
  a synced folder can have its own — not just one file at the folder root.
  A `.gitignore` inside a real git-cloned project now syncs and applies
  normally, the same as any other file in that project.
- `.gitignore` is **normal, visible, synced content** — unlike the old
  `.stignore`, it is not hidden from peers or from your own file listing.
  This also means ignore rules are shared across your devices for a given
  folder, not set per-device.
- Comments use real gitignore syntax (`#`), not the old `//`.
- `#include` and `#escape=` (both Syncthing-only extensions with no
  gitignore equivalent) are gone. Any line starting with `#` is just a
  normal comment now, so old `#include`/`#escape=` lines become harmless
  no-ops rather than errors — but the content they used to pull in or the
  custom escape behavior they configured won't apply anymore. If you're
  migrating an existing `.stignore`, inline any `#include`d content
  manually first.
- `(?i)` (case-insensitive) and `(?d)` (deletable, used by receive-only
  folders) still work as line prefixes, layered on top of real gitignore
  syntax.
- **Real git semantics, including one genuinely surprising behavior change**:
  a `!negation` pattern cannot un-ignore something inside a directory that's
  already excluded by an earlier pattern, unless that directory itself is
  separately un-ignored. Concretely, this no longer works the way it used
  to:
  ```
  *
  !/foo/bar
  ```
  `foo` itself still matches the bare `*` and is excluded, so `foo/bar`
  stays excluded too. The real-gitignore-correct way to write "ignore
  everything except `foo/bar`" is to un-ignore every intermediate directory
  explicitly:
  ```
  *
  !/foo/
  !/foo/bar/
  !/foo/bar/**
  ```
  This is standard, documented real-`git` behavior, not a bug — but if
  you're bringing over `.stignore` patterns that relied on the old, more
  lenient matcher, check for this pattern and rewrite it.

## Goals

Syncthing is a **continuous file synchronization program**. It synchronizes
files between two or more computers. We strive to fulfill the goals below.
The goals are listed in order of importance, the most important ones first.
This is the summary version of the goal list - for more
commentary, see the full [Goals document][13].

Syncthing should be:

1. **Safe From Data Loss**

   Protecting the user's data is paramount. We take every reasonable
   precaution to avoid corrupting the user's files.

2. **Secure Against Attackers**

   Again, protecting the user's data is paramount. Regardless of our other
   goals, we must never allow the user's data to be susceptible to
   eavesdropping or modification by unauthorized parties.

3. **Easy to Use**

   Syncthing should be approachable, understandable, and inclusive.

4. **Automatic**

   User interaction should be required only when absolutely necessary.

5. **Universally Available**

   Syncthing should run on every common computer. We are mindful that the
   latest technology is not always available to every individual.

6. **For Individuals**

   Syncthing is primarily about empowering the individual user with safe,
   secure, and easy to use file synchronization.

7. **Everything Else**

   There are many things we care about that don't make it on to the list. It
   is fine to optimize for these values, as long as they are not in conflict
   with the stated goals above.

## Getting Started

Take a look at the [getting started guide][2].

There are a few examples for keeping Syncthing running in the background
on your system in [the etc directory][3]. There are also several [GUI
implementations][11] for Windows, Mac, and Linux.

## Docker

To run Syncthing in Docker, see [the Docker README][16].

## Getting in Touch

The first and best point of contact is the [Forum][8].
If you've found something that is clearly a
bug, feel free to report it in the [GitHub issue tracker][10].

If you believe that you’ve found a Syncthing-related security vulnerability,
please report it by emailing security@syncthing.net. Do not report it in the
Forum or issue tracker.

## Building

Building Syncthing from source is easy. After extracting the source bundle from
a release or checking out git, you just need to run `go run build.go` and the
binaries are created in `./bin`. There's [a guide][5] with more details on the
build process.

## Signed Releases

Release binaries are GPG signed with the key available from
https://syncthing.net/security/. There is also a built-in automatic
upgrade mechanism (disabled in some distribution channels) which uses a
compiled in ECDSA signature. macOS and Windows binaries are also
code-signed.

## Documentation

Please see the Syncthing [documentation site][6] [[source]][17].

All code is licensed under the [MPLv2 License][7].

[1]: https://docs.syncthing.net/specs/bep-v1.html
[2]: https://docs.syncthing.net/intro/getting-started.html
[3]: https://github.com/syncthing/syncthing/blob/main/etc
[5]: https://docs.syncthing.net/dev/building.html
[6]: https://docs.syncthing.net/
[7]: https://github.com/syncthing/syncthing/blob/main/LICENSE
[8]: https://forum.syncthing.net/
[10]: https://github.com/syncthing/syncthing/issues
[11]: https://docs.syncthing.net/users/contrib.html#gui-wrappers
[13]: https://github.com/syncthing/syncthing/blob/main/GOALS.md
[14]: assets/logo-text-128.png
[15]: https://syncthing.net/
[16]: https://github.com/syncthing/syncthing/blob/main/README-Docker.md
[17]: https://github.com/syncthing/docs
