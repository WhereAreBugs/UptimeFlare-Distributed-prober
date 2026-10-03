package main

import "testing"

func TestExplicitDataDirectoryWithoutUserHome(t *testing.T) {
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA"} {
		t.Setenv(name, "")
	}
	directory := t.TempDir()
	got, err := resolveDataDirectory(directory)
	if err != nil || got != directory {
		t.Fatalf("explicit service directory = %q, %v", got, err)
	}
	if _, err := resolveDataDirectory(""); err == nil {
		t.Fatal("missing default user directory should report an error")
	}
}
