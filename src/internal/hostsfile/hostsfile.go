// Package hostsfile owns the grammar of a hosts document. An entry is an
// address followed by one or more names, an optional trailing comment, and a
// leading "#" that disables it; a comment line that is not a disabled entry is
// a section heading. Every other line is kept byte for byte, so a document the
// person has not edited renders back to the same bytes.
package hostsfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// The file's own limits. They bound one parse and one render; the host caps a
// menu invoke at 1 MiB of text, so a hosts file that parses here always fits
// in the page's request.
const (
	MaxFileBytes  = 1 << 20
	MaxEntries    = 4096
	MaxNames      = 64
	MaxNameBytes  = 253
	MaxGroupBytes = 120
)

// Entry is one hosts line as the page sees it. The same shape comes back from
// the page: an ID this document does not know adds a new entry, and an
// existing ID the page omits deletes that line.
type Entry struct {
	// ID is stable only within one document, and only while the line stays in
	// the file: it is the ordinal among entry lines when the file was read.
	ID      string   `json:"id"`
	IP      string   `json:"ip"`
	Names   []string `json:"names"`
	Comment string   `json:"comment"`
	Enabled bool     `json:"enabled"`
	// Group is the heading text of the nearest section comment above the
	// entry, or "" when the line has none. A new entry uses it to choose where
	// it lands.
	Group string `json:"group"`
}

type entry struct {
	Entry
	raw string
}

