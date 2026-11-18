package hostsfile

import (
	"errors"
	"strings"
	"testing"
)

const sample = `##
# Host Database
#
# localhost is used to configure the loopback interface
##
127.0.0.1	localhost
255.255.255.255	broadcasthost
::1             localhost

# dev - local services
192.168.1.10 app.local api.local  # dev box
# 192.168.1.11 old.local
`

func parseSample(t *testing.T) *Document {
	t.Helper()
	document, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return document
}

func TestParseClassifiesEntriesHeadingsAndOtherLines(t *testing.T) {
	document := parseSample(t)
	entries := document.Entries()
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5: %+v", len(entries), entries)
	}
	local := entries[0]
	if local.IP != "127.0.0.1" || len(local.Names) != 1 || local.Names[0] != "localhost" ||
		!local.Enabled || local.Group != "" {
		t.Errorf("first entry = %+v", local)
	}
	if entries[1].Names[0] != "broadcasthost" || entries[1].Group != "" {
		t.Errorf("second entry = %+v", entries[1])
	}
	if entries[2].IP != "::1" || entries[2].Names[0] != "localhost" {
		t.Errorf("ipv6 entry = %+v", entries[2])
	}
	dev := entries[3]
	if dev.IP != "192.168.1.10" || dev.Group != "dev - local services" ||
		dev.Comment != "dev box" || !dev.Enabled {
		t.Errorf("dev entry = %+v", dev)
	}
	if entries[4].IP != "192.168.1.11" || entries[4].Enabled {
		t.Errorf("disabled entry = %+v", entries[4])
	}
	if groups := document.Groups(); len(groups) != 1 || groups[0] != "dev - local services" {
		t.Errorf("groups = %v", groups)
	}
	// Six comment lines: none of them is an entry, and all are preserved.
	if document.OtherLines() != 6 {
		t.Errorf("other lines = %d, want 6", document.OtherLines())
	}
	if document.Digest() == "" {
		t.Error("digest is empty")
	}
}

func TestRenderWithoutEditsKeepsEveryByte(t *testing.T) {
	document := parseSample(t)
	rendered, err := document.Render(document.Entries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(rendered) != sample {
		t.Fatalf("round trip changed the file:\n--- got ---\n%s", rendered)
	}
}

func TestRenderToggleDisablesAndEnables(t *testing.T) {
	document := parseSample(t)
	entries := document.Entries()
	entries[0].Enabled = false
	entries[4].Enabled = true
	rendered, err := document.Render(entries)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := string(rendered)
	if !strings.Contains(text, "# 127.0.0.1 localhost\n") {
		t.Errorf("disabled localhost missing:\n%s", text)
	}
	if !strings.Contains(text, "192.168.1.11 old.local\n") {
		t.Errorf("enabled old.local missing:\n%s", text)
	}
}

func TestRenderEditsNamesAndComment(t *testing.T) {
	document := parseSample(t)
	entries := document.Entries()
	entries[3].Names = []string{"app.local", "api.local", "new.local"}
	entries[3].Comment = "moved"
	rendered, err := document.Render(entries)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(rendered), "192.168.1.10 app.local api.local new.local  # moved\n") {
		t.Errorf("edited line missing:\n%s", rendered)
	}
	// The untouched entry keeps its original tab separator.
	if !strings.Contains(string(rendered), "127.0.0.1\tlocalhost\n") {
		t.Errorf("untouched line was rewritten:\n%s", rendered)
	}
}

func TestRenderDeletesOmittedEntries(t *testing.T) {
	document := parseSample(t)
	entries := document.Entries()
	kept := append([]Entry{entries[0]}, entries[2:]...)
	rendered, err := document.Render(kept)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(rendered), "broadcasthost") {
		t.Errorf("deleted entry is still there:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "old.local") {
		t.Errorf("kept entry disappeared:\n%s", rendered)
	}
}

