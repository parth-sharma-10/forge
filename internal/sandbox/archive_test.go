package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type entry struct {
	typ  byte
	link string
	body string
}

// readArchive parses what writeArchive wrote, failing the test on anything
// that is not a well-formed tar.
func readArchive(t *testing.T, b []byte) map[string]entry {
	t.Helper()
	got := map[string]entry{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("archive is not a well-formed tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		got[hdr.Name] = entry{typ: hdr.Typeflag, link: hdr.Linkname, body: string(body)}
	}
}

func archive(t *testing.T, root string, limit int64) (map[string]entry, string, int) {
	t.Helper()
	var out, report bytes.Buffer
	if err := writeArchive(root, &out, limit, &report); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	if int64(out.Len()) > limit {
		t.Fatalf("archive is %d bytes, cap %d", out.Len(), limit)
	}
	return readArchive(t, out.Bytes()), report.String(), out.Len()
}

func TestArchiveKeepsFilesDirectoriesAndModes(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(root, "report.txt"), []byte("ok\n"), 0o644)

	got, report, _ := archive(t, root, 1<<20)
	want := map[string]entry{
		"bin/":       {typ: tar.TypeDir},
		"bin/tool":   {typ: tar.TypeReg, body: "#!/bin/sh\n"},
		"report.txt": {typ: tar.TypeReg, body: "ok\n"},
	}
	if len(got) != len(want) {
		t.Fatalf("archive = %v, want %v", got, want)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
	if report != "" {
		t.Errorf("report = %q, want nothing", report)
	}
}

// A symlink out of the tree — to a secret, or to a directory — is recorded
// as the link it is. Following it would put the target in the archive.
func TestArchiveRecordsSymlinksWithoutFollowingThem(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "shadow")
	os.WriteFile(secret, []byte("root:SECRET"), 0o600)
	os.WriteFile(filepath.Join(outside, "inner"), []byte("SECRET"), 0o600)

	root := t.TempDir()
	os.Symlink(secret, filepath.Join(root, "passwd"))
	os.Symlink(outside, filepath.Join(root, "etc"))

	var out bytes.Buffer
	if err := writeArchive(root, &out, 1<<20, io.Discard); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("SECRET")) {
		t.Fatal("the archive holds a symlink target's contents")
	}
	got := readArchive(t, out.Bytes())
	if e := got["passwd"]; e.typ != tar.TypeSymlink || e.link != secret {
		t.Errorf("passwd = %+v, want a symlink to %s", e, secret)
	}
	if e := got["etc"]; e.typ != tar.TypeSymlink || e.link != outside {
		t.Errorf("etc = %+v, want a symlink to %s", e, outside)
	}
	if len(got) != 2 {
		t.Errorf("archive = %v, want the two links and nothing under them", got)
	}
}

// A FIFO opened for reading blocks until a writer appears, and nothing will
// ever write it. It and a socket are skipped, counted, and do not hang.
func TestArchiveSkipsFIFOsAndSocketsWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A unix socket path is limited to ~100 bytes, and TempDir's can be
	// longer, so it is bound short and moved in. Raw syscalls rather than
	// package net, which would link this test binary against libc under cgo;
	// see test-sandbox in the Makefile.
	sockDir, err := os.MkdirTemp("/tmp", "forge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: filepath.Join(sockDir, "s")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(sockDir, "s"), filepath.Join(root, "sock")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "z"), []byte("after"), 0o644)

	done := make(chan struct{})
	var got map[string]entry
	var report string
	go func() {
		defer close(done)
		got, report, _ = archive(t, root, 1<<20)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writeArchive blocked on a special file")
	}
	if len(got) != 1 || got["z"].body != "after" {
		t.Errorf("archive = %v, want only z", got)
	}
	if !strings.Contains(report, "skipped 2 entries") {
		t.Errorf("report = %q, want the two skips counted", report)
	}
}

// Twice the cap: the archive stops before the entry that would cross it,
// is still a valid tar, and the report says where it stopped.
func TestArchiveStopsAtTheCap(t *testing.T) {
	const limit = 64 << 10
	root := t.TempDir()
	chunk := bytes.Repeat([]byte("x"), 8<<10)
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p"} {
		os.WriteFile(filepath.Join(root, n), chunk, 0o644)
	}

	got, report, size := archive(t, root, limit)
	if len(got) == 0 || len(got) >= 16 {
		t.Fatalf("archive kept %d of 16 files under a cap of half their size", len(got))
	}
	// Lexical order: what is kept is a prefix.
	for i, n := range []string{"a", "b", "c", "d", "e", "f"} {
		if i < len(got) && got[n].body != string(chunk) {
			t.Errorf("%s missing or short; a prefix of the walk should be kept", n)
		}
	}
	if !strings.Contains(report, "over the 65536-byte cap") {
		t.Errorf("report = %q, want the truncation reported", report)
	}
	if size > limit {
		t.Errorf("archive is %d bytes, over the cap", size)
	}
}

// Long names take a PAX header, which the cap estimate has to include.
func TestArchiveCapHoldsWithLongNames(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("n", 200)
	for i := 0; i < 40; i++ {
		os.WriteFile(filepath.Join(root, long+string(rune('a'+i%26))+strings.Repeat("é", i)), []byte("x"), 0o644)
	}
	// Every block-aligned cap in the range, so some cap lands in the gap a
	// wrong estimate leaves between the check and the trailer.
	for limit := int64(2 << 10); limit <= 40<<10; limit += blockSize {
		archive(t, root, limit) // fails the test if the cap is crossed
	}
}

// A million empty files is the cheap way to a large archive: no data, all
// header. The cap has to count the headers, not only the bytes in files.
func TestArchiveCapCountsHeaders(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 200; i++ {
		os.WriteFile(filepath.Join(root, strings.Repeat("e", 1+i%50)+string(rune('a'+i/50))), nil, 0o644)
	}
	got, report, _ := archive(t, root, 16<<10)
	if len(got) >= 200 || !strings.Contains(report, "over the") {
		t.Errorf("kept %d of 200 empty files in 16 KiB, report %q; want a truncation", len(got), report)
	}
}

func TestArchiveOfNothing(t *testing.T) {
	var out, report bytes.Buffer
	if err := writeArchive(filepath.Join(t.TempDir(), "missing"), &out, 1<<20, &report); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || report.Len() != 0 {
		t.Errorf("a missing root wrote %d bytes and reported %q, want nothing", out.Len(), report.String())
	}

	file := filepath.Join(t.TempDir(), "out")
	os.WriteFile(file, []byte("x"), 0o644)
	if err := writeArchive(file, &out, 1<<20, &report); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(report.String(), "not a directory") {
		t.Errorf("a file as the root wrote %d bytes and reported %q", out.Len(), report.String())
	}
}
