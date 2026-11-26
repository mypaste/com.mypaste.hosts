// Command mypaste-hosts is the hosts extension's runtime. The host starts it
// for one request, writes one invocation line, answers the storage calls it
// makes, and reads one response line (PLUGINS.md §4). Reading, parsing and
// rendering the file happen here; writing either uses the person's own
// permissions or one macOS administrator prompt, and the host never performs
// either.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/mypaste/com.mypaste.hosts/src/internal/hook"
	"github.com/mypaste/com.mypaste.hosts/src/internal/hostsfile"
	"github.com/mypaste/com.mypaste.hosts/src/internal/manager"
	"github.com/mypaste/com.mypaste.hosts/src/internal/provider"
)

// Version is the extension version written into archive manifests.
const Version = "1.3.0"

const commandRequest = "hosts.request"

// HostsPath is the one file this extension manages. It is a constant, not a
// preference: a hosts manager that could be pointed elsewhere would be a
// general file writer.
const HostsPath = provider.DefaultPath

// input is the text one invocation carries: the page's JSON request.
type input struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type invocation struct {
	ID        string `json:"id"`
	Method    string `json:"method"`
	CommandID string `json:"commandId"`
	Deadline  int64  `json:"deadlineUnixMs"`
	Input     *input `json:"input,omitempty"`
}

type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type result struct {
	Output string `json:"output,omitempty"`
}

type response struct {
	ID     string   `json:"id"`
	OK     bool     `json:"ok"`
	Result *result  `json:"result,omitempty"`
	Error  *failure `json:"error,omitempty"`
}

// request is the JSON the page sends as the command's text.
type request struct {
	Action     string              `json:"action"`
	BaseDigest string              `json:"baseDigest"`
	BackupKey  string              `json:"backupKey"`
	Entries    []hostsfile.Entry   `json:"entries"`
	Groups     []hostsfile.GroupOp `json:"groups,omitempty"`
}

// answer is the JSON the runtime puts in its output. It carries its own ok and
// failure code because the host's result record has one message and no code;
// the page needs to tell "the file changed underneath you" from "that hostname
// is invalid".
type answer struct {
	OK       bool                `json:"ok"`
	Snapshot *manager.Snapshot   `json:"snapshot,omitempty"`
	Preview  string              `json:"preview,omitempty"`
	Backup   *manager.BackupView `json:"backup,omitempty"`
	Error    *failure            `json:"error,omitempty"`
}

func main() {
	client := hook.NewClient(os.Stdin, os.Stdout)
	line, err := client.ReadRequestLine()
	if err != nil {
		os.Exit(1)
	}
	var call invocation
	if err := json.Unmarshal(line, &call); err != nil || call.ID == "" {
		os.Exit(1)
	}
	ctx, cancel := deadline(call.Deadline)
	defer cancel()
	output, runErr := run(ctx, client, call)
	if runErr != nil {
		write(client, response{ID: call.ID, OK: false,
			Error: &failure{Code: "runtime", Message: runErr.Error()}})
		return
	}
	write(client, response{ID: call.ID, OK: true, Result: &result{Output: output}})
}

func run(ctx context.Context, client *hook.Client, call invocation) (string, error) {
	// The host writes every invocation with method "invoke" (PLUGINS.md §4);
	// the menu page's request arrives as pipe input, so its type is text/plain.
	if call.Method != "invoke" || call.CommandID != commandRequest {
		return "", errors.New("这个扩展只提供 Hosts 页面使用的一个操作")
	}
	if call.Input == nil || call.Input.Type != "text/plain" || strings.TrimSpace(call.Input.Text) == "" {
		return "", errors.New("请求内容为空")
	}
	var asked request
	if err := json.Unmarshal([]byte(call.Input.Text), &asked); err != nil {
		return "", errors.New("请求内容无法解析")
	}
	hosts := &manager.Manager{
		Files:   provider.New(HostsPath),
		Backups: backupStore{client: client},
	}
	reply := answer{OK: true}
	switch asked.Action {
	case "load":
		snapshot, err := hosts.Load(ctx)
		if err != nil {
			return encode(failureOf(err))
		}
		reply.Snapshot = &snapshot
	case "preview":
		rendered, err := hosts.Preview(asked.Entries, asked.Groups)
		if err != nil {
			return encode(failureOf(err))
		}
		reply.Preview = rendered
	case "apply":
		snapshot, err := hosts.Apply(ctx, asked.BaseDigest, asked.Entries, asked.Groups)
		if err != nil {
			return encode(failureOf(err))
		}
		reply.Snapshot = &snapshot
	case "revert":
		snapshot, err := hosts.Revert(ctx, asked.BackupKey)
		if err != nil {
			return encode(failureOf(err))
		}
		reply.Snapshot = &snapshot
	case "backup":
		// Reading one stored version back is a look, not a write: the page
		// shows it and the person decides whether to restore it.
		view, err := hosts.BackupText(ctx, asked.BackupKey)
		if err != nil {
			return encode(failureOf(err))
		}
		reply.Backup = &view
	default:
		return encode(answer{OK: false, Error: &failure{
			Code: manager.CodeInvalid, Message: "未知的操作：" + asked.Action}})
	}
	return encode(reply)
}

// failureOf turns any error into the page's failure record. The manager's
// wrapped error never reaches the page; only its code and sentence do.
func failureOf(err error) answer {
	var managerFailure *manager.Failure
	if errors.As(err, &managerFailure) {
		return answer{OK: false, Error: &failure{
			Code: managerFailure.Code, Message: managerFailure.Message}}
	}
	return answer{OK: false, Error: &failure{
		Code: manager.CodeWrite, Message: err.Error()}}
}

func encode(value answer) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("无法编码结果")
	}
	return string(encoded), nil
}

func write(client *hook.Client, payload response) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		os.Exit(1)
	}
	if err := client.WriteLine(encoded); err != nil {
		os.Exit(1)
	}
}

// deadline keeps two seconds of the host's budget for the final line, which
// carries the outcome the page reads. A write that waits on the administrator
// dialog runs under the host's long-running budget.
func deadline(ms int64) (context.Context, context.CancelFunc) {
	if ms <= 0 {
		return context.WithTimeout(context.Background(), 10*time.Minute)
	}
	limit := time.Until(time.UnixMilli(ms)) - 2*time.Second
	if limit < time.Second {
		limit = time.Second
	}
	return context.WithTimeout(context.Background(), limit)
}

// backupStore adapts the host's private storage to the manager's backup list.
type backupStore struct {
	client *hook.Client
}

func (b backupStore) Put(ctx context.Context, key string, data []byte) error {
	return b.client.Write(ctx, key, data)
}

func (b backupStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return b.client.Read(ctx, key)
}

func (b backupStore) Delete(ctx context.Context, key string) error {
	return b.client.Delete(ctx, key)
}

func (b backupStore) List(ctx context.Context) ([]manager.Backup, error) {
	listing, err := b.client.List(ctx)
	if err != nil {
		return nil, err
	}
	backups := make([]manager.Backup, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		if !strings.HasPrefix(entry.Key, manager.BackupKeyPrefix) ||
			!strings.HasSuffix(entry.Key, ".hosts") {
			continue
		}
		backup := manager.Backup{Key: entry.Key, Size: entry.Size}
		if at := manager.BackupTime(entry.Key); !at.IsZero() {
			backup.At = at.Format(time.RFC3339)
		}
		backups = append(backups, backup)
	}
	manager.SortBackups(backups)
	return backups, nil
}
