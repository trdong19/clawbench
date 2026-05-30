package handler

import (
	"archive/zip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxArchivePaths limits the number of paths in a single archive request.
const maxArchivePaths = 1000

const defaultArchiveName = "archive.zip"

// archiveDirEntry walks a directory and adds its contents to the zip writer.
func archiveDirEntry(zw *zip.Writer, entry absEntry) error {
	return filepath.Walk(entry.absPath, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip symlinks that escape root paths (prevent traversal & infinite loops)
		if fi.Mode()&os.ModeSymlink != 0 {
			target, linkErr := filepath.EvalSymlinks(path)
			if linkErr != nil || !isPathUnderAnyRoot(target) {
				slog.Warn("archive: skip symlink escaping root paths", "path", path)
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		rel, relErr := filepath.Rel(filepath.Dir(entry.absPath), path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if fi.IsDir() {
			_, err = zw.Create(rel + "/")
			return err
		}
		return addFileToZip(zw, path, rel, fi)
	})
}

// archiveFileEntry adds a single file entry to the zip writer.
func archiveFileEntry(zw *zip.Writer, entry absEntry, totalEntries int, info os.FileInfo) error {
	rel := filepath.Base(entry.absPath)
	if totalEntries > 1 {
		parentRel := filepath.Dir(entry.relPath)
		if parentRel != "." {
			rel = filepath.ToSlash(parentRel) + "/" + filepath.Base(entry.absPath)
		}
	}
	return addFileToZip(zw, entry.absPath, rel, info)
}

// absEntry holds resolved path info for archive entries.
type absEntry struct {
	absPath string
	relPath string // original relative path for zip entry prefix
}

// resolveArchiveEntries resolves and validates all archive request paths.
func resolveArchiveEntries(w http.ResponseWriter, r *http.Request, paths []string) ([]absEntry, bool) {
	entries := make([]absEntry, 0, len(paths))
	for _, p := range paths {
		absPath, ok := resolveAbsPath(w, r, p)
		if !ok {
			return nil, false
		}
		entries = append(entries, absEntry{absPath: absPath, relPath: p})
	}
	return entries, true
}

// countAccessibleEntries returns how many entries have accessible paths.
func countAccessibleEntries(entries []absEntry) int {
	accessible := 0
	for _, entry := range entries {
		if _, err := os.Stat(entry.absPath); err == nil {
			accessible++
		}
	}
	return accessible
}

// computeArchiveZipName computes a friendly zip filename from the entries.
func computeArchiveZipName(entries []absEntry) string {
	if len(entries) != 1 {
		return defaultArchiveName
	}
	base := filepath.Base(entries[0].relPath)
	base = strings.TrimRight(base, "/")
	if base != "" && base != "." {
		return base + ".zip"
	}
	return defaultArchiveName
}

// writeArchiveEntries writes all entries to the zip writer.
func writeArchiveEntries(zw *zip.Writer, entries []absEntry) int {
	written := 0
	for _, entry := range entries {
		info, err := os.Stat(entry.absPath)
		if err != nil {
			slog.Warn("archive: skip missing path", "path", entry.absPath, "err", err)
			continue
		}

		if info.IsDir() {
			if walkErr := archiveDirEntry(zw, entry); walkErr != nil {
				slog.Warn("archive: walk error", "dir", entry.absPath, "err", walkErr)
			}
		} else {
			if addErr := archiveFileEntry(zw, entry, len(entries), info); addErr != nil {
				slog.Warn("archive: add file error", "path", entry.absPath, "err", addErr)
			}
		}
		written++
	}
	return written
}

// ServeFileArchive handles POST /api/file/archive
// Accepts { paths: ["rel/path1", "rel/path2"] } and streams a zip archive.
// Paths can be files or directories; each is walked and added to the zip.
func ServeFileArchive(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Paths) == 0 {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "MissingPath")
		return
	}
	if len(req.Paths) > maxArchivePaths {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "ArchiveFailed")
		return
	}

	entries, ok := resolveArchiveEntries(w, r, req.Paths)
	if !ok {
		return
	}

	if countAccessibleEntries(entries) == 0 {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "ArchiveFailed")
		return
	}

	safeName := sanitizeArchiveName(computeArchiveZipName(entries))

	// Set response headers before writing any data
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName))
	w.Header().Set("Cache-Control", "no-store")

	// Stream zip directly to response writer
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	written := writeArchiveEntries(zw, entries)
	if written == 0 {
		slog.Warn("archive: no files written")
	}
}

// sanitizeArchiveName removes or replaces characters that could break
// the Content-Disposition header (quotes, backslashes, control chars).
func sanitizeArchiveName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, name)
}

// addFileToZip adds a single file to the zip writer.
func addFileToZip(zw *zip.Writer, absPath, zipRelPath string, fi os.FileInfo) error {
	fh, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	fh.Name = zipRelPath
	fh.Method = zip.Deflate

	w, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}

	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	_, err = io.Copy(w, f)
	return err
}
