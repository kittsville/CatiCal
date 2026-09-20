package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBinaryBuilds(t *testing.T) {
	modRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "catical")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/catical")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
}
