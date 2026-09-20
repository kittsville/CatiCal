package version

import "testing"

func TestResolvePrefersEnvSHAOverBuild(t *testing.T) {
	Commit = "bbbbbbcccccccccccccccccccccccccccccccccc"
	t.Cleanup(func() { Commit = "" })
	got := Resolve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if got != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveIgnoresCoolifyHEAD(t *testing.T) {
	Commit = "abcdef1234567890deadbeefcafebabe01234567"
	t.Cleanup(func() { Commit = "" })
	got := Resolve("HEAD")
	if got != Commit {
		t.Fatalf("got %q want baked SHA", got)
	}
}

func TestResolveEmptyWithoutBuild(t *testing.T) {
	Commit = ""
	if got := Resolve(""); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := Resolve("HEAD"); got != "" {
		t.Fatalf("got %q", got)
	}
}
