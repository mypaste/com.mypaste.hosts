package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mypaste/com.mypaste.hosts/src/internal/hostsfile"
	"github.com/mypaste/com.mypaste.hosts/src/internal/provider"
)

const startingFile = `# local development
10.0.0.1 dev.local api.local
# 10.0.0.2 old.local

# staging
10.1.0.1 stage.local
`

type memoryFiles struct {
	path  string
	data  []byte
	write func(data []byte) (provider.Mode, error)
}

func (f *memoryFiles) Path() string             { return f.path }
func (f *memoryFiles) Read() ([]byte, error)    { return append([]byte(nil), f.data...), nil }
func (f *memoryFiles) NeedsAuthorization() bool { return false }
func (f *memoryFiles) Write(_ context.Context, data []byte) (provider.Mode, error) {
	if f.write != nil {
		return f.write(data)
	}
	f.data = append(f.data[:0], data...)
	return provider.ModeDirect, nil
}

type memoryBackups struct {
	files   map[string][]byte
	listErr error
	putErr  error
}

func newBackups() *memoryBackups { return &memoryBackups{files: map[string][]byte{}} }

func (b *memoryBackups) Put(_ context.Context, key string, data []byte) error {
	if b.putErr != nil {
		return b.putErr
	}
	b.files[key] = append([]byte(nil), data...)
	return nil
}

func (b *memoryBackups) Get(_ context.Context, key string) ([]byte, bool, error) {
	data, ok := b.files[key]
	return data, ok, nil
}

func (b *memoryBackups) List(context.Context) ([]Backup, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	backups := make([]Backup, 0, len(b.files))
	for key, data := range b.files {
		backups = append(backups, Backup{Key: key, Size: int64(len(data)),
			At: BackupTime(key).Format(time.RFC3339)})
	}
	SortBackups(backups)
	return backups, nil
}

func (b *memoryBackups) Delete(_ context.Context, key string) error {
	delete(b.files, key)
	return nil
}

func newManager(t *testing.T) (*Manager, *memoryFiles, *memoryBackups) {
	t.Helper()
	files := &memoryFiles{path: "/etc/hosts", data: []byte(startingFile)}
	backups := newBackups()
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	return &Manager{Files: files, Backups: backups, Now: func() time.Time {
		at = at.Add(time.Minute)
		return at
	}}, files, backups
}

func entriesOf(t *testing.T, m *Manager) []hostsfile.Entry {
	t.Helper()
	document, err := hostsfile.Parse(m.Files.(*memoryFiles).data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return document.Entries()
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a manager failure: %v", err)
	}
	return failure.Code
}

func TestLoadDescribesTheFile(t *testing.T) {
	m, _, backups := newManager(t)
	if err := backups.Put(context.Background(), "backups/20261116T090000Z.hosts", []byte("old\n")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snapshot.Path != "/etc/hosts" || snapshot.Digest == "" {
		t.Errorf("snapshot = %+v", snapshot)
	}
	if len(snapshot.Entries) != 3 {
		t.Errorf("entries = %d, want 3", len(snapshot.Entries))
	}
	if strings.Join(snapshot.Groups, ",") != "local development,staging" {
		t.Errorf("groups = %v", snapshot.Groups)
	}
	// Two comment lines are preserved text, not entries.
	if snapshot.OtherLines != 2 {
		t.Errorf("other lines = %d, want 2", snapshot.OtherLines)
	}
	if len(snapshot.Backups) != 1 || snapshot.Backups[0].Key != "backups/20261116T090000Z.hosts" {
		t.Errorf("backups = %+v", snapshot.Backups)
	}
}

func TestLoadSurvivesAnUnreadableBackupStore(t *testing.T) {
	m, _, backups := newManager(t)
	backups.listErr = errors.New("storage.plugin 未授权")
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snapshot.Backups) != 0 || snapshot.BackupError == "" {
		t.Errorf("snapshot = %+v", snapshot)
	}
	if len(snapshot.Entries) != 3 {
		t.Errorf("the file was hidden by the backup failure")
	}
}

