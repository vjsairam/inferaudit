package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArguments(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--from", "testdata/lab-2026-08-23", "--html", "", "extra"}, "unexpected argument"},
		{[]string{"--select", "app=a,app=b"}, "twice"},
		{[]string{"--from", "testdata/lab-2026-08-23", "--html", "r.out", "--json", "r.out"}, "same file"},
		{[]string{"--from", "testdata/lab-2026-08-23", "-n", "x"}, "don't apply"},
	} {
		if err := run(tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: err = %v, want %q", tt.args, err, tt.want)
		}
	}
}

func TestSamePathThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if !samePath(filepath.Join(dir, "r.out"), filepath.Join(link, "r.out")) {
		t.Error("a symlinked folder hid that both outputs are the same file")
	}
	sub := filepath.Join(dir, "actual", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(sub, alias); err != nil {
		t.Fatal(err)
	}
	if !samePath(filepath.Join(dir, "actual", "r.out"), alias+"/../r.out") {
		t.Error(`"alias/.." should resolve through the symlink before going up`)
	}
}
