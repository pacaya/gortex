package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureIndexDirGitignore(t *testing.T) {
	read := func(dir string) string {
		b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
		if err != nil {
			t.Fatalf("read .gitignore: %v", err)
		}
		return string(b)
	}

	// created: an empty index dir gets the default written.
	t.Run("created", func(t *testing.T) {
		dir := t.TempDir()
		wrote, err := EnsureIndexDirGitignore(dir)
		if err != nil || !wrote {
			t.Fatalf("wrote=%v err=%v, want wrote=true", wrote, err)
		}
		if got := read(dir); got != DefaultIndexGitignore {
			t.Errorf("content = %q, want default", got)
		}
	})

	// unchanged: a dir already holding the current default is a no-op.
	t.Run("unchanged", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := EnsureIndexDirGitignore(dir); err != nil {
			t.Fatal(err)
		}
		wrote, err := EnsureIndexDirGitignore(dir)
		if err != nil || wrote {
			t.Errorf("second call wrote=%v err=%v, want wrote=false", wrote, err)
		}
	})

	// healed: a stale bare-`*` default is rewritten to the current default.
	t.Run("healed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		wrote, err := EnsureIndexDirGitignore(dir)
		if err != nil || !wrote {
			t.Fatalf("heal wrote=%v err=%v, want wrote=true", wrote, err)
		}
		if got := read(dir); got != DefaultIndexGitignore {
			t.Errorf("healed content = %q, want default", got)
		}
	})

	// kept: a user-customized .gitignore is left untouched.
	t.Run("kept", func(t *testing.T) {
		dir := t.TempDir()
		const custom = "!keepme.json\nstore.sqlite\n"
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(custom), 0o644); err != nil {
			t.Fatal(err)
		}
		wrote, err := EnsureIndexDirGitignore(dir)
		if err != nil || wrote {
			t.Errorf("kept wrote=%v err=%v, want wrote=false", wrote, err)
		}
		if got := read(dir); got != custom {
			t.Errorf("customized content was altered: %q", got)
		}
	})
}
