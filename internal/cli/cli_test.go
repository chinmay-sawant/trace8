package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	code := fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func TestRunVersion(t *testing.T) {
	code, out := captureStdout(t, func() int { return Run([]string{"--version"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "trace8 "+Version {
		t.Fatalf("output = %q, want %q", out, "trace8 "+Version)
	}
}

func TestRunDefaultName(t *testing.T) {
	code, out := captureStdout(t, func() int { return Run(nil) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "hello world" {
		t.Fatalf("output = %q, want %q", out, "hello world")
	}
}

func TestRunNamedArg(t *testing.T) {
	code, out := captureStdout(t, func() int { return Run([]string{"gopher"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "hello gopher" {
		t.Fatalf("output = %q, want %q", out, "hello gopher")
	}
}

func TestRunBadFlag(t *testing.T) {
	if code := Run([]string{"--nope"}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
