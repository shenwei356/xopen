[![GoDoc](https://godoc.org/github.com/shenwei356/xopen?status.png)](https://godoc.org/github.com/shenwei356/xopen)

# xopen

    import "github.com/shenwei356/xopen"

xopen makes it easy to get buffered (possibly `gzip`-, `xz`-, `zstd`-, `bzip2`-, or `lz4`- compressed) readers and writers. and
close all of the associated files. `Ropen` opens a file for reading. `Wopen` opens a
file for writing. 

> This packages is forked from https://github.com/brentp/xopen ,
> but I have modified too much :(

Requires Go 1.25.5 or later. Run `go test -race ./...` to check the package.

## Usage

Here's how to get a buffered reader:

```go
// gzipped
rdr, err := xopen.Ropen("some.gz")
// xz compressed
rdr, err := xopen.Ropen("some.xz")
// zstd compressed
rdr, err := xopen.Ropen("some.zst")
// normal
rdr, err := xopen.Ropen("some.txt")
// stdin (possibly gzip-, xz-, or zstd-compressed)
rdr, err := xopen.Ropen("-")
// https://
rdr, err := xopen.Ropen("http://example.com/some-file.txt")
// Cmd
rdr, err := xopen.Ropen("| ls -lh somefile.gz")
// User directory:
rdr, err := xopen.Ropen("~/shenwei356/somefile")

checkError(err)
defer func() { checkError(rdr.Close()) }()
```

`Buf` and `Ropen` return `ErrNoContent` only for an empty stream that ends
normally. Read and decompression errors are returned by the constructor or a
subsequent read; callers must check both. `Buf` leaves its input open if
construction fails, while `Ropen` closes inputs it opened.

Stopping early does not validate unread compressed data. Read to EOF to check
the whole stream, then close the reader to release its resources.

Writer

```go
wtr, err := xopen.Wopen("some.gz")
checkError(err)
defer func() { checkError(wtr.Close()) }()

checkError(wtr.Flush())
```

`Close` releases resources even after a read or write error; repeated calls return
its original result. Readers and writers must not be used concurrently or after
`Close`. As before, closing a stream opened with `"-"` also closes stdin or stdout.

Set `Level` before starting concurrent writes. Compression levels, buffer sizes,
and default compression settings are unchanged. Invalid gzip, zstd, or bzip2
levels are rejected before the destination is opened or truncated.

`Flush` flushes gzip and zstd compressor buffers. For parallel LZ4 it completes
the current frame and starts another on the next write, so data and write errors
are available when `Flush` returns. Xz and bzip2 require `Close` to finish their
compressed output. `Flush` does not call `File.Sync`.

Command inputs (`|command args`) accept multiple arguments, whitespace, quotes,
and escapes without starting a shell. Nonzero exits are returned as errors;
closing early stops and reaps the direct child process.
