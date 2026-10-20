package committour

import (
	"path/filepath"
	"testing"
)

func TestProgress(t *testing.T) {
	dir, _ := gitRepo(t)
	common := filepath.Join(dir, ".git")
	hash := "0123456789abcdef0123456789abcdef01234567"

	// Without a server-started log, recording is a no-op.
	if err := RecordProgress(dir, hash, "survey"); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadProgress(common, hash); err != nil || got != nil {
		t.Fatalf("untracked progress = %v, %v; want nil", got, err)
	}

	steps := func() []string {
		t.Helper()
		got, err := ReadProgress(common, hash)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range got {
			names = append(names, m.Step)
		}
		return names
	}
	if err := StartProgress(common, hash); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"survey", "scaffold"} {
		if err := RecordProgress(dir, hash, step); err != nil {
			t.Fatal(err)
		}
	}
	if got := steps(); len(got) != 3 || got[0] != "start" || got[2] != "scaffold" {
		t.Fatalf("steps = %v", got)
	}

	// A retry starts the log over.
	if err := StartProgress(common, hash); err != nil {
		t.Fatal(err)
	}
	if got := steps(); len(got) != 1 || got[0] != "start" {
		t.Fatalf("steps after restart = %v", got)
	}
}
