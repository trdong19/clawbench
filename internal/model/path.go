package model

import (
	"os"
	"path/filepath"
	"strings"
)

// ValidatePath validates that a relative path stays within the base directory boundary.
// It resolves symlinks on both sides before comparing, preventing symlink traversal attacks.
// Returns the lexical absolute path (for OS operations) and whether it's valid.
func ValidatePath(basePath, relPath string) (string, bool) {
	absBase, err := filepath.Abs(basePath)
	if err != nil {
		return "", false
	}
	fullPath := filepath.Join(absBase, relPath)
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", false
	}

	// Resolve symlinks on both sides to prevent symlink traversal.
	evalBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		return "", false
	}
	evalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", false
		}
		// Target doesn't exist yet (e.g., file creation) — resolve parent directory
		evalPath = ResolveExistingPath(absPath, evalBase)
		if evalPath == "" {
			return "", false
		}
	}

	valid := strings.HasPrefix(evalPath, evalBase+string(filepath.Separator)) || evalPath == evalBase
	return absPath, valid
}

// ResolveExistingPath walks up from absPath to find the first existing ancestor,
// resolves its symlinks, then appends the remaining non-existent components.
// Returns empty string if no ancestor can be resolved or if the resolved path
// escapes evalBase. Exported for use by handler.isPathUnderBase.
func ResolveExistingPath(absPath, _ string) string {
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	parts := []string{base}

	for {
		evalDir, err := filepath.EvalSymlinks(dir)
		if err == nil {
			// Found an existing ancestor — reconstruct the full path
			return filepath.Join(append([]string{evalDir}, parts...)...)
		}
		if !os.IsNotExist(err) {
			return "" // unexpected error
		}
		// Parent doesn't exist either — walk up
		parentDir := filepath.Dir(dir)
		if parentDir == dir {
			return "" // reached root without finding an existing directory
		}
		parts = append([]string{filepath.Base(dir)}, parts...)
		dir = parentDir
	}
}
