package extract

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

func WriteTarExclude(
	writer io.Writer,
	localPath string,
	compress bool,
	excludedPaths []string,
) error {
	return WriteTarMatch(writer, localPath, compress, NewPatternMatcher(excludedPaths))
}

// WriteTarMatch is like WriteTarExclude but takes a prebuilt gitignore.Matcher,
// letting callers supply matchers built from richer sources (e.g. a tree of
// nested .gitignore files via gitignore.ReadPatterns).
func WriteTarMatch(
	writer io.Writer,
	localPath string,
	compress bool,
	matcher gitignore.Matcher,
) error {
	absolute, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("absolute: %w", err)
	}

	// Check if target is there
	stat, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	// Use compression
	gw := writer
	if compress {
		gwWriter := gzip.NewWriter(writer)
		defer func() { _ = gwWriter.Close() }()

		gw = gwWriter
	}

	// Create tar writer
	tarWriter := tar.NewWriter(gw)
	defer func() { _ = tarWriter.Close() }()

	// When its a file we copy the file to the toplevel of the tar
	if !stat.IsDir() {
		return NewArchiver(filepath.Dir(absolute), tarWriter, matcher).
			AddToArchive(filepath.Base(absolute))
	}

	// When its a folder we copy the contents and not the folder itself to the
	// toplevel of the tar
	return NewArchiver(absolute, tarWriter, matcher).AddToArchive("")
}

func WriteTar(writer io.Writer, localPath string, compress bool) error {
	return WriteTarExclude(writer, localPath, compress, nil)
}

// NewPatternMatcher compiles a list of gitignore-style pattern lines into a
// gitignore.Matcher. Blank lines and "#" comments are skipped. All patterns
// share the root domain, which matches what we want when the patterns come
// from a single root-level ignore file.
func NewPatternMatcher(excludedPaths []string) gitignore.Matcher {
	patterns := make([]gitignore.Pattern, 0, len(excludedPaths))
	for _, p := range excludedPaths {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(p, nil))
	}
	return gitignore.NewMatcher(patterns)
}

// Archiver is responsible for compressing specific files and folders within a target directory.
type Archiver struct {
	basePath     string
	writer       *tar.Writer
	writtenFiles map[string]bool

	matcher gitignore.Matcher
}

// NewArchiver creates a new archiver scoped to basePath. Paths added to the
// archive are matched against matcher; entries that match are skipped and
// directories that match are not recursed into.
func NewArchiver(basePath string, writer *tar.Writer, matcher gitignore.Matcher) *Archiver {
	if matcher == nil {
		matcher = gitignore.NewMatcher(nil)
	}
	return &Archiver{
		basePath:     basePath,
		writer:       writer,
		writtenFiles: map[string]bool{},
		matcher:      matcher,
	}
}

// AddToArchive adds a new path to the archive.
func (a *Archiver) AddToArchive(relativePath string) error {
	if a.writtenFiles[relativePath] {
		return nil
	}

	// We skip files that are suddenly not there anymore
	stat, err := os.Lstat(path.Join(a.basePath, relativePath))
	if err != nil {
		// config.Logf("[Upstream] Couldn't stat file %s: %s\n", absFilepath, err.Error())
		return nil
	}

	if stat.IsDir() {
		if a.isExcluded(path.Clean(relativePath), true) {
			return nil
		}

		// Recursively tar folder
		return a.tarFolder(relativePath, stat)
	}

	if a.isExcluded(path.Clean(relativePath), false) {
		return nil
	}
	return a.tarFile(relativePath, stat)
}

func (a *Archiver) isExcluded(relativePath string, isDir bool) bool {
	if relativePath == "" || relativePath == "." {
		return false
	}
	return a.matcher.Match(strings.Split(relativePath, "/"), isDir)
}

func (a *Archiver) tarFolder(target string, targetStat os.FileInfo) error {
	filePath := path.Join(a.basePath, target)
	files, err := os.ReadDir(filePath)
	if err != nil {
		// config.Logf("[Upstream] Couldn't read dir %s: %s\n", filepath, err.Error())
		return nil
	}

	if len(files) == 0 && target != "" {
		// Case empty directory
		hdr, _ := tar.FileInfoHeader(targetStat, filePath)
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Mode = fillGo18FileTypeBits(int64(chmodTarEntry(os.FileMode(hdr.Mode))), targetStat)
		hdr.Name = target
		if err := a.writer.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar write header: %w", err)
		}
		a.writtenFiles[target] = true
	}

	for _, dirEntry := range files {
		f, err := dirEntry.Info()
		if err != nil {
			continue
		}

		if err = a.AddToArchive(path.Join(target, f.Name())); err != nil {
			return fmt.Errorf("recursive tar %s: %w", f.Name(), err)
		}
	}

	return nil
}

