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

unarchive -o film.7z film.rar     # convert, extracting nothing
unarchive -o backup.tar.zst backup.zip
unarchive -o disc.7z disc.iso     # yes, that works
```

## Formats

| | read | notes |
|---|---|---|
| RAR 1.5–4.x, RAR5 | ✅ | multi-volume sets followed by volume **number** |
| ZIP | ✅ | including the jar/epub/odf family, and entries compressed with **bzip2, LZMA, xz or zstd** — not just deflate |
| 7z | ✅ | follows its own `.001` chain, by name |
| tar | ✅ | v7, USTAR, PAX and GNU alike, with real random access |
| ar | ✅ | static libraries **and `.deb`** — both long-name spellings, SysV and BSD |
| cpio | ✅ | `newc`, `crc`, `odc` **and the old binary variant, both byte orders** |
| gzip, bzip2, xz, zstd, lz4 | ✅ | stream wrappers: `.tar.gz`, `.tgz`, `.tar.zst`, a lone `notes.txt.gz` … |
| compress `.Z` | read only | the LZW `compress/lzw` cannot read — see below |
| ISO 9660 | ✅ | a disc image is an archive too — read through `go-filesystems/iso9660` |
| SquashFS | ✅ | likewise, through `go-filesystems/squashfs` |
| HFS+ | ✅ | what a `.dmg` held before APFS — through `go-filesystems/hfsplus` |
| DMG | ✅ | a UDIF *container* around one of the above: peeled, then sniffed |
| plakar `.ptar` | recognised, not unpacked | and the reason is below |

A **numbered split** — `film.zip.001`, `.002`, … — is read as one file, whatever
the format inside it, so `unarchive film.zip.001` works and extracts into `film/`.
RAR is the exception: its own volume sets are followed by number already, and a
plain numbered split of a RAR is a different thing this has no entry point for.

### Written

`.tar`, `.tar.gz`, `.tgz`, `.tar.xz`, `.txz`, `.tar.zst`, `.tzst`, `.tar.lz4`,
`.tar.bz2`, `.tbz2`, `.tbz`, `.zip`, `.jar`, `.7z`, `.a`, `.deb`, `.cpio`, `.iso`.

`ar` is written in the **BSD** convention, not SysV — measured, not chosen: written
SysV, `ar t` on macOS listed every member with its slash attached (`notes.txt/`)
and `ar p archive notes.txt` then found nothing. BSD also needs no string table,
which is why it streams: SysV puts long names in a `//` member that must *precede*
every entry.

`cpio` is written as **newc**, whose eight hex digits reach 4 GiB, rather than the
octal `odc` whose six do not reach 8 MiB.

⚠ An **`.iso`** holds every entry in memory while it is built. That is upstream's
shape rather than a choice here: `iso9660.Builder.AddFile` takes a `[]byte`. And
ISO 9660 without Rock Ridge or Joliet upper-cases names and cannot carry a long
one — the builder writes neither extension.

Not written: **RAR**, because no free writer exists, and **`.Z`**, because a new
`.Z` is a file nobody should be making — `gzip`, `xz` and `zstd` all compress
better and are read everywhere. Both say which and why:

```
$ unarchive -o out.tar.Z film.tar.Z
unarchive: out.tar.Z: unarchive: this format is read but not written: the LZW of
compress(1). A new .Z is a file nobody should be making: gzip, xz and zstd all
compress better and are read everywhere
```

