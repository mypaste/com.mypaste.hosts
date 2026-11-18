package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mypaste/com.mypaste.hosts/src/internal/hook"
)

// These tests play the host's side of the line protocol. The manager's own
// tests use fakes; this file proves the wiring the app actually talks to: one
// invocation in, storage.plugin calls answered, one answer out.

type envelope struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result *struct {
		Output string `json:"output"`
	} `json:"result"`
	Error *failure `json:"error"`
}

// replyAnswer is the runtime's own envelope inside the output: the host's
// result record has no failure code, so the page reads it from here.
type replyAnswer struct {
	OK       bool             `json:"ok"`
	Snapshot *snapshotSummary `json:"snapshot"`
	Preview  string           `json:"preview"`
	Backup   *backupSummary   `json:"backup"`
	Error    *failure         `json:"error"`
}

type backupSummary struct {
	Key   string `json:"key"`
	At    string `json:"at"`
	Size  int64  `json:"size"`
	Text  string `json:"text"`
	Lines int    `json:"lines"`
}

type snapshotSummary struct {
	Path        string `json:"path"`
	Digest      string `json:"digest"`
	OtherLines  int    `json:"otherLines"`
	BackupError string `json:"backupError"`
	Backups     []struct {
		Key string `json:"key"`
		At  string `json:"at"`
	} `json:"backups"`
	Entries []json.RawMessage `json:"entries"`
}

// hostCallRecord is what the runtime writes when it needs the host.
type hostCallRecord struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Service string `json:"service"`
	Method  string `json:"method"`
}

// storage answers one storage.plugin call the way the host would.
type storage func(method string) (any, error)

// invoke runs one invocation and plays the host until the runtime answers.
func invoke(t *testing.T, text string, store storage) replyAnswer {
	t.Helper()
	answer, _ := invokeRaw(t, text, store)
	return answer
}

func invokeRaw(t *testing.T, text string, store storage) (replyAnswer, envelope) {
	t.Helper()
	requestIn, hostW := io.Pipe()
	hostR, runtimeW := io.Pipe()
	t.Cleanup(func() {
		requestIn.Close()
		hostW.Close()
		runtimeW.Close()
		hostR.Close()
	})
	client := hook.NewClient(requestIn, runtimeW)

	type outcome struct {
		output string
		err    error
	}
	results := make(chan outcome, 1)
	go func() {
		output, err := run(context.Background(), client, invocation{
			ID:        "test-1",
			Method:    "invoke",
			CommandID: commandRequest,
			Deadline:  time.Now().Add(time.Minute).UnixMilli(),
			Input:     &input{Type: "text/plain", Text: text},
		})
		// main writes the one response line once run returns; the test does
		// the same so the host sees the same conversation.
		if err == nil {
			write(client, response{ID: "test-1", OK: true,
				Result: &result{Output: output}})
		}
		results <- outcome{output: output, err: err}
	}()

	reader := bufio.NewReader(hostR)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("runtime closed the pipe: %v", err)
		}
		var record hostCallRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("the runtime wrote a line the host cannot parse: %v", err)
		}
		if record.Type != "hostCall" {
			var response envelope
			if err := json.Unmarshal(line, &response); err != nil {
				t.Fatalf("response line: %v", err)
			}
			if response.ID != "test-1" || !response.OK || response.Result == nil {
				t.Fatalf("response = %+v", response)
			}
			select {
			case got := <-results:
				if got.err != nil {
					t.Fatalf("run: %v", got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run did not return")
			}
			var answer replyAnswer
			if err := json.Unmarshal([]byte(response.Result.Output), &answer); err != nil {
				t.Fatalf("output is not the page's answer: %v", err)
			}
			return answer, response
		}
		if record.Service != "storage.plugin" || record.ID == "" {
			t.Fatalf("unexpected host call: %+v", record)
		}
		result, callErr := store(record.Method)
		reply := map[string]any{"type": "hostCallResult", "id": record.ID}
		if callErr != nil {
			reply["ok"] = false
			reply["error"] = callErr.Error()
		} else {
			reply["ok"] = true
			reply["result"] = result
		}
		encoded, err := json.Marshal(reply)
		if err != nil {
			t.Fatalf("encode host reply: %v", err)
		}
		if _, err := hostW.Write(append(encoded, '\n')); err != nil {
			t.Fatalf("write host reply: %v", err)
		}
	}
}

func withBackups(entries ...map[string]any) storage {
	return func(method string) (any, error) {
		if method != "list" {
			return nil, errors.New("unexpected storage method " + method)
		}
		return map[string]any{"entries": entries, "used": 0, "quota": 1 << 26}, nil
	}
}

