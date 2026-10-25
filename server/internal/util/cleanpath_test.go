package util

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"/a/b/../c": "/a/c",
		"/a//b/":    "/a/b",
		"a/./b":     "a/b",
		"../x":      "../x",
		"/":         "/",
	}
	for in, want := range cases {
		got, err := CleanPath(in)
		if err != nil || got != filepath.FromSlash(want) {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a\x00b"} {
		if _, err := CleanPath(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanPath(%q) err = %v; want ErrInvalidPath", bad, err)
		}
	}
}

func TestFileHelpersRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x", "y")
	if err := MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "f.txt")
	if err := WriteFile(file, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := Stat(file); err != nil || info.Size() != 2 {
		t.Fatalf("Stat = %v, %v", info, err)
	}
	if b, err := ReadFile(file); err != nil || string(b) != "hi" {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
	if _, err := Stat(""); !errors.Is(err, os.ErrInvalid) && err == nil {
		t.Fatal("Stat(\"\") should fail")
	}
}