`.Z` is read through
[go-compressions/compress](https://github.com/go-compressions/compress), written
for this: `compress/lzw` stops at 12 bits, has no clear code, and knows nothing of
the group alignment, so it cannot read a `.Z` at all.

bzip2 used to be on it, for a different kind of reason — the standard library
decompresses bzip2 and nothing compressed it. A missing *library* is not a
property of a format, so the library was written
([go-compressions/bzip2](https://github.com/go-compressions/bzip2)) and the
sentence stopped being true. The two reasons look identical from the outside and
only one of them is permanent.

**brotli is absent on purpose.** It has no signature: a brotli stream begins
with the first bits of its own data, so there is nothing to recognise it *by*.
The only way to open one is to be told, by an extension or a flag, and this
package decides from the bytes.

### plakar `.ptar`: recognised, and deliberately not unpacked

A `.ptar` is not an archive. It is a **Kloset repository in a file** — a config
blob, a packfile region and a state region, addressed by MAC — holding snapshots
and deduplicated chunks, and **encrypted** unless it was made with `-plaintext`.

Reading one means implementing their storage container, their state index, their
packfile format, their snapshot model, and decryption with a key the caller must
supply. That is a backup tool, not an unarchiver, and *"which snapshot?"* is a
question this command has nowhere to ask. So it is recognised and says what it
is:

```
$ unarchive backup.ptar
unarchive: backup.ptar: unarchive: recognised, but this format is not read yet:
ptar is a plakar Kloset archive: a content-addressed repository of snapshots and
deduplicated chunks, encrypted unless it was made with -plaintext. Open it with
plakar, which has the key
```

Recognition earns its keep on its own: without it, a `.ptar` answered *"the bytes
match no format this knows"*, which sends somebody hunting for a corrupt file.

The magic is `_PLATAR_`, read out of PlakarKorp's own storage driver. Guessing it
from the extension — the thing this package exists not to do — would have got it
wrong.

## An image is an archive too

A filesystem image is one file holding a tree, handed around to be unpacked —
which is what an archive is, from the outside. So `.iso` and `.squashfs` open,
list, extract and convert like any other input, and **nothing here decodes
them**: `go-filesystems` already owns those drivers, and the detection is
`go-filesystems/detect`'s hardened prober rather than a second magic table
written here.

The line is drawn at formats people **distribute**. You download an `.iso`, you
ship a `.squashfs`, somebody sends you a `.dmg`; you do not hand somebody an ext4
image expecting them to unpack it. The org has drivers for a dozen more
filesystems and this opens three, on purpose.

### A `.dmg` is not one of them — it is a container around one

Its sectors are compressed and may be stored out of order, so there is nothing
for a driver to read in place. It is peeled like a stream wrapper: the sectors are
decoded to a spool through `go-diskimages/dmg`, and the spool is **sniffed**,
because a `.dmg` holds HFS+, or APFS, or an ISO 9660, and the container does not
record which.

Two consequences worth stating rather than discovering:

- **An unrecognised spool is an error, not a one-entry directory.** A `.gz`
  legitimately holds one ordinary file and `openCompressed` hands that back as
  such; a UDIF image never does — its sectors are a filesystem or they are
  nothing. APFS is the case this matters for: `detect` names it and no driver here
  opens it, and that sentence is more useful than a megabyte of raw sectors
  presented as an archive of one file.
- **A partition map is not handled.** An image carrying one holds a table where a
  filesystem's magic would be, so the sniff finds nothing and says so, naming the
  `.dmg` it came out of. Splitting that apart is a volume manager's job, and
  `go-volumes` has one.

The trailer is what names a UDIF, and it has to be: a `.dmg` whose sectors are
stored raw *begins* with the filesystem inside it, so a prober reading only the
head would recognise that filesystem and read straight past the container. That
is the wrong answer even when it works — the same image written sparse or
compressed would then be unreadable.

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

## Changing an archive without rewriting it

An archive can be opened as a filesystem you may **write to**, where the changes
cost nothing until you say so:

```go
o, format, err := unarchive.Writable("backup.tar.zst")
if err != nil { return err }
defer o.Close()                  // discards the changes; writes nothing back

o.WriteFile("etc/hosts", newHosts, 0o644)
o.MkDir("etc/extra", 0o755)
o.DeleteFile("var/log/old.log")

err = unarchive.SealTo(o, "backup.7z")   // ONE rewrite, here
```

Renaming one entry in a 4 GiB archive costs a rename, not 4 GiB of rewriting.
The archive underneath is untouched until `SealTo`, so abandoning the changes is
free and an interrupted session leaves the original exactly as it was. `SealTo`
builds the new archive **beside** the target and renames over it, so an
interruption there leaves whatever the target held before, rather than half of
something new.

The target's format comes from its **name**. That is the one place in this
package where a name decides anything, and it has to: an output file has no
bytes yet to read.

`unarchive -o` is this, with no changes: a conversion.

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
