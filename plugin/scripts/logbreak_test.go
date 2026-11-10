package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLongestLiteral(t *testing.T) {
	cases := map[string]string{
		"could not open %s: %v":   "could not open",
		"%d retries left":         "retries left",
		"100%% done after %.2fs":  "done after",
		"no verbs here":           "no verbs here",
		"%s":                      "",
		"user %[1]s logged in %s": "logged in",
	}
	for in, want := range cases {
		if got := longestLiteral(in); got != want {
			t.Errorf("longestLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainsWord(t *testing.T) {
	yes := []struct{ line, w string }{
		{`| json | status >= 500`, "status"},
		{`"order_id"`, "order_id"},
		{`@user.id:42`, "user.id"},
		{`status`, "status"},
	}
	for _, c := range yes {
		if !containsWord(c.line, c.w) {
			t.Errorf("containsWord(%q, %q) = false", c.line, c.w)
		}
	}
	no := []struct{ line, w string }{
		{`http_status_code`, "status"},
		{`order_id_v2`, "order_id"},
		{`statuses`, "status"},
		{`anything`, ""},
	}
	for _, c := range no {
		if containsWord(c.line, c.w) {
			t.Errorf("containsWord(%q, %q) = true", c.line, c.w)
		}
	}
}

func TestKeyInQueryPosition(t *testing.T) {
	yes := []string{`| json | status >= 500`, `status="500"`, `{"status": 1}`, `@status:500`, `| status!="ok"`, `\"status\"=~\"5..\"`}
	for _, l := range yes {
		if !keyInQueryPosition(l, "status") {
			t.Errorf("keyInQueryPosition(%q) = false", l)
		}
	}
	no := []string{`the status of the build`, `status`, `http_status = 1`}
	for _, l := range no {
		if keyInQueryPosition(l, "status") {
			t.Errorf("keyInQueryPosition(%q) = true", l)
		}
	}
}

func TestZapFieldKind(t *testing.T) {
	cases := map[string]string{
		"String": kindStr, "Int": kindNum, "Int64": kindNum, "Uint8": kindNum, "Float64p": kindNum,
		"Ints": kindArr, "Int64s": kindArr, "Bool": kindBool, "Duration": kindDur, "Time": kindTime,
		"Error": kindStr, "Namespace": kindGrp, "Any": kindAny, "Strings": kindArr,
	}
	for name, want := range cases {
		got, ok := zapFieldKind(name)
		if !ok || got != want {
			t.Errorf("zapFieldKind(%q) = %q %v, want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"Skip", "Integer", "Intx", "Must"} {
		if _, ok := zapFieldKind(name); ok {
			t.Errorf("zapFieldKind(%q) recognized", name)
		}
	}
}

func TestReadRefRefusesDamagedFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty.yaml":     "",
		"blank.yaml":     "  \n\n",
		"binary.yaml":    "groups:\x00\x01\x02\n",
		"latin1.txt":     "caf\xe9\n",
		"cut.json":       `{"panels": [{"expr": "msg=\"payment failed\""`,
		"cut.yaml":       "groups:\n  - name: payments\n    rules:\n      - expr: msg=\"payment fa",
		"buildlog.txt":   "# example.com/m\n./main.go:3:2: undefined: foo\n",
		"testfail.txt":   "--- FAIL: TestCharge (0.00s)\nFAIL\nexit status 1\n",
		"gofail.txt":     "go: example.com/m@v1.0.0: missing go.sum entry\n",
		"panic.yaml":     "panic: runtime error: index out of range\n",
		"goroutine.yaml": "goroutine 1 [running]:\nmain.main()\n",
	}
	for name, body := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readRef(p); err == nil {
			t.Errorf("readRef(%s) accepted a damaged file", name)
		}
	}
	good := filepath.Join(dir, "ok.yaml")
	os.WriteFile(good, []byte("expr: msg=\"payment failed\" | status >= 500\n"), 0o644)
	if _, err := readRef(good); err != nil {
		t.Errorf("readRef(ok.yaml): %v", err)
	}
	goodJSON := filepath.Join(dir, "ok.json")
	os.WriteFile(goodJSON, []byte(`{"expr": "msg=\"payment failed\""}`), 0o644)
	if _, err := readRef(goodJSON); err != nil {
		t.Errorf("readRef(ok.json) without a final newline: %v", err)
	}
}

func TestFindMessageInEscapedJSON(t *testing.T) {
	rf := &refFile{path: "d.json", lines: []string{`{"expr": "{app=\"x\"} |= \"card \\\"declined\\\"\""}`}}
	if hits := findMessage([]*refFile{rf}, `card "declined"`, false); len(hits) == 0 {
		t.Error("message with quotes not found in JSON-escaped dashboard")
	}
	if hits := findMessage([]*refFile{rf}, "ok", false); len(hits) != 0 {
		t.Error("messages shorter than 3 characters must not be searched")
	}
}

func TestUntarRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/abs/evil", "a/../../evil"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		if err := untar(&buf, t.TempDir()); err == nil {
			t.Errorf("untar accepted %q", name)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "go.mod", Mode: 0o644, Size: 100, Typeflag: tar.TypeReg})
	tw.Write([]byte("module x"))
	if err := untar(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Error("untar accepted a cut-off archive")
	}
}

func TestFlagErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"--old", "a", "--base", "b"},
		{"--old", "a", "--format", "xml"},
		{"--old", "a", "--fail-on", "info"},
		{"--old", "a", "--fail-on", "break"},
		{"--list", "--old", "a"},
		{"--list", "--refs", "x"},
		{"--list", "--ignore", "x"},
		{"--old", "a", "extra"},
		{"--nope"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if out.Len() != 0 {
			t.Errorf("run(%q) printed on stdout: %q", args, out.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"--version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), version) {
		t.Errorf("--version = %d %q", code, out.String())
	}
}

func TestParseIgnoreLine(t *testing.T) {
	cases := []struct {
		line       string
		wantChange string
		wantMsg    string
		wantKey    string
		wantReason string
		wantErr    bool
	}{
		{
			line:       `message-changed "payment failed" alert updated in rules.yaml`,
			wantChange: "message-changed",
			wantMsg:    "payment failed",
			wantReason: "alert updated in rules.yaml",
		},
		{
			line:       `key-removed "payment failed" amount alert updated in rules.yaml`,
			wantChange: "key-removed",
			wantMsg:    "payment failed",
			wantKey:    "amount",
			wantReason: "alert updated in rules.yaml",
		},
		{
			line:       `key-removed "payment failed" "order_id" alert updated`,
			wantChange: "key-removed",
			wantMsg:    "payment failed",
			wantKey:    "order_id",
			wantReason: "alert updated",
		},
		{
			line:    `bad-change "msg" reason`,
			wantErr: true,
		},
		{
			line:    `message-changed`,
			wantErr: true,
		},
		{
			line:    `message-changed "unclosed reason`,
			wantErr: true,
		},
		{
			line:    `message-changed "msg"`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		rule, err := parseIgnoreLine(c.line, 1)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseIgnoreLine(%q) expected error, got nil", c.line)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseIgnoreLine(%q) unexpected error: %v", c.line, err)
		}
		if rule.change != c.wantChange || rule.msg != c.wantMsg {
			t.Errorf("parseIgnoreLine(%q) = (%q, %q), want (%q, %q)", c.line, rule.change, rule.msg, c.wantChange, c.wantMsg)
		}
		key := rule.key
		reason := rule.fullReason
		if rule.candKey != "" {
			key = rule.candKey
			reason = rule.candReason
		}
		if key != c.wantKey || reason != c.wantReason {
			t.Errorf("parseIgnoreLine(%q) key/reason = (%q, %q), want (%q, %q)", c.line, key, reason, c.wantKey, c.wantReason)
		}
	}
}
