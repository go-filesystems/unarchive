# Fixtures

Every one of these was written by the tool that owns its format, and read back by
that tool before it was kept. None was written by this package.

| file | written by | read back with |
|---|---|---|
| `xar-cli.xar` | `/usr/bin/xar` (macOS) | `xar -tf` |
| `gcab.cab` | `gcab` 1.6 | `cabextract -l` |
| `lzop.tar.lzo` | `lzop` 1.04 | `lzop -dc \| tar tf -` |
| `lzip.tar.lz` | `lzip` 1.26 | `lzip -dc \| tar tf -` |
| `hdiutil-iso-in-udzo.dmg` | `hdiutil makehybrid`, then `hdiutil convert` | — |
| `hdiutil-nofs-in-udzo.dmg` | `hdiutil convert` over 1 MiB of ordinary bytes | — |
| `hfsplus-in-udzo.dmg` | `hdiutil convert`; the **volume** by `go-filesystems/hfsplus` | `hdiutil imageinfo` names it `Apple_HFS` |

They are embedded with `//go:embed`, not read from this directory: the emulated CI
lanes run a `go test -c` binary with no `testdata/` beside it, and none of the
tools above exists on a Linux runner either — so a test that shelled out would
skip everywhere that matters.

## Two that came from elsewhere

**`rootfiles-el9-zstd.rpm`** — a real `rootfiles` package built by `rpmbuild` on
Rocky 9, taken from `go-filesystems/rpm`'s own fixtures. Its RPM tag 1014 reads
`License: Public Domain`, so redistributing it carries no restriction and no
attribution obligation. `go-filesystems/rpm/testdata/README.md` has the
provenance URL and the licence reasoning in full.

**`warcio.warc`** — written by [warcio](https://github.com/webrecorder/warcio)
(Apache-2.0), taken from `go-filesystems/warc`'s own fixtures. The framing is
warcio's; the payloads in it are ours, so nothing third-party is redistributed
beyond the framing itself.

Both are somebody else's bytes, so the tests assert that named entries come back
**non-empty** rather than pinning their contents: pinning them would be pinning a
copy, and the thing under test here is the route to the driver, not the driver.

## The symbolic-link pair

| file | written by |
|---|---|
| `gnutar-links.tar` | macOS `tar`, `COPYFILE_DISABLE=1` so no AppleDouble entries |
| `cpio-links.cpio` | `find . -print \| cpio -o -H newc` |

Both hold the same three links — `link-to-real -> real.txt`, `sub/link-up ->
../real.txt`, `link-absolute -> /etc/passwd` — because the two arrive by different
routes: tar has a driver of its own, cpio comes through the index and the `io/fs`
adapter, and **neither declared the symlink type**.

The hostile archives are built in the test with `archive/tar` rather than
committed: a link followed by an entry beneath it is not something `tar` will write
for you, and a fixture nobody can regenerate is a fixture nobody can check.
