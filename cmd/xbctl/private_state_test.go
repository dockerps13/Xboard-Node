package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateFileReplacesPermissiveMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected private file mode: %v", info.Mode())
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "new" {
		t.Fatal("replacement failed", err)
	}
}

func TestMergeCredentialsRejectsMissingInput(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(dst, []byte("OLD=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeCredentials(filepath.Join(dir, "missing"), dst, "NEW", "value"); err == nil {
		t.Fatal("missing input accepted")
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "OLD=keep\n" {
		t.Fatal("existing credentials altered", err)
	}
}

func TestConfigInitRejectsMultilineToken(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.yml")
	err := runConfigInit([]string{"--mode", "node", "--panel-url", "https://example.com", "--node-id", "1", "--token", "abc\nINJECT=value", "--output", out})
	if err == nil {
		t.Fatal("multiline token accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("configuration written before token validation")
	}
}
