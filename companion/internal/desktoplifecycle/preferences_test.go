//go:build linux

package desktoplifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPausePreferenceIsScopedAndPersisted(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "desktop-control-preferences.json")
	preference := filePausePreference{path: path}
	if paused, err := preference.Load("https://my.personastack.ai"); err != nil || paused {
		t.Fatalf("initial Load() = %t, %v", paused, err)
	}
	if err := preference.Save("https://my.personastack.ai", true); err != nil {
		t.Fatal(err)
	}
	if paused, err := (filePausePreference{path: path}).Load("https://my.personastack.ai"); err != nil || !paused {
		t.Fatalf("persisted Load() = %t, %v", paused, err)
	}
	if paused, err := preference.Load("https://mcp.personastack.ai"); err != nil || paused {
		t.Fatalf("other origin Load() = %t, %v", paused, err)
	}
	if err := preference.Save("https://my.personastack.ai", false); err != nil {
		t.Fatal(err)
	}
	if paused, err := preference.Load("https://my.personastack.ai"); err != nil || paused {
		t.Fatalf("resumed Load() = %t, %v", paused, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("preference file mode = %v, %v", info, err)
	}
}

func TestPausePreferenceRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "desktop-control-preferences.json")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := (filePausePreference{path: path}).Save("https://my.personastack.ai", true); err == nil {
		t.Fatal("Save() followed a preference symlink")
	}
}