func TestApplyWritesAndKeepsThePreviousFile(t *testing.T) {
	m, files, backups := newManager(t)
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[0].Enabled = false
	entries = append(entries, hostsfile.Entry{IP: "10.2.0.1", Names: []string{"new.local"},
		Enabled: true, Group: "staging"})
	applied, err := m.Apply(context.Background(), snapshot.Digest, entries, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied.Changed || applied.Written != string(provider.ModeDirect) {
		t.Errorf("applied = %+v", applied)
	}
	if !strings.Contains(string(files.data), "# 10.0.0.1 dev.local api.local\n") {
		t.Errorf("toggle missing:\n%s", files.data)
	}
	if !strings.Contains(string(files.data), "10.2.0.1 new.local\n") {
		t.Errorf("new entry missing:\n%s", files.data)
	}
	if len(backups.files) != 1 {
		t.Fatalf("backups = %d, want 1", len(backups.files))
	}
	for _, stored := range backups.files {
		if string(stored) != startingFile {
			t.Errorf("backup is not the previous file:\n%s", stored)
		}
	}
	// The page gets the new digest, so a second edit builds on this write.
	if applied.Digest == snapshot.Digest {
		t.Error("digest did not change after the write")
	}
}

func TestApplyRefusesAStaleDigest(t *testing.T) {
	m, files, backups := newManager(t)
	entries := entriesOf(t, m)
	files.data = []byte(startingFile + "10.9.9.9 elsewhere.local\n")
	_, err := m.Apply(context.Background(), "not-the-digest", entries, nil)
	if code := codeOf(t, err); code != CodeStale {
		t.Errorf("code = %q, want %q", code, CodeStale)
	}
	if len(backups.files) != 0 {
		t.Error("a stale apply stored a backup")
	}
	if !strings.Contains(string(files.data), "elsewhere.local") {
		t.Error("a stale apply rewrote the file")
	}
}

func TestApplyWithNoChangesDoesNotWriteOrBackUp(t *testing.T) {
	m, files, backups := newManager(t)
	writeCalls := 0
	files.write = func(data []byte) (provider.Mode, error) {
		writeCalls++
		files.data = append(files.data[:0], data...)
		return provider.ModeDirect, nil
	}
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	applied, err := m.Apply(context.Background(), snapshot.Digest, snapshot.Entries, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.Changed || writeCalls != 0 || len(backups.files) != 0 {
		t.Errorf("unchanged apply wrote: %+v calls=%d backups=%d", applied, writeCalls, len(backups.files))
	}
}

func TestApplyReportsAnInvalidEntry(t *testing.T) {
	m, files, _ := newManager(t)
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[0].IP = "localhost"
	_, err = m.Apply(context.Background(), snapshot.Digest, entries, nil)
	if code := codeOf(t, err); code != CodeInvalid {
		t.Errorf("code = %q, want %q", code, CodeInvalid)
	}
	if !strings.Contains(err.Error(), "第 1 条") {
		t.Errorf("error does not name the row: %v", err)
	}
	if string(files.data) != startingFile {
		t.Error("an invalid apply changed the file")
	}
}

func TestApplyRefusesToWriteWithoutABackup(t *testing.T) {
	m, files, backups := newManager(t)
	backups.putErr = errors.New("插件私有存储超过 50 MiB 配额")
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[0].Enabled = false
	_, err = m.Apply(context.Background(), snapshot.Digest, entries, nil)
	if code := codeOf(t, err); code != CodeBackup {
		t.Errorf("code = %q, want %q", code, CodeBackup)
	}
	if string(files.data) != startingFile {
		t.Error("the file was written without a backup")
	}
}

func TestApplyReportsACancelledPrompt(t *testing.T) {
	m, files, _ := newManager(t)
	files.write = func([]byte) (provider.Mode, error) { return "", provider.ErrCanceled }
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[0].Enabled = false
	_, err = m.Apply(context.Background(), snapshot.Digest, entries, nil)
	if code := codeOf(t, err); code != CodeCancelled {
		t.Errorf("code = %q, want %q", code, CodeCancelled)
	}
}

func TestApplyRestoresThePreviousBytesAfterAPartialWrite(t *testing.T) {
	m, files, _ := newManager(t)
	calls := 0
	files.write = func(data []byte) (provider.Mode, error) {
		calls++
		if calls == 1 {
			files.data = []byte("truncated")
			return "", errors.New("写入中途失败")
		}
		files.data = append(files.data[:0], data...)
		return provider.ModeDirect, nil
	}
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[0].Enabled = false
	_, err = m.Apply(context.Background(), snapshot.Digest, entries, nil)
	if err == nil || !strings.Contains(err.Error(), "已恢复原来的内容") {
		t.Fatalf("err = %v", err)
	}
	if string(files.data) != startingFile {
		t.Errorf("file = %q", files.data)
	}
}

func TestRevertRestoresTheNewestBackup(t *testing.T) {
	m, files, backups := newManager(t)
	ctx := context.Background()
	snapshot, err := m.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[1].Enabled = true
	if _, err := m.Apply(ctx, snapshot.Digest, entries, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(files.data) == startingFile {
		t.Fatal("the apply did not change the file")
	}
	reverted, err := m.Revert(ctx, "")
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if string(files.data) != startingFile {
		t.Errorf("revert did not restore the original:\n%s", files.data)
	}
	if !reverted.Changed {
		t.Error("revert did not report a change")
	}
	// The version revert replaced was backed up as well, so it can be undone.
	if len(backups.files) != 2 {
		t.Errorf("backups = %d, want 2", len(backups.files))
	}
}

func TestRevertNeedsABackup(t *testing.T) {
	m, _, _ := newManager(t)
	_, err := m.Revert(context.Background(), "")
	if code := codeOf(t, err); code != CodeBackup {
		t.Errorf("code = %q, want %q", code, CodeBackup)
	}
}

func TestRevertRefusesAnUnknownKey(t *testing.T) {
	m, _, backups := newManager(t)
	backups.files["backups/20261116T090000Z.hosts"] = []byte("old\n")
	_, err := m.Revert(context.Background(), "backups/20261116T100000Z.hosts")
	if code := codeOf(t, err); code != CodeBackup {
		t.Errorf("code = %q, want %q", code, CodeBackup)
	}
}

func TestBackupTextShowsOneStoredVersion(t *testing.T) {
	m, _, backups := newManager(t)
	const stored = "10.1.0.1 stage.local\n# 10.1.0.2 gone.local\n"
	backups.files["backups/20261116T090000Z.hosts"] = []byte(stored)
	view, err := m.BackupText(context.Background(), "backups/20261116T090000Z.hosts")
	if err != nil {
		t.Fatalf("BackupText: %v", err)
	}
	if view.Text != stored {
		t.Errorf("text = %q", view.Text)
	}
	if view.Lines != 2 {
		t.Errorf("lines = %d, want 2", view.Lines)
	}
	if view.Size != int64(len(stored)) {
		t.Errorf("size = %d, want %d", view.Size, len(stored))
	}
	if !strings.HasPrefix(view.At, "2026-11-16T09:00:00") {
		t.Errorf("at = %q", view.At)
	}
}

func TestBackupTextCountsALastLineWithoutNewline(t *testing.T) {
	m, _, backups := newManager(t)
	backups.files["backups/20261116T090000Z.hosts"] = []byte("a\nb")
	view, err := m.BackupText(context.Background(), "backups/20261116T090000Z.hosts")
	if err != nil {
		t.Fatalf("BackupText: %v", err)
	}
	if view.Lines != 2 {
		t.Errorf("lines = %d, want 2", view.Lines)
	}
}

func TestBackupTextRefusesAKeyTheStoreDoesNotList(t *testing.T) {
	// The page names a key and the store answers: only a version the store
	// lists is readable, so an absent key is refused before anything is read.
	m, _, backups := newManager(t)
	backups.files["backups/20261116T090000Z.hosts"] = []byte("old\n")
	_, err := m.BackupText(context.Background(), "backups/20261116T100000Z.hosts")
	if code := codeOf(t, err); code != CodeBackup {
		t.Errorf("code = %q, want %q", code, CodeBackup)
	}
}

func TestBackupTextRefusesABinaryOrOversizedVersion(t *testing.T) {
	m, _, backups := newManager(t)
	ctx := context.Background()
	backups.files["backups/20261116T090000Z.hosts"] = []byte{0xff, 0xfe, 0x00}
	if _, err := m.BackupText(ctx, "backups/20261116T090000Z.hosts"); codeOf(t, err) != CodeBackup {
		t.Errorf("binary version: code = %q", codeOf(t, err))
	}
	backups.files["backups/20261116T100000Z.hosts"] =
		bytes.Repeat([]byte("x"), MaxBackupPreviewBytes+1)
	if _, err := m.BackupText(ctx, "backups/20261116T100000Z.hosts"); codeOf(t, err) != CodeBackup {
		t.Errorf("oversized version: code = %q", codeOf(t, err))
	}
}

func TestBackupsArePrunedToTheNewestFive(t *testing.T) {
	m, _, backups := newManager(t)
	ctx := context.Background()
	for index := 0; index < 7; index++ {
		entries := entriesOf(t, m)
		if len(entries) > 0 {
			entries[0].Enabled = !entries[0].Enabled
		}
		entries = append(entries, hostsfile.Entry{
			IP: fmt.Sprintf("10.9.0.%d", index+1), Names: []string{"x.local"}, Enabled: true})
		if _, err := m.Apply(ctx, "", entries, nil); err != nil {
			t.Fatalf("Apply %d: %v", index, err)
		}
	}
	if len(backups.files) != MaxBackups {
		t.Fatalf("backups = %d, want %d", len(backups.files), MaxBackups)
	}
	list, err := backups.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if BackupTime(list[0].Key).Before(BackupTime(list[len(list)-1].Key)) {
		t.Error("backups are not newest first")
	}
}

func TestPreviewDoesNotWrite(t *testing.T) {
	m, files, backups := newManager(t)
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := snapshot.Entries
	entries[0].Enabled = false
	text, err := m.Preview(entries, nil)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !strings.Contains(text, "# 10.0.0.1 dev.local api.local\n") {
		t.Errorf("preview = %q", text)
	}
	if string(files.data) != startingFile || len(backups.files) != 0 {
		t.Error("preview wrote something")
	}
}

func TestBackupTimeReadsTheKey(t *testing.T) {
	at := BackupTime("backups/20261117T101500Z.hosts")
	if at.UTC() != time.Date(2026, 11, 17, 10, 15, 0, 0, time.UTC) {
		t.Errorf("BackupTime = %v", at)
	}
	if at := BackupTime("backups/20261117T101500Z-2.hosts"); !at.Equal(
		time.Date(2026, 11, 17, 10, 15, 0, 0, time.UTC)) {
		t.Errorf("BackupTime with a suffix = %v", at)
	}
	if at := BackupTime("other/key.hosts"); !at.IsZero() {
		t.Errorf("BackupTime of a foreign key = %v", at)
	}
}

func TestTwoBackupsInOneSecondDoNotCollide(t *testing.T) {
	m, _, backups := newManager(t)
	m.Now = func() time.Time { return time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	for index := 0; index < 2; index++ {
		entries := entriesOf(t, m)
		entries[0].Enabled = !entries[0].Enabled
		if _, err := m.Apply(ctx, "", entries, nil); err != nil {
			t.Fatalf("Apply %d: %v", index, err)
		}
	}
	if len(backups.files) != 2 {
		t.Errorf("backups = %d, want 2: %v", len(backups.files), backups.files)
	}
}

func TestApplyRenamesAGroupHeading(t *testing.T) {
	m, files, _ := newManager(t)
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	applied, err := m.Apply(context.Background(), snapshot.Digest, snapshot.Entries,
		[]hostsfile.GroupOp{{Kind: hostsfile.GroupRename, From: "staging", To: "预发布"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(string(files.data), "# staging") ||
		!strings.Contains(string(files.data), "# 预发布\n10.1.0.1 stage.local\n") {
		t.Errorf("heading not renamed:\n%s", files.data)
	}
	if len(applied.Groups) != 2 || applied.Groups[1] != "预发布" {
		t.Errorf("groups = %v", applied.Groups)
	}
}

func TestApplyRefusesAGroupRemovalThatWouldStrandEntries(t *testing.T) {
	m, files, _ := newManager(t)
	snapshot, err := m.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = m.Apply(context.Background(), snapshot.Digest, snapshot.Entries,
		[]hostsfile.GroupOp{{Kind: hostsfile.GroupRemove, Group: "staging"}})
	if code := codeOf(t, err); code != CodeInvalid {
		t.Errorf("code = %q, want %q", code, CodeInvalid)
	}
	if string(files.data) != startingFile {
		t.Error("a refused group removal wrote the file")
	}
}
