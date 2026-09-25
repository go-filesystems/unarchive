# unarchive

Open an archive without being told what it is, and extract it while checking
that what came out is what the archive promised.

Pure Go, `CGO_ENABLED=0`, and it builds for every target Go builds for.

```
go install github.com/go-filesystems/unarchive/cmd/unarchive@latest

unarchive film.part1.rar          # into ./film/
unarchive -C /tmp/out backup.tar.zst
unarchive -f already-there.zip    # overwrite
unarchive -n suspicious.7z        # list, extract nothing
```

## Formats

| | read | notes |
|---|---|---|
| RAR 1.5–4.x, RAR5 | ✅ | multi-volume sets followed by volume **number** |
| ZIP | ✅ | including the jar/epub/odf family |
| 7z | ✅ | follows its own `.001` chain, by name |
| tar | ✅ | v7, USTAR, PAX and GNU alike, with real random access |
| gzip, bzip2, xz, zstd, lz4 | ✅ | stream wrappers: `.tar.gz`, `.tgz`, `.tar.zst`, a lone `notes.txt.gz` … |

**brotli is absent on purpose.** It has no signature: a brotli stream begins
with the first bits of its own data, so there is nothing to recognise it *by*.
The only way to open one is to be told, by an extension or a flag, and this
package decides from the bytes.

## Why the bytes and not the name

A format is recognised by what is **in** the file. An extension is a claim by
whoever last renamed it: a `.rar` that is really a zip, a video saved as `.mp4`
that is an MPEG transport stream, a volume renamed by a download manager — all
ordinary, and all wrong if the name decides.

The one thing a name is used for is what to **call** the single file inside a
wrapper: somebody who gunzips `notes.txt.gz` expects `notes.txt`, and nothing in
the bytes says so.

## Why the check

An extractor that reports success is not the same as an extractor that got
everything.

libarchive — which is `bsdtar`, and therefore macOS — does not follow a
multi-volume RAR chain. It writes the first volume, pads the remainder with
**zeros** to the declared length, and exits 0. The result is exactly the right
*size*, which is what almost every check afterwards looks at. A 1.26 GiB file
came out of it here, verified by size, believed, and the source archives were
deleted on the strength of it.

So this package compares what was written against what the header declared, per
entry, and says so when they differ (`ErrIncomplete`).

## Three kinds of no

| | means |
|---|---|
| `ErrUnknownFormat` | I do not know what this is |
| `ErrNotImplemented` | I know exactly what this is and do not read it yet |
| anything else | I read this format and **this file** is broken |

They send a person to three different places, which is why they are three
errors. Nothing is in the middle bucket today; a count in the tests fails if a
format is ever added to the sniffer alone, so that stays a decision rather than
a default.

## As a filesystem

`Open` returns a `filesystem.Filesystem` from
[go-filesystems/interface](https://github.com/go-filesystems/interface), so an
archive can be read like a mounted volume — `ReadFile`, `ListDir`, `Stat`, and
`OpenFile` for a seekable handle, without extracting anything.

```go
fsys, format, err := unarchive.Open("backup.tar.xz")
if err != nil { return err }
defer fsys.Close()          // removes the spool, for a compressed wrapper

b, err := fsys.ReadFile("etc/hosts")
```

A stream wrapper is spooled to a temporary file first, and that is the format's
doing rather than a shortcut: every archive here needs random access — a tar
index, a zip central directory, a 7z header at the end — and a decompressor
gives a one-way stream. Reading it twice means decompressing it twice; holding
it in memory means holding a whole archive in memory. `Close` removes the spool.

Nesting is capped: a wrapper holds one stream and that stream may be another
wrapper, so a few hundred bytes of `.gz` nested on itself would otherwise ask
for however much spool the machine has.

## Licence

BSD-3-Clause.