func (a *Archiver) tarFile(target string, targetStat os.FileInfo) error {
	filePath := path.Join(a.basePath, target)

	// Symlinks: header only, no body. Read the link from the Lstat path.
	if targetStat.Mode()&os.ModeSymlink == os.ModeSymlink {
		linkName, err := os.Readlink(filePath)
		if err != nil {
			return nil
		}
		return a.writeFileHeader(target, targetStat, linkName)
	}

	// For regular files, open before writing the header so the size we
	// declare matches what we can actually deliver. Skipping a file after the
	// header is written corrupts the tar stream for every subsequent entry.
	f, err := os.Open(filePath) // #nosec G304
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil
	}

	if err := a.writeFileHeader(target, fi, ""); err != nil {
		return err
	}

	if !fi.Mode().IsRegular() {
		return nil
	}

	// Bound the read to the size we declared and pad with zeros if the file
	// shrinks before we finish, so the body always matches the header size.
	copied, err := io.Copy(a.writer, io.LimitReader(f, fi.Size()))
	if err != nil {
		return fmt.Errorf("tar copy file: %w", err)
	}
	if copied < fi.Size() {
		if _, err := io.CopyN(a.writer, zeroReader{}, fi.Size()-copied); err != nil {
			return fmt.Errorf("tar pad file: %w", err)
		}
	}

	return nil
}

func (a *Archiver) writeFileHeader(target string, fi os.FileInfo, linkName string) error {
	hdr, err := tar.FileInfoHeader(fi, linkName)
	if err != nil {
		return fmt.Errorf("create tar file info header: %w", err)
	}
	hdr.Name = target
	hdr.Uid = 0
	hdr.Gid = 0
	hdr.Mode = fillGo18FileTypeBits(int64(chmodTarEntry(os.FileMode(hdr.Mode))), fi) // #nosec G115
	hdr.ModTime = time.Unix(fi.ModTime().Unix(), 0)
	if err := a.writer.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar write header: %w", err)
	}
	a.writtenFiles[target] = true
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

const (
	modeISDIR  = 0o40000  // Directory
	modeISFIFO = 0o10000  // FIFO
	modeISREG  = 0o100000 // Regular file
	modeISLNK  = 0o120000 // Symbolic link
	modeISBLK  = 0o60000  // Block special file
	modeISCHR  = 0o20000  // Character special file
	modeISSOCK = 0o140000 // Socket
)

// chmodTarEntry is used to adjust the file permissions used in tar header based
// on the platform the archival is done.
func chmodTarEntry(perm os.FileMode) os.FileMode {
	if runtime.GOOS != "windows" {
		return perm
	}

	// perm &= 0755 // this 0-ed out tar flags (like link, regular file, directory marker etc.)
	permPart := perm & os.ModePerm
	noPermPart := perm &^ os.ModePerm
	// Add the x bit: make everything +x from windows
	permPart |= 0o111
	permPart &= 0o755

	return noPermPart | permPart
}

// fillGo18FileTypeBits fills type bits which have been removed on Go 1.9 archive/tar
// https://github.com/golang/go/commit/66b5a2f
func fillGo18FileTypeBits(mode int64, fi os.FileInfo) int64 {
	fm := fi.Mode()
	switch {
	case fm.IsRegular():
		mode |= modeISREG
	case fi.IsDir():
		mode |= modeISDIR
	case fm&os.ModeSymlink != 0:
		mode |= modeISLNK
	case fm&os.ModeDevice != 0:
		if fm&os.ModeCharDevice != 0 {
			mode |= modeISCHR
		} else {
			mode |= modeISBLK
		}
	case fm&os.ModeNamedPipe != 0:
		mode |= modeISFIFO
	case fm&os.ModeSocket != 0:
		mode |= modeISSOCK
	}
	return mode
}
