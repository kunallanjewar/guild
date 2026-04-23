package lore

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Echo pairs a stale entry with the reason we decided it was stale.
// The CLI prints both so operators can reforge with context.
type Echo struct {
	Entry  *Entry
	Reason string
}

// Echoes returns entries whose valid_days threshold has passed, plus
// optionally (gitAware=true) entries whose file_path has been modified
// in git since the entry was written. Only current-status entries are
// considered — archived/superseded are already out of circulation.
//
// The git-aware branch shells out to `git log` per entry; failures
// degrade silently so missing git / non-repo paths don't block the
// time-based signal.
func Echoes(ctx context.Context, db *sql.DB, project string, gitAware bool) ([]Echo, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: echoes: nil db")
	}
	if strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("lore: echoes: project required")
	}

	//nolint:gosec // G202: entryColumns is a constant; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.project_id = ? AND e.status = 'current'
		ORDER BY e.created_at ASC`
	rows, err := db.QueryContext(ctx, sqlText, project) //sqlcheck:ignore // sqlText is a constant template; entryColumns is a constant
	if err != nil {
		return nil, fmt.Errorf("lore: echoes: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := time.Now().UTC()
	var stale []Echo
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		if reason := echoReason(ctx, e, now, gitAware); reason != "" {
			stale = append(stale, Echo{Entry: e, Reason: reason})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: echoes: iterate: %w", err)
	}
	return stale, nil
}

// echoReason returns a non-empty reason string iff the entry should be
// flagged as a fading echo under the provided policy.
func echoReason(ctx context.Context, e *Entry, now time.Time, gitAware bool) string {
	age := int(daysBetween(e.CreatedAt, now))
	if e.ValidDays != nil && *e.ValidDays > 0 && age >= *e.ValidDays {
		return fmt.Sprintf("%dd old (valid: %dd)", age, *e.ValidDays)
	}
	if gitAware && e.FilePath != "" {
		if gitDate := gitFileLastModified(ctx, e.FilePath); !gitDate.IsZero() && gitDate.After(e.CreatedAt) {
			return "file modified after entry was created"
		}
	}
	return ""
}

// gitFileLastModifiedFn is the seam for the git-aware staleness check.
// Tests replace it to exercise the logic without a real git repo or a
// specific process CWD.
var gitFileLastModifiedFn = defaultGitFileLastModified

// gitFileLastModified is the internal call site; it delegates to the
// injectable seam so tests can substitute behaviour.
func gitFileLastModified(ctx context.Context, path string) time.Time {
	return gitFileLastModifiedFn(ctx, path)
}

// defaultGitFileLastModified shells out to `git log -1 --format=%aI -- <path>`
// and returns the last-modified timestamp parsed to a time.Time.
// Returns the zero time when git is missing, the file is not tracked,
// or any error occurs — best-effort.
//
// The command is run with its working directory set to the directory that
// contains the file, not the process CWD. This ensures the correct
// repository context is used when guild is launched from outside the
// target project's directory tree (e.g. MCP sessions, project overrides).
func defaultGitFileLastModified(ctx context.Context, path string) time.Time {
	// Bound the command with a 5-second timeout so a stuck git
	// doesn't hang the lore read.
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Derive the repository root from the file's own directory so the
	// query is never sensitive to the guild process's ambient CWD.
	dir := filepath.Dir(path)
	//nolint:gosec // arguments are hard-coded + file path from DB
	repoRootCmd := exec.CommandContext(cmdCtx, "git", "rev-parse", "--show-toplevel")
	repoRootCmd.Dir = dir
	repoRootOut, err := repoRootCmd.Output()
	if err != nil {
		// git missing, not a repo, or dir doesn't exist — degrade quietly.
		return time.Time{}
	}
	repoRoot := strings.TrimSpace(string(repoRootOut))

	//nolint:gosec // arguments are hard-coded + file path from DB
	cmd := exec.CommandContext(cmdCtx, "git", "log", "-1", "--format=%aI", "--", path)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return time.Time{}
	}
	return parseSQLiteTime(s)
}
