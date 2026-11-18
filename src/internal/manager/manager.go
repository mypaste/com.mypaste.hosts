// Package manager is the hosts extension's one owner of an edit: it reads the
// file, refuses to overwrite a file someone else changed, keeps the previous
// bytes in the extension's private storage, writes, and reads the result back.
package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mypaste/com.mypaste.hosts/src/internal/hostsfile"
	"github.com/mypaste/com.mypaste.hosts/src/internal/provider"
)

// MaxBackups is how many previous versions the private storage keeps. The
// oldest goes when a sixth is written.
const MaxBackups = 5

// BackupKeyPrefix holds every backup the extension writes.
const BackupKeyPrefix = "backups/"

// Backup is one stored previous version.
type Backup struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	At   string `json:"at"`
}

// Snapshot is what the page renders: the file as it is now, plus how the last
// write went.
type Snapshot struct {
	Path               string            `json:"path"`
	Digest             string            `json:"digest"`
	Entries            []hostsfile.Entry `json:"entries"`
	Groups             []string          `json:"groups"`
	OtherLines         int               `json:"otherLines"`
	NeedsAuthorization bool              `json:"needsAuthorization"`
	Backups            []Backup          `json:"backups,omitempty"`
	BackupError        string            `json:"backupError,omitempty"`
	Written            string            `json:"written,omitempty"`
	Changed            bool              `json:"changed"`
}

// Failure is a code the page can act on plus a sentence it can show.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Err     error  `json:"-"`
}

func (f *Failure) Error() string { return f.Message }
func (f *Failure) Unwrap() error { return f.Err }