func TestLoadReadsTheFileAndItsBackups(t *testing.T) {
	answer := invoke(t, `{"action":"load"}`, withBackups(
		map[string]any{"key": "backups/20261117T101500Z.hosts", "size": 812},
		map[string]any{"key": "notes/other.txt", "size": 3},
	))
	if !answer.OK || answer.Snapshot == nil {
		t.Fatalf("answer = %+v", answer)
	}
	if answer.Snapshot.Path != HostsPath || answer.Snapshot.Digest == "" {
		t.Errorf("snapshot = %+v", answer.Snapshot)
	}
	// Only this package's keys reach the page, and each carries its moment.
	if len(answer.Snapshot.Backups) != 1 {
		t.Fatalf("backups = %+v", answer.Snapshot.Backups)
	}
	if got := answer.Snapshot.Backups[0]; got.Key != "backups/20261117T101500Z.hosts" ||
		!strings.HasPrefix(got.At, "2026-11-17T10:15:00") {
		t.Errorf("backup = %+v", got)
	}
}

func TestLoadSurvivesAStorageFailure(t *testing.T) {
	answer := invoke(t, `{"action":"load"}`, func(string) (any, error) {
		return nil, errors.New("storage.plugin 未授权")
	})
	if !answer.OK || answer.Snapshot == nil {
		t.Fatalf("answer = %+v", answer)
	}
	if answer.Snapshot.BackupError == "" || len(answer.Snapshot.Backups) != 0 {
		t.Errorf("snapshot = %+v", answer.Snapshot)
	}
	if answer.Snapshot.Digest == "" {
		t.Error("a broken backup store hid the file")
	}
}

func TestBackupReadsOneStoredVersion(t *testing.T) {
	const key = "backups/20261117T101500Z.hosts"
	const text = "10.1.0.1 stage.local\n# 10.1.0.2 gone.local\n"
	store := func(method string) (any, error) {
		switch method {
		case "list":
			return map[string]any{"entries": []map[string]any{
				{"key": key, "size": len(text)},
			}, "used": 0, "quota": 1 << 26}, nil
		case "read":
			return map[string]any{
				"data": base64.StdEncoding.EncodeToString([]byte(text)),
			}, nil
		}
		return nil, errors.New("unexpected storage method " + method)
	}
	answer := invoke(t, `{"action":"backup","backupKey":"`+key+`"}`, store)
	if !answer.OK || answer.Backup == nil {
		t.Fatalf("answer = %+v", answer)
	}
	if answer.Backup.Text != text || answer.Backup.Lines != 2 ||
		answer.Backup.Size != int64(len(text)) {
		t.Errorf("backup = %+v", answer.Backup)
	}
}

func TestBackupRefusesAKeyTheExtensionDidNotWrite(t *testing.T) {
	answer := invoke(t, `{"action":"backup","backupKey":"secrets.json"}`,
		withBackups())
	if answer.OK || answer.Error == nil || answer.Error.Code != "backup" {
		t.Errorf("answer = %+v", answer)
	}
}

func TestPreviewRoundTripsTheFileUnchanged(t *testing.T) {
	loaded := invoke(t, `{"action":"load"}`, withBackups())
	if !loaded.OK || loaded.Snapshot == nil || len(loaded.Snapshot.Entries) == 0 {
		t.Fatalf("load = %+v", loaded)
	}
	// Submit exactly what load returned: the renderer must reproduce the file
	// byte for byte, including every line it does not model.
	asked, err := json.Marshal(map[string]any{
		"action":  "preview",
		"entries": loaded.Snapshot.Entries,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	answer := invoke(t, string(asked), withBackups())
	if !answer.OK {
		t.Fatalf("preview = %+v", answer)
	}
	current, err := os.ReadFile(HostsPath)
	if err != nil {
		t.Fatalf("read %s: %v", HostsPath, err)
	}
	if answer.Preview != string(current) {
		t.Fatalf("preview differs from %s (%d vs %d bytes)",
			HostsPath, len(answer.Preview), len(current))
	}
}

func TestUnknownActionIsAnInvalidRequest(t *testing.T) {
	answer := invoke(t, `{"action":"apply-everything"}`, withBackups())
	if answer.OK || answer.Error == nil || answer.Error.Code != "invalid" {
		t.Fatalf("answer = %+v", answer)
	}
}

func TestAnEmptyRequestNeverReachesTheFile(t *testing.T) {
	requestIn, hostW := io.Pipe()
	hostR, runtimeW := io.Pipe()
	t.Cleanup(func() {
		requestIn.Close()
		hostW.Close()
		runtimeW.Close()
		hostR.Close()
	})
	client := hook.NewClient(requestIn, runtimeW)
	_, err := run(context.Background(), client, invocation{
		ID: "test-2", Method: "invoke", CommandID: commandRequest,
		Input: &input{Type: "text/plain", Text: "   "},
	})
	if err == nil || !strings.Contains(err.Error(), "请求内容为空") {
		t.Fatalf("err = %v", err)
	}
}
