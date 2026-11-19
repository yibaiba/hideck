package modem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIMEICacheRejectsReusedSerialNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ttyUSB2")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	node, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	storeCachedIMEIForProfile(path, "111111111111111", node, ATProfileDefault)
	if got := loadCachedIMEIForProfile(path, ATProfileDefault); got != "111111111111111" {
		t.Fatalf("cache = %q", got)
	}
	// Keep the old inode alive so this models replacement rather than inode reuse.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadCachedIMEIForProfile(path, ATProfileDefault); got != "" {
		t.Fatalf("reused port retained identity %q", got)
	}
	storeCachedIMEIForProfile(path, "111111111111111", node, ATProfileDefault)
	if got := loadCachedIMEIForProfile(path, ATProfileDefault); got != "" {
		t.Fatalf("late probe poisoned new node: %q", got)
	}
}