func failf(code, format string, args ...any) *Failure {
	return &Failure{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Failure codes.
const (
	CodeRead      = "read"
	CodeInvalid   = "invalid"
	CodeStale     = "stale"
	CodeBackup    = "backup"
	CodeWrite     = "write"
	CodeCancelled = "cancelled"
)

// Files is the disk side: the provider in production, a temp file in tests.
type Files interface {
	Path() string
	Read() ([]byte, error)
	Write(ctx context.Context, data []byte) (provider.Mode, error)
	NeedsAuthorization() bool
}

// Backups is the extension's private storage, reached through the host.
type Backups interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	List(ctx context.Context) ([]Backup, error)
	Delete(ctx context.Context, key string) error
}

// Manager ties the file, its grammar and the backup store together.
type Manager struct {
	Files   Files
	Backups Backups
	Now     func() time.Time
}

// Load reads the file and the backup list. A backup-store failure does not
// hide the file: the page can still edit it, and the snapshot says why the
// restore list is empty.
func (m *Manager) Load(ctx context.Context) (Snapshot, error) {
	current, err := m.Files.Read()
	if err != nil {
		return Snapshot{}, failf(CodeRead, "%v", err)
	}
	document, err := hostsfile.Parse(current)
	if err != nil {
		return Snapshot{}, failf(CodeRead, "%v", err)
	}
	backups, backupErr := m.Backups.List(ctx)
	snapshot := Snapshot{
		Path:               m.Files.Path(),
		Digest:             document.Digest(),
		Entries:            document.Entries(),
		Groups:             document.Groups(),
		OtherLines:         document.OtherLines(),
		NeedsAuthorization: m.Files.NeedsAuthorization(),
		Backups:            backups,
	}
	if backupErr != nil {
		snapshot.Backups = nil
		snapshot.BackupError = backupErr.Error()
	}
	return snapshot, nil
}

// Preview renders the submitted entries without writing anything.
func (m *Manager) Preview(entries []hostsfile.Entry, groups []hostsfile.GroupOp) (string, error) {
	current, err := m.Files.Read()
	if err != nil {
		return "", failf(CodeRead, "%v", err)
	}
	document, err := hostsfile.Parse(current)
	if err != nil {
		return "", failf(CodeRead, "%v", err)
	}
	rendered, err := document.RenderWith(entries, groups)
	if err != nil {
		return "", renderFailure(err)
	}
	return string(rendered), nil
}

// Apply writes the submitted entry list and the group changes the page made.
// baseDigest is what the page loaded: when the file on disk no longer matches
// it, nothing is written.
func (m *Manager) Apply(ctx context.Context, baseDigest string,
	entries []hostsfile.Entry, groups []hostsfile.GroupOp) (Snapshot, error) {
	current, err := m.Files.Read()
	if err != nil {
		return Snapshot{}, failf(CodeRead, "%v", err)
	}
	document, err := hostsfile.Parse(current)
	if err != nil {
		return Snapshot{}, failf(CodeRead, "%v", err)
	}
	if baseDigest != "" && document.Digest() != baseDigest {
		return Snapshot{}, failf(CodeStale,
			"%s 在打开页面后被其他程序修改，未写入；请重新加载后再改。", m.Files.Path())
	}
	next, err := document.RenderWith(entries, groups)
	if err != nil {
		return Snapshot{}, renderFailure(err)
	}
	if bytes.Equal(next, current) {
		snapshot, err := m.Load(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		return snapshot, nil
	}
	if err := m.saveBackup(ctx, current); err != nil {
		return Snapshot{}, failf(CodeBackup, "无法保存备份，未写入文件：%v", err)
	}
	mode, writeErr := m.Files.Write(ctx, next)
	if writeErr != nil {
		return Snapshot{}, m.writeFailure(ctx, current, writeErr)
	}
	snapshot, err := m.Load(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Changed = true
	snapshot.Written = string(mode)
	return snapshot, nil
}

// Revert writes a stored previous version back, newest first when key is
// empty. The version being replaced is backed up too, so a restore can be
// undone the same way.
func (m *Manager) Revert(ctx context.Context, key string) (Snapshot, error) {
	current, err := m.Files.Read()
	if err != nil {
		return Snapshot{}, failf(CodeRead, "%v", err)
	}
	backups, err := m.Backups.List(ctx)
	if err != nil {
		return Snapshot{}, failf(CodeBackup, "无法读取备份：%v", err)
	}
	if len(backups) == 0 {
		return Snapshot{}, failf(CodeBackup, "还没有可恢复的备份；每次应用前都会先存一份。")
	}
	if key == "" {
		key = backups[0].Key
	}
	stored, ok, err := m.Backups.Get(ctx, key)
	if err != nil {
		return Snapshot{}, failf(CodeBackup, "无法读取备份：%v", err)
	}
	if !ok {
		return Snapshot{}, failf(CodeBackup, "这份备份已不存在：%s", key)
	}
	if bytes.Equal(stored, current) {
		snapshot, err := m.Load(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		return snapshot, nil
	}
	if err := m.saveBackup(ctx, current); err != nil {
		return Snapshot{}, failf(CodeBackup, "无法保存当前版本，未恢复：%v", err)
	}
	mode, writeErr := m.Files.Write(ctx, stored)
	if writeErr != nil {
		return Snapshot{}, m.writeFailure(ctx, current, writeErr)
	}
	snapshot, err := m.Load(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Changed = true
	snapshot.Written = string(mode)
	return snapshot, nil
}

// MaxBackupPreviewBytes caps what one stored version may show in the page. A
// hosts file is a few kilobytes; anything far larger is not the file this
// extension wrote, and a settings tab is no place to load it.
const MaxBackupPreviewBytes = 256 << 10

// BackupView is one stored version as the page shows it: when it was taken,
// how big it is, and its exact text.
type BackupView struct {
	Key   string `json:"key"`
	At    string `json:"at"`
	Size  int64  `json:"size"`
	Text  string `json:"text"`
	Lines int    `json:"lines"`
}

// BackupText reads one stored version for the page to show. The key must be
// one the store lists, so the page can only ever look at versions this
// extension wrote itself.
func (m *Manager) BackupText(ctx context.Context, key string) (BackupView, error) {
	backups, err := m.Backups.List(ctx)
	if err != nil {
		return BackupView{}, failf(CodeBackup, "无法读取备份：%v", err)
	}
	var found *Backup
	for index := range backups {
		if backups[index].Key == key {
			found = &backups[index]
			break
		}
	}
	if found == nil {
		return BackupView{}, failf(CodeBackup, "这份备份已不存在；重新加载后再看。")
	}
	if found.Size > MaxBackupPreviewBytes {
		return BackupView{}, failf(CodeBackup,
			"这份备份有 %d KB，太大，无法在页面里预览。", found.Size/1024)
	}
	data, ok, err := m.Backups.Get(ctx, found.Key)
	if err != nil {
		return BackupView{}, failf(CodeBackup, "无法读取备份：%v", err)
	}
	if !ok {
		return BackupView{}, failf(CodeBackup, "这份备份已不存在；重新加载后再看。")
	}
	if !utf8.Valid(data) {
		return BackupView{}, failf(CodeBackup, "这份备份不是文本，无法预览。")
	}
	return BackupView{
		Key:   found.Key,
		At:    found.At,
		Size:  found.Size,
		Text:  string(data),
		Lines: textLines(data),
	}, nil
}

// textLines counts the lines a person sees: a trailing newline ends the last
// line rather than starting an empty one.
func textLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

func (m *Manager) writeFailure(ctx context.Context, previous []byte, writeErr error) error {
	if errors.Is(writeErr, provider.ErrCanceled) {
		return &Failure{Code: CodeCancelled, Message: "已取消管理员授权，文件没有改动。", Err: writeErr}
	}
	// A failed write can leave the file truncated. Put the bytes we read
	// before it back when they no longer match, and say what happened.
	if current, err := m.Files.Read(); err == nil && !bytes.Equal(current, previous) {
		if _, restoreErr := m.Files.Write(ctx, previous); restoreErr == nil {
			return failf(CodeWrite, "写入失败，已恢复原来的内容：%v", writeErr)
		}
		return failf(CodeWrite, "写入失败，且无法恢复原内容，请用备份恢复：%v", writeErr)
	}
	return failf(CodeWrite, "写入失败：%v", writeErr)
}

func renderFailure(err error) error {
	switch {
	case errors.Is(err, hostsfile.ErrStale):
		return &Failure{Code: CodeStale, Message: "文件已被修改，请重新加载后再改。", Err: err}
	case errors.Is(err, hostsfile.ErrInvalidEntry),
		errors.Is(err, hostsfile.ErrInvalidGroup):
		return &Failure{Code: CodeInvalid, Message: err.Error(), Err: err}
	default:
		return failf(CodeWrite, "%v", err)
	}
}

func (m *Manager) saveBackup(ctx context.Context, data []byte) error {
	key := m.nextBackupKey()
	if backups, err := m.Backups.List(ctx); err == nil {
		taken := make(map[string]bool, len(backups))
		for _, backup := range backups {
			taken[backup.Key] = true
		}
		stem := strings.TrimSuffix(key, ".hosts")
		for suffix := 2; taken[key]; suffix++ {
			key = stem + "-" + strconv.Itoa(suffix) + ".hosts"
		}
	}
	if err := m.Backups.Put(ctx, key, data); err != nil {
		return err
	}
	// Pruning is housekeeping: a failure never blocks the write the person
	// asked for, and the next successful write tries again.
	_ = m.pruneBackups(ctx)
	return nil
}

func (m *Manager) nextBackupKey() string {
	stamp := m.now().UTC().Format("20060102T150405Z")
	return BackupKeyPrefix + stamp + ".hosts"
}

func (m *Manager) pruneBackups(ctx context.Context) error {
	backups, err := m.Backups.List(ctx)
	if err != nil {
		return nil
	}
	if len(backups) <= MaxBackups {
		return nil
	}
	// List is newest first; the oldest live at the end.
	for _, old := range backups[MaxBackups:] {
		if err := m.Backups.Delete(ctx, old.Key); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// BackupTime reads the moment a backup key was written. Keys this package did
// not write report a zero time.
func BackupTime(key string) time.Time {
	name := strings.TrimSuffix(strings.TrimPrefix(key, BackupKeyPrefix), ".hosts")
	if dash := strings.Index(name, "-"); dash > 0 {
		name = name[:dash]
	}
	stamp, err := time.Parse("20060102T150405Z", name)
	if err != nil {
		return time.Time{}
	}
	return stamp
}

// SortBackups orders newest first; keys sort in time order inside one prefix.
func SortBackups(backups []Backup) {
	sort.SliceStable(backups, func(left, right int) bool {
		return backups[left].Key > backups[right].Key
	})
}
