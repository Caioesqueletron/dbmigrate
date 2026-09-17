package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Migration is one versioned schema change, backed by a pair of files:
//
//	migrations/20240115093000_add_users_table.up.sql
//	migrations/20240115093000_add_users_table.down.sql
//
// The version is the timestamp prefix (YYYYMMDDHHMMSS), which sorts
// lexicographically the same as numerically and, unlike a plain
// auto-increment counter, avoids collisions when two developers create
// migrations on different branches at the same time.
type Migration struct {
	Version  int64
	Name     string
	UpSQL    string
	DownSQL  string
	Checksum string // sha256 of UpSQL+DownSQL, used to detect drift
}

var filenameRE = regexp.MustCompile(`^(\d{14})_([a-zA-Z0-9_\-]+)\.(up|down)\.sql$`)

// LoadDir scans dir for *.up.sql/*.down.sql pairs and returns them sorted
// by version ascending. It errors on an up file with no matching down
// file (or vice versa) rather than silently skipping it — a migration
// tool should fail loudly on an inconsistent migrations directory.
func LoadDir(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading migrations dir %s: %w", dir, err)
	}

	type halves struct {
		up, down string
		name     string
	}
	byVersion := map[int64]*halves{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := filenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue // ignore unrelated files (README, .gitkeep, ...)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad version in filename %s: %w", e.Name(), err)
		}
		name, direction := m[2], m[3]

		h, ok := byVersion[version]
		if !ok {
			h = &halves{name: name}
			byVersion[version] = h
		}

		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
		}
		if direction == "up" {
			h.up = string(content)
		} else {
			h.down = string(content)
		}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for version, h := range byVersion {
		if h.up == "" {
			return nil, fmt.Errorf("migration %d (%s) is missing its .up.sql file", version, h.name)
		}
		if h.down == "" {
			return nil, fmt.Errorf("migration %d (%s) is missing its .down.sql file", version, h.name)
		}
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     h.name,
			UpSQL:    h.up,
			DownSQL:  h.down,
			Checksum: checksum(h.up, h.down),
		})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

func checksum(up, down string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(up) + "\x00" + strings.TrimSpace(down)))
	return hex.EncodeToString(sum[:])
}
