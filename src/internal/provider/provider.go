// Package provider is the only code that touches the hosts file on disk. It
// reads with the person's ordinary permissions and writes the same way when
// the file allows it; only a permission failure falls back to one explicit
// macOS authorization prompt, and nothing is written until that prompt is
// accepted.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultPath is the file this provider manages.
const DefaultPath = "/etc/hosts"

// defaultPrompt is what macOS shows above the administrator dialog.
const defaultPrompt = "MyPaste 需要管理员权限写入 /etc/hosts"

// Mode says how one write reached the file.
type Mode string

const (
	// ModeDirect is a write the person's own permissions allowed.
	ModeDirect Mode = "direct"
	// ModeAuthorized is a write performed after the system asked for an
	// administrator's authorization.
	ModeAuthorized Mode = "authorized"
)

// ErrCanceled is the person's "cancel" in the authorization dialog. It is not
// a failure of the file; the page says so without calling it an error.
var ErrCanceled = errors.New("已取消管理员授权")

// File reads and writes one hosts document.
type File struct {
	Target string
	Prompt string
	// TempDir stages the authorized copy; empty means os.TempDir().
	TempDir string
	// Run executes one command and returns its combined output. Tests replace
	// it; production uses exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// New returns the provider for one hosts path.
func New(path string) *File {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	return &File{Target: path, Prompt: defaultPrompt, Run: runCommand}
}

// Path returns the file this provider manages.
func (f *File) Path() string { return f.Target }

// Read returns the file's current bytes.
func (f *File) Read() ([]byte, error) {
	info, err := os.Stat(f.Target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s 不存在", f.Target)
		}
		return nil, fmt.Errorf("无法读取 %s：%v", f.Target, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", f.Target)
	}
	data, err := os.ReadFile(f.Target)
	if err != nil {
		return nil, fmt.Errorf("无法读取 %s：%v", f.Target, err)
	}
	return data, nil
}

// NeedsAuthorization reports whether a direct write would be refused. It opens
// the file for writing without truncating it, so a "no" changes nothing.
func (f *File) NeedsAuthorization() bool {
	file, err := os.OpenFile(f.Target, os.O_WRONLY, 0)
	if err != nil {
		return true
	}
	return file.Close() != nil
}

// Write replaces the file's bytes and verifies the result by reading it back.
func (f *File) Write(ctx context.Context, data []byte) (Mode, error) {
	mode, err := f.write(ctx, data)
	if err != nil {
		return "", err
	}
	current, err := os.ReadFile(f.Target)
	if err != nil {
		return mode, fmt.Errorf("写入后无法读回 %s：%v", f.Target, err)
	}
	if !bytes.Equal(current, data) {
		return mode, fmt.Errorf("写入后校验失败：%s 的内容与预期不一致", f.Target)
	}
	return mode, nil
}

func (f *File) write(ctx context.Context, data []byte) (Mode, error) {
	directErr := writeDirect(f.Target, data)
	if directErr == nil {
		return ModeDirect, nil
	}
	if !errors.Is(directErr, fs.ErrPermission) {
		return "", fmt.Errorf("无法写入 %s：%v", f.Target, directErr)
	}
	if err := f.writeAuthorized(ctx, data); err != nil {
		return "", err
	}
	return ModeAuthorized, nil
}

func writeDirect(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// writeAuthorized stages the bytes in a temporary file and asks macOS, through
// one `osascript` prompt, to copy it over the hosts file as root. `cp` onto an
// existing file keeps that file's owner and mode.
func (f *File) writeAuthorized(ctx context.Context, data []byte) error {
	if f.Run == nil {
		return errors.New("没有可用的管理员授权通道")
	}
	if !safePath(f.Target) {
		return fmt.Errorf("路径 %q 不能用于管理员写入", f.Target)
	}
	directory := f.TempDir
	if directory == "" {
		directory = os.TempDir()
	}
	staged, err := os.CreateTemp(directory, "mypaste-hosts-*.txt")
	if err != nil {
		return fmt.Errorf("无法创建临时文件：%v", err)
	}
	name := staged.Name()
	defer os.Remove(name)
	if _, err := staged.Write(data); err != nil {
		staged.Close()
		return fmt.Errorf("无法写入临时文件：%v", err)
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return fmt.Errorf("无法写入临时文件：%v", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("无法写入临时文件：%v", err)
	}
	if !safePath(name) {
		return errors.New("临时文件路径不能用于管理员写入")
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("无法设置临时文件权限：%v", err)
	}
	script := "/bin/cp " + shellQuote(name) + " " + shellQuote(f.Target)
	apple := "do shell script " + appleQuote(script) +
		" with prompt " + appleQuote(f.Prompt) +
		" with administrator privileges"
	output, err := f.Run(ctx, "/usr/bin/osascript", "-e", apple)
	if err != nil {
		if cancelled(string(output)) || cancelled(err.Error()) {
			return ErrCanceled
		}
		if detail := firstLine(string(output)); detail != "" {
			return fmt.Errorf("管理员写入失败：%s", detail)
		}
		return fmt.Errorf("管理员写入失败：%v", err)
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func cancelled(text string) bool {
	return strings.Contains(text, "-128") ||
		strings.Contains(text, "User canceled") ||
		strings.Contains(text, "已取消")
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// safePath keeps a path usable inside both a shell word and an AppleScript
// string literal. The two paths here are ours, not the person's input.
func safePath(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '-' || r == '_' || r == ' ':
		default:
			return false
		}
	}
	return true
}

func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func appleQuote(text string) string {
	escaped := strings.ReplaceAll(text, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
