package sandbox

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ArtifactDir is the directory a build leaves its artifacts in. Whatever is
// under it when the build exits is what the job hands back.
const ArtifactDir = "/out"

// errFull ends the walk once the next entry would not fit under the cap.
var errFull = errors.New("artifact cap reached")

// writeArchive writes the tree under root to w as a tar of at most limit
// bytes, and says on report what it left out. The tree is the build's, so it
// is read as hostile input:
//
//   - entries are lstat'd and never followed: a symlink is recorded as a
//     link, whatever it points at, and a symlinked directory is not entered;
//   - only regular files are opened, with O_NOFOLLOW and O_NONBLOCK, so a
//     FIFO or a socket that slips through cannot block the read;
//   - everything else — sockets, FIFOs, devices — is skipped and counted;
//   - an entry that would take the archive past limit ends it there, with a
//     well-formed tar of everything before it. Which entries fit is
//     deterministic: the walk is in lexical order.
//
// None of this defends against the tree changing while it is read. The
// caller makes sure nothing of the build is still running first (supervise),
// which is what makes a check-then-open here safe.
//
// A root that does not exist is no artifacts, and writes nothing at all.
func writeArchive(root string, w io.Writer, limit int64, report io.Writer) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	if !fi.IsDir() {
		fmt.Fprintf(report, "forge: artifacts: %s is not a directory; nothing collected\n", root)
		return nil
	}

	out := &capWriter{w: w, left: limit}
	tw := tar.NewWriter(out)
	var kept, skipped int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
		var file *os.File
		switch {
		case d.IsDir():
			hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
		case d.Type()&fs.ModeSymlink != 0:
			if hdr.Linkname, err = os.Readlink(path); err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
		case d.Type().IsRegular():
			if file, hdr.Size, err = openRegular(path); err != nil {
				return err
			}
			if file == nil {
				skipped++
				return nil
			}
			defer file.Close()
			hdr.Typeflag = tar.TypeReg
		default:
			skipped++
			return nil
		}
		// The trailer is two zero blocks; it must still fit after this entry.
		if headerCost(hdr)+roundBlock(hdr.Size)+2*blockSize > out.left {
			fmt.Fprintf(report, "forge: artifacts: %s is over the %d-byte cap; stopped at %s with %d entries kept\n",
				root, limit, name, kept)
			return errFull
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if file != nil {
			if _, err := io.CopyN(tw, file, hdr.Size); err != nil {
				return err
			}
		}
		kept++
		return nil
	})
	if err != nil && !errors.Is(err, errFull) {
		return fmt.Errorf("artifacts: %w", err)
	}
	if skipped > 0 {
		fmt.Fprintf(report, "forge: artifacts: skipped %d entries under %s that are not files, directories or symlinks\n",
			skipped, root)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	return nil
}

// openRegular opens path only if it is still a regular file once open, and
// returns its size. A nil file with no error means it was not one.
func openRegular(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, 0, nil
	}
	return f, fi.Size(), nil
}

const blockSize = 512

func roundBlock(n int64) int64 { return (n + blockSize - 1) / blockSize * blockSize }

// headerCost is an upper bound on what archive/tar writes for hdr before its
// data: one block, plus a PAX extended header — its own block and its
// records — when a name or link is too long or not ASCII for USTAR. The
// bound only has to be safe; capWriter is the backstop if it is not.
func headerCost(hdr *tar.Header) int64 {
	n := int64(len(hdr.Name) + len(hdr.Linkname))
	if n < 100 && isASCII(hdr.Name) && isASCII(hdr.Linkname) {
		return blockSize
	}
	return 2*blockSize + roundBlock(2*n+64)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// capWriter refuses any write past left bytes. writeArchive's own checks keep
// it from being reached; if they are ever wrong, the archive ends in an error
// rather than over the cap.
type capWriter struct {
	w    io.Writer
	left int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, fmt.Errorf("artifacts: archive would pass its cap (a bug in headerCost)")
	}
	n, err := c.w.Write(p)
	c.left -= int64(n)
	return n, err
}
