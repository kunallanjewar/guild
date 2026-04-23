package lore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// canonicalizeProjectFilePath normalizes a stored lore file_path.
//
// Empty stays empty. Absolute paths are cleaned in-place. Relative paths are
// resolved against the registered project root so later git-aware reads do not
// depend on the guild process's ambient cwd.
func canonicalizeProjectFilePath(ctx context.Context, db *sql.DB, projectID, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}

	var projectRoot string
	err := db.QueryRowContext(ctx,
		`SELECT path FROM projects WHERE id = ?`,
		projectID,
	).Scan(&projectRoot)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("lore: canonicalize file_path: project %q not found", projectID)
		}
		return "", fmt.Errorf("lore: canonicalize file_path: lookup project %q: %w", projectID, err)
	}
	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return "", fmt.Errorf("lore: canonicalize file_path: project %q has empty path", projectID)
	}
	return filepath.Clean(filepath.Join(projectRoot, raw)), nil
}