func TestRenderAddsIntoAnExistingSection(t *testing.T) {
	document := parseSample(t)
	entries := document.Entries()
	entries = append(entries, Entry{IP: "192.168.1.12", Names: []string{"new.local"},
		Enabled: true, Group: "dev - local services"})
	rendered, err := document.Render(entries)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	lines := strings.Split(string(rendered), "\n")
	lastDev := -1
	for index, lineText := range lines {
		if strings.Contains(lineText, "192.168.1.1") {
			lastDev = index
		}
	}
	if lastDev < 0 || !strings.Contains(lines[lastDev], "192.168.1.12 new.local") {
		t.Errorf("new entry is not the last line of its section:\n%s", rendered)
	}
	if !strings.Contains(lines[lastDev-1], "192.168.1.11") {
		t.Errorf("new entry left its section:\n%s", rendered)
	}
}

func TestRenderAddsANewSectionAndAnUngroupedEntry(t *testing.T) {
	document := parseSample(t)
	entries := document.Entries()
	entries = append(entries,
		Entry{IP: "10.1.1.1", Names: []string{"staging.local"}, Enabled: true, Group: "staging"},
		Entry{IP: "10.1.1.2", Names: []string{"loose.local"}, Enabled: true},
	)
	rendered, err := document.Render(entries)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := string(rendered)
	if !strings.HasSuffix(text, "# staging\n10.1.1.1 staging.local\n10.1.1.2 loose.local\n") {
		t.Errorf("appended entries are wrong:\n%s", text)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	document := parseSample(t)
	cases := []struct {
		name    string
		entries []Entry
	}{
		{"bad ip", []Entry{{IP: "not-an-ip", Names: []string{"a.local"}}}},
		{"no names", []Entry{{IP: "10.0.0.1"}}},
		{"bad name", []Entry{{IP: "10.0.0.1", Names: []string{"a b"}}}},
		{"unknown id", []Entry{{ID: "e99", IP: "10.0.0.1", Names: []string{"a.local"}}}},
		{"duplicate id", []Entry{
			{ID: "e0", IP: "10.0.0.1", Names: []string{"a.local"}},
			{ID: "e0", IP: "10.0.0.2", Names: []string{"b.local"}},
		}},
		{"newline comment", []Entry{{IP: "10.0.0.1", Names: []string{"a.local"}, Comment: "x\ny"}}},
		{"group with hash", []Entry{{IP: "10.0.0.1", Names: []string{"a.local"}, Group: "a # b"}}},
	}
	for _, test := range cases {
		if _, err := document.Render(test.entries); err == nil {
			t.Errorf("%s: Render accepted the entry", test.name)
		}
	}
	if _, err := document.Render([]Entry{{IP: "bad", Names: []string{"a.local"}}}); !errors.Is(err, ErrInvalidEntry) {
		t.Errorf("invalid entry error does not wrap ErrInvalidEntry: %v", err)
	}
	if _, err := document.Render([]Entry{{ID: "e99", IP: "10.0.0.1", Names: []string{"a.local"}}}); !errors.Is(err, ErrStale) {
		t.Errorf("stale id was not reported as stale: %v", err)
	}
}

func TestParseKeepsNonEntryLinesAndCRLF(t *testing.T) {
	input := "# header\r\npublic-ip-abc api.example\r\n10.0.0.1 api.example\r\n"
	document, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if document.OtherLines() != 2 {
		t.Errorf("other lines = %d, want 2", document.OtherLines())
	}
	rendered, err := document.Render(document.Entries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(rendered) != input {
		t.Errorf("CRLF file changed:\n%q", rendered)
	}
	// A comment may contain a '#' when whitespace precedes the first one.
	document, err = Parse([]byte("10.0.0.1 a.example # note #2\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if entries := document.Entries(); len(entries) != 1 || entries[0].Comment != "note #2" {
		t.Errorf("comment split wrong: %+v", document.Entries())
	}
	// A "#" with no whitespace before it is not a comment: the line does not
	// parse as an entry and is preserved as a plain line.
	document, err = Parse([]byte("10.0.0.1 a#b.example\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(document.Entries()) != 0 || document.OtherLines() != 1 {
		t.Errorf("embedded # parsed as an entry: %+v", document.Entries())
	}
}

func TestParseRejectsAnOversizedFile(t *testing.T) {
	if _, err := Parse(make([]byte, MaxFileBytes+1)); err == nil {
		t.Error("Parse accepted an oversized file")
	}
}

// twoSections is the smallest file with two named groups: renaming and
// removing one must leave the other untouched.
const twoSections = `# alpha
10.0.0.1 a.local

# beta
10.0.0.2 b.local
`

func parseText(t *testing.T, text string) *Document {
	t.Helper()
	document, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return document
}

func TestRenameGroupRetitlesTheHeadingAndKeepsEveryLine(t *testing.T) {
	document := parseSample(t)
	rendered, err := document.RenderWith(document.Entries(), []GroupOp{
		{Kind: GroupRename, From: "dev - local services", To: "开发机"},
	})
	if err != nil {
		t.Fatalf("RenderWith: %v", err)
	}
	want := strings.Replace(sample, "# dev - local services", "# 开发机", 1)
	if string(rendered) != want {
		t.Errorf("rename changed more than the heading:\n%s", rendered)
	}
	// The document the page reads next reports the new name.
	if groups := document.Groups(); len(groups) != 1 || groups[0] != "开发机" {
		t.Errorf("groups = %v", groups)
	}
}

func TestRenameGroupRefusesWhatTheFileCannotAnswer(t *testing.T) {
	document := parseText(t, twoSections)
	entries := document.Entries()
	for _, test := range []struct {
		name string
		op   GroupOp
	}{
		{"missing group", GroupOp{Kind: GroupRename, From: "gamma", To: "delta"}},
		{"taken name", GroupOp{Kind: GroupRename, From: "alpha", To: "beta"}},
		{"empty name", GroupOp{Kind: GroupRename, From: "alpha", To: "  "}},
		{"hash in name", GroupOp{Kind: GroupRename, From: "alpha", To: "a # b"}},
		{"unknown kind", GroupOp{Kind: "move", From: "alpha", To: "beta"}},
	} {
		if _, err := document.RenderWith(entries, []GroupOp{test.op}); !errors.Is(err, ErrInvalidGroup) {
			t.Errorf("%s: err = %v, want ErrInvalidGroup", test.name, err)
		}
	}
}

func TestRemoveGroupDropsItsHeadingOnceItsEntriesAreGone(t *testing.T) {
	document := parseText(t, twoSections)
	kept := []Entry{}
	for _, entry := range document.Entries() {
		if entry.Group != "beta" {
			kept = append(kept, entry)
		}
	}
	rendered, err := document.RenderWith(kept, []GroupOp{
		{Kind: GroupRemove, Group: "beta"},
	})
	if err != nil {
		t.Fatalf("RenderWith: %v", err)
	}
	if strings.Contains(string(rendered), "beta") || strings.Contains(string(rendered), "b.local") {
		t.Errorf("beta survived:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "# alpha\n10.0.0.1 a.local\n") {
		t.Errorf("alpha changed:\n%s", rendered)
	}
	if document.OtherLines() != 1 {
		t.Errorf("other lines = %d, want 1 (the alpha heading)", document.OtherLines())
	}
}

func TestRemoveGroupRefusesWhileEntriesStillNameIt(t *testing.T) {
	document := parseText(t, twoSections)
	if _, err := document.RenderWith(document.Entries(), []GroupOp{
		{Kind: GroupRemove, Group: "beta"},
	}); !errors.Is(err, ErrInvalidGroup) {
		t.Errorf("err = %v, want ErrInvalidGroup", err)
	}
	document = parseText(t, twoSections)
	if _, err := document.RenderWith(nil, []GroupOp{
		{Kind: GroupRemove, Group: "gamma"},
	}); !errors.Is(err, ErrInvalidGroup) {
		t.Errorf("missing group: err = %v, want ErrInvalidGroup", err)
	}
}
