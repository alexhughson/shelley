package committour

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Milestone is a step `shelley tour` reached while building a commit's tour.
type Milestone struct {
	Step string    `json:"step"`
	At   time.Time `json:"at"`
}

func progressPath(commonDir, hash string) string {
	return filepath.Join(commonDir, "shelley-tour-progress", hash)
}

// StartProgress replaces the commit's progress log with a single "start" milestone.
func StartProgress(commonDir, hash string) error {
	path := progressPath(commonDir, hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return err
	}
	return appendProgress(path, "start")
}

// RecordProgress appends step to the commit's progress log in the repository
// holding dir. Only builds started by the server have a log; otherwise it is a no-op.
func RecordProgress(dir, hash, step string) error {
	out, err := gitOutput(dir, "", nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	err = appendProgress(progressPath(strings.TrimSpace(string(out)), hash), step)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func appendProgress(path, step string) error {
	line, err := json.Marshal(Milestone{Step: step, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadProgress returns the commit's recorded milestones, oldest first.
func ReadProgress(commonDir, hash string) ([]Milestone, error) {
	f, err := os.Open(progressPath(commonDir, hash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var milestones []Milestone
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var m Milestone
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			return nil, err
		}
		milestones = append(milestones, m)
	}
	return milestones, scanner.Err()
}
