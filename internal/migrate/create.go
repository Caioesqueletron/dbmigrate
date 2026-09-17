package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var slugRE = regexp.MustCompile(`[^a-z0-9_]+`)

// Create scaffolds a new pair of empty .up.sql/.down.sql files under dir,
// named with the current UTC timestamp so versions are monotonically
// increasing and collision-free across branches/developers.
func Create(dir, name string) (upPath, downPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("creating migrations dir: %w", err)
	}

	slug := slugRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "_")
	slug = strings.Trim(slug, "_")
	if slug == "" {
		return "", "", fmt.Errorf("migration name is empty after slugifying %q", name)
	}

	version := time.Now().UTC().Format("20060102150405")
	base := fmt.Sprintf("%s_%s", version, slug)

	upPath = filepath.Join(dir, base+".up.sql")
	downPath = filepath.Join(dir, base+".down.sql")

	if err := os.WriteFile(upPath, []byte("-- +migrate up\n-- Write your schema change here.\n"), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(downPath, []byte("-- +migrate down\n-- Write the rollback for the change above.\n"), 0o644); err != nil {
		return "", "", err
	}
	return upPath, downPath, nil
}