// GroupOp is one change to the headings that define groups. The page makes
// them in order while the person manages the sidebar; the renderer applies
// them to the document before it places entries. A rename retitles the
// heading comment in place, so its section keeps its position in the file; a
// remove drops a heading whose section the page has already emptied.
type GroupOp struct {
	Kind  string `json:"kind"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Group string `json:"group,omitempty"`
}

// The two group changes the page can submit.
const (
	GroupRename = "rename"
	GroupRemove = "remove"
)

type line struct {
	text    string
	entry   *entry
	heading string
}

// Document is one parsed hosts file together with the bytes it came from.
type Document struct {
	newline string
	lines   []line
	entries []*entry
	other   int
	digest  string
}

// Parse reads one hosts document.
func Parse(data []byte) (*Document, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("文件超过 %d KiB，无法在页面里编辑", MaxFileBytes>>10)
	}
	document := &Document{newline: "\n"}
	sum := sha256.Sum256(data)
	document.digest = hex.EncodeToString(sum[:])
	text := string(data)
	if strings.Contains(text, "\r\n") {
		document.newline = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	group := ""
	for _, text := range lines {
		trimmed := strings.TrimSpace(text)
		switch {
		case trimmed == "":
			group = ""
			document.lines = append(document.lines, line{text: text})
		case strings.HasPrefix(trimmed, "#"):
			body := strings.TrimSpace(trimmed[1:])
			if strings.HasPrefix(body, "#") {
				// "##" and friends are decorative separators, not headings.
				body = ""
			}
			ip, names, comment, ok := parseBody(body)
			if ok {
				document.appendEntry(text, ip, names, comment, false, group)
				continue
			}
			document.other++
			group = body
			document.lines = append(document.lines, line{text: text, heading: body})
		default:
			ip, names, comment, ok := parseBody(trimmed)
			if !ok {
				document.other++
				document.lines = append(document.lines, line{text: text})
				continue
			}
			document.appendEntry(text, ip, names, comment, true, group)
		}
	}
	if len(document.entries) > MaxEntries {
		return nil, fmt.Errorf("条目超过 %d 条，无法在页面里编辑", MaxEntries)
	}
	return document, nil
}

func (d *Document) appendEntry(raw, ip string, names []string, comment string, enabled bool, group string) {
	parsed := &entry{
		Entry: Entry{
			ID:      "e" + strconv.Itoa(len(d.entries)),
			IP:      ip,
			Names:   names,
			Comment: comment,
			Enabled: enabled,
			Group:   group,
		},
		raw: raw,
	}
	d.entries = append(d.entries, parsed)
	d.lines = append(d.lines, line{text: raw, entry: parsed})
}

// Digest is the SHA-256 of the bytes the document was parsed from. The page
// sends it back with an edit so a file changed outside MyPaste is never
// overwritten silently.
func (d *Document) Digest() string { return d.digest }

// Entries returns the parsed entries in file order.
func (d *Document) Entries() []Entry {
	entries := make([]Entry, 0, len(d.entries))
	for _, parsed := range d.entries {
		copied := parsed.Entry
		copied.Names = append([]string(nil), parsed.Names...)
		entries = append(entries, copied)
	}
	return entries
}

// OtherLines counts the non-blank lines this document does not represent as
// entries: free comments, headings, and lines whose first field is not an
// address. All of them are preserved unchanged.
func (d *Document) OtherLines() int { return d.other }

// Groups lists the distinct section headings in file order.
func (d *Document) Groups() []string {
	groups := make([]string, 0, 8)
	seen := make(map[string]bool, 8)
	for _, parsed := range d.entries {
		if parsed.Group == "" || seen[parsed.Group] {
			continue
		}
		seen[parsed.Group] = true
		groups = append(groups, parsed.Group)
	}
	return groups
}

// Render applies the page's entry list to the document. Lines that are not
// entries stay where they are, an entry whose fields did not change keeps its
// exact original text, and a new entry follows the last line of the section
// it names.
func (d *Document) Render(entries []Entry) ([]byte, error) {
	return d.RenderWith(entries, nil)
}

// RenderWith is Render plus the group changes the page made before it submits
// the list. Group changes are applied first, then entries are placed as usual.
func (d *Document) RenderWith(entries []Entry, groups []GroupOp) ([]byte, error) {
	if err := d.applyGroups(groups, entries); err != nil {
		return nil, err
	}
	known := make(map[string]*entry, len(d.entries))
	for _, parsed := range d.entries {
		known[parsed.ID] = parsed
	}
	byID := make(map[string]Entry, len(entries))
	added := make([]Entry, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for index, entry := range entries {
		clean, err := validate(entry, index)
		if err != nil {
			return nil, err
		}
		if clean.ID == "" {
			added = append(added, clean)
			continue
		}
		if _, ok := known[clean.ID]; !ok {
			return nil, fmt.Errorf("%w：第 %d 条的条目编号已不存在，文件可能被其他程序修改",
				ErrStale, index+1)
		}
		if seen[clean.ID] {
			return nil, fmt.Errorf("%w：第 %d 条重复提交了同一个条目", ErrInvalidEntry, index+1)
		}
		seen[clean.ID] = true
		byID[clean.ID] = clean
	}

	type outLine struct {
		text  string
		group string
	}
	out := make([]outLine, 0, len(d.lines)+len(added))
	for _, source := range d.lines {
		if source.entry == nil {
			out = append(out, outLine{text: source.text, group: source.heading})
			continue
		}
		entry, keep := byID[source.entry.ID]
		if !keep {
			continue
		}
		out = append(out, outLine{text: renderEntry(entry, source.entry), group: source.entry.Group})
	}
	for _, entry := range added {
		at := -1
		if entry.Group != "" {
			for index := len(out) - 1; index >= 0; index-- {
				if out[index].group == entry.Group {
					at = index + 1
					break
				}
			}
		}
		if at < 0 {
			if entry.Group != "" {
				out = append(out, outLine{text: "# " + entry.Group, group: entry.Group})
			}
			at = len(out)
		}
		rendered := renderEntry(entry, nil)
		next := make([]outLine, 0, len(out)+1)
		next = append(next, out[:at]...)
		next = append(next, outLine{text: rendered, group: entry.Group})
		next = append(next, out[at:]...)
		out = next
	}

	texts := make([]string, len(out))
	for index, rendered := range out {
		texts[index] = rendered.text
	}
	joined := strings.Join(texts, d.newline)
	if joined != "" {
		joined += d.newline
	}
	if len(joined) > MaxFileBytes {
		return nil, fmt.Errorf("%w：结果超过 %d KiB", ErrInvalidEntry, MaxFileBytes>>10)
	}
	return []byte(joined), nil
}

// applyGroups rewrites the heading comments the page renamed or removed. No
// entry line moves: a rename retitles the heading above its section, and a
// remove is refused while a submitted entry still uses that group.
func (d *Document) applyGroups(ops []GroupOp, entries []Entry) error {
	for index, op := range ops {
		switch op.Kind {
		case GroupRename:
			from := strings.TrimSpace(op.From)
			to := strings.TrimSpace(op.To)
			if from == "" || to == "" {
				return groupFailure(index, "缺少分组名")
			}
			if len(to) > MaxGroupBytes {
				return groupFailure(index, fmt.Sprintf("分组名超过 %d 个字符", MaxGroupBytes))
			}
			if strings.ContainsAny(to, "\r\n#") {
				return groupFailure(index, "分组名不能包含换行或 #")
			}
			if !d.hasHeading(from) {
				return groupFailure(index, fmt.Sprintf("文件里没有「%s」这个分组", from))
			}
			if from == to {
				continue
			}
			if d.hasHeading(to) {
				return groupFailure(index, fmt.Sprintf("已经有「%s」这个分组", to))
			}
			d.renameHeading(from, to)
			for _, parsed := range d.entries {
				if parsed.Group == from {
					parsed.Group = to
				}
			}
		case GroupRemove:
			group := strings.TrimSpace(op.Group)
			if group == "" {
				return groupFailure(index, "缺少分组名")
			}
			for _, entry := range entries {
				if strings.TrimSpace(entry.Group) == group {
					return groupFailure(index, fmt.Sprintf(
						"「%s」里还有条目，先删除这些条目再删除分组", group))
				}
			}
			if !d.removeHeading(group) {
				return groupFailure(index, fmt.Sprintf("文件里没有「%s」这个分组", group))
			}
		default:
			return groupFailure(index, fmt.Sprintf("不认识的分组操作 %q", op.Kind))
		}
	}
	return nil
}

func groupFailure(index int, reason string) error {
	return fmt.Errorf("%w：第 %d 个分组操作 %s", ErrInvalidGroup, index+1, reason)
}

// hasHeading reports whether a section heading with this name is still in the
// document. A named group always has one: that comment line is what decides
// which entries belong to it.
func (d *Document) hasHeading(group string) bool {
	for _, line := range d.lines {
		if line.entry == nil && line.heading == group {
			return true
		}
	}
	return false
}

func (d *Document) renameHeading(from, to string) {
	for index, line := range d.lines {
		if line.entry != nil || line.heading != from {
			continue
		}
		d.lines[index].heading = to
		d.lines[index].text = replaceHeading(line.text, to)
	}
}

// replaceHeading keeps the line's indentation and writes the new name after
// the same single "#".
func replaceHeading(text, to string) string {
	trimmed := strings.TrimLeft(text, " \t")
	return text[:len(text)-len(trimmed)] + "# " + to
}

// removeHeading drops the heading line itself. The section's other lines and
// entries are left alone; the page submits the list without that group's
// entries before it asks for the removal.
func (d *Document) removeHeading(group string) bool {
	kept := make([]line, 0, len(d.lines))
	found := false
	for _, line := range d.lines {
		if line.entry == nil && line.heading == group {
			found = true
			d.other--
			continue
		}
		kept = append(kept, line)
	}
	d.lines = kept
	return found
}

// ErrInvalidGroup marks a group change the document refuses: a name that does
// not exist, a rename onto an existing group, or a removal that would strand
// entries.
var ErrInvalidGroup = errors.New("分组无效")

// ErrInvalidEntry marks submitted entries the document refuses; the page
// highlights the row and keeps its copy of the list.
var ErrInvalidEntry = errors.New("条目无效")

// ErrStale marks an entry list that no longer matches the file: the person
// must reload before the edit can be rebuilt.
var ErrStale = errors.New("文件已被修改")

func validate(entry Entry, index int) (Entry, error) {
	clean := Entry{
		ID:      strings.TrimSpace(entry.ID),
		IP:      strings.TrimSpace(entry.IP),
		Enabled: entry.Enabled,
		Group:   strings.TrimSpace(entry.Group),
	}
	bad := func(format string, args ...any) (Entry, error) {
		return Entry{}, fmt.Errorf("%w：第 %d 条 %s", ErrInvalidEntry, index+1,
			fmt.Sprintf(format, args...))
	}
	if net.ParseIP(clean.IP) == nil {
		return bad("地址 %q 不是 IPv4 或 IPv6 地址", entry.IP)
	}
	if len(clean.Group) > MaxGroupBytes {
		return bad("分组名超过 %d 个字符", MaxGroupBytes)
	}
	if strings.ContainsAny(clean.Group, "\r\n#") {
		return bad("分组名不能包含换行或 #")
	}
	if len(entry.Names) == 0 {
		return bad("至少需要一个主机名")
	}
	if len(entry.Names) > MaxNames {
		return bad("主机名超过 %d 个", MaxNames)
	}
	names := make([]string, 0, len(entry.Names))
	seen := make(map[string]bool, len(entry.Names))
	for _, raw := range entry.Names {
		name := strings.TrimSpace(raw)
		if !validName(name) {
			return bad("主机名 %q 无效", raw)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	clean.Names = names
	comment := strings.TrimSpace(entry.Comment)
	comment = strings.TrimSpace(strings.TrimPrefix(comment, "#"))
	if strings.ContainsAny(comment, "\r\n") || len(comment) > 200 {
		return bad("备注无效")
	}
	clean.Comment = comment
	return clean, nil
}

func renderEntry(entry Entry, original *entry) string {
	if original != nil && original.IP == entry.IP &&
		original.Comment == entry.Comment &&
		original.Enabled == entry.Enabled &&
		sameNames(original.Names, entry.Names) {
		return original.raw
	}
	rendered := entry.IP + " " + strings.Join(entry.Names, " ")
	if entry.Comment != "" {
		rendered += "  # " + entry.Comment
	}
	if !entry.Enabled {
		rendered = "# " + rendered
	}
	return rendered
}

func sameNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// parseBody splits an entry body — what is left after an optional leading "#"
// — into an address, its names and a trailing comment. A comment starts at a
// "#" that follows whitespace, so a name can never swallow the rest of the
// line.
func parseBody(body string) (ip string, names []string, comment string, ok bool) {
	if at := commentStart(body); at >= 0 {
		comment = strings.TrimSpace(body[at+1:])
		body = strings.TrimSpace(body[:at])
	}
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return "", nil, "", false
	}
	if net.ParseIP(fields[0]) == nil {
		return "", nil, "", false
	}
	names = make([]string, 0, len(fields)-1)
	for _, name := range fields[1:] {
		if !validName(name) {
			return "", nil, "", false
		}
		names = append(names, name)
	}
	return fields[0], names, comment, true
}

func commentStart(body string) int {
	for index := 1; index < len(body); index++ {
		if body[index] == '#' && (body[index-1] == ' ' || body[index-1] == '\t') {
			return index
		}
	}
	return -1
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// validName accepts the names hosts files actually carry: letters, digits and
// the dots, hyphens and underscores DNS and mDNS use, without a leading or
// trailing separator.
func validName(name string) bool {
	return len(name) <= MaxNameBytes && namePattern.MatchString(name)
}
