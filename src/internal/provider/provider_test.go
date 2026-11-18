package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	return path
}

func TestWriteDirectlyWhenTheFileAllowsIt(t *testing.T) {
	path := tempFile(t, "10.0.0.1 old.local\n", 0o644)
	file := New(path)
	if file.NeedsAuthorization() {
		t.Error("a writable file reports that it needs authorization")
	}
	mode, err := file.Write(context.Background(), []byte("10.0.0.2 new.local\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if mode != ModeDirect {
		t.Errorf("mode = %q, want %q", mode, ModeDirect)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "10.0.0.2 new.local\n" {
		t.Errorf("file = %q", data)
	}
}

func TestWriteFallsBackToOneAuthorizationPrompt(t *testing.T) {
	path := tempFile(t, "10.0.0.1 old.local\n", 0o444)
	file := New(path)
	if !file.NeedsAuthorization() {
		t.Fatal("a read-only file reports that it is writable")
	}
	var script string
	file.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/osascript" || len(args) != 2 || args[0] != "-e" {
			t.Fatalf("unexpected command %q %v", name, args)
		}
		script = args[1]
		return nil, nil
	}
	_, err := file.Write(context.Background(), []byte("10.0.0.2 new.local\n"))
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		// The fake prompt wrote nothing, so the read-back catches it.
		t.Fatalf("Write did not verify the result: %v", err)
	}
	for _, want := range []string{
		"do shell script", "/bin/cp", "'" + path + "'",
		"with administrator privileges", "MyPaste 需要管理员权限写入 /etc/hosts",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("prompt script %q is missing %q", script, want)
		}
	}
	// The staging file never outlives the write.
	if strings.Contains(script, "\n") {
		t.Errorf("prompt script carries a newline: %q", script)
	}
}

func TestWriteReportsACanceledPrompt(t *testing.T) {
	path := tempFile(t, "10.0.0.1 old.local\n", 0o444)
	file := New(path)
	file.Run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("execution error: User canceled. (-128)\n"), errors.New("exit status 1")
	}
	if _, err := file.Write(context.Background(), []byte("10.0.0.2 new.local\n")); !errors.Is(err, ErrCanceled) {
		t.Errorf("err = %v, want ErrCanceled", err)
	}
}

func TestWriteReportsAFailedPrompt(t *testing.T) {
	path := tempFile(t, "10.0.0.1 old.local\n", 0o444)
	file := New(path)
	file.Run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("execution error: cp: /etc/hosts: Operation not permitted (-60006)\n"),
			errors.New("exit status 1")
	}
	_, err := file.Write(context.Background(), []byte("10.0.0.2 new.local\n"))
	if err == nil || !strings.Contains(err.Error(), "管理员写入失败") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "60006") == false {
		t.Errorf("error dropped the command's own words: %v", err)
	}
}

func TestReadRejectsAMissingFile(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing")).Read(); err == nil {
		t.Error("Read accepted a missing file")
	}
}

func TestQuotingAndPathGuard(t *testing.T) {
	if got := shellQuote("/tmp/it's here"); got != `'/tmp/it'\''s here'` {
		t.Errorf("shellQuote = %s", got)
	}
	if got := appleQuote(`a "b" \ c`); got != `"a \"b\" \\ c"` {
		t.Errorf("appleQuote = %s", got)
	}
	if safePath("/etc/hosts") == false {
		t.Error("safePath rejected /etc/hosts")
	}
	for _, path := range []string{"", "etc/hosts", "/etc/ho'sts", `/etc/ho"sts`, "/etc/ho\\sts"} {
		if safePath(path) {
			t.Errorf("safePath accepted %q", path)
		}
	}
}
