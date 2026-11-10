// Package tests runs the logbreak command against real modules, compiled by
// the go toolchain on the test machine.
package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	toolPath  string
	buildErr  error
	buildOut  []byte
)

func tool(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "logbreak-tool")
		if err != nil {
			buildErr = err
			return
		}
		exe := ""
		if runtime.GOOS == "windows" {
			exe = ".exe"
		}
		toolPath = filepath.Join(dir, "logbreak"+exe)
		cmd := exec.Command("go", "build", "-o", toolPath, "./plugin/scripts")
		cmd.Dir = ".."
		buildOut, buildErr = cmd.CombinedOutput()
	})
	if buildErr != nil {
		t.Fatalf("building logbreak: %v\n%s", buildErr, buildOut)
	}
	return toolPath
}

type result struct {
	stdout, stderr string
	code           int
}

func runIn(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(tool(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{out.String(), errb.String(), code}
}

func runTool(t *testing.T, args ...string) result {
	t.Helper()
	return runIn(t, "", nil, args...)
}

func example(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module example.com/m\n\ngo 1.22\n"
	}
	writeFiles(t, dir, files)
	return dir
}

type report struct {
	OldEvents    int      `json:"old_events"`
	NewEvents    int      `json:"new_events"`
	Libraries    []string `json:"libraries"`
	RefFiles     []string `json:"ref_files"`
	NotChecked   []string `json:"not_checked"`
	StaleIgnores []string `json:"stale_ignores"`
	Breaks       int      `json:"breaks"`
	Warnings     int      `json:"warnings"`
	Infos        int      `json:"infos"`
	Ignored      int      `json:"ignored"`
	Findings     []struct {
		Severity     string   `json:"severity"`
		Change       string   `json:"change"`
		Message      string   `json:"message"`
		Key          string   `json:"key"`
		Detail       string   `json:"detail"`
		Pos          string   `json:"pos"`
		Refs         []string `json:"refs"`
		Ignored      bool     `json:"ignored"`
		IgnoreReason string   `json:"ignore_reason"`
	} `json:"findings"`
}

func parseReport(t *testing.T, r result) report {
	t.Helper()
	var rep report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("output isn't JSON: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	return rep
}

// changes lists findings as "severity change message key".
func changes(rep report) []string {
	var out []string
	for _, f := range rep.Findings {
		s := f.Severity + " " + f.Change + " " + f.Message
		if f.Key != "" {
			s += " " + f.Key
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestExampleBreak(t *testing.T) {
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--refs", example(t, "alerts"), "--format", "json")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", r.code, r.stdout, r.stderr)
	}
	got := changes(parseReport(t, r))
	want := []string{
		"break key-renamed invalid amount order_id",
		"break kind-changed refund provider error status",
		"break level-lowered refund provider error",
		"break message-changed payment failed",
		"info message-added charge timing",
		"warn key-removed payment captured elapsed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without refs nothing is a BREAK, and the same changes are warnings.
	r = runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--format", "json")
	rep := parseReport(t, r)
	if r.code != 1 || rep.Breaks != 0 || rep.Warnings != 5 {
		t.Errorf("without refs: exit %d, %d breaks, %d warnings; want 1, 0, 5", r.code, rep.Breaks, rep.Warnings)
	}
	// --fail-on never reports and exits 0.
	r = runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--fail-on", "never")
	if r.code != 0 || !strings.Contains(r.stdout, "5 warnings") {
		t.Errorf("--fail-on never: exit %d\n%s", r.code, r.stdout)
	}
}

func TestExampleRefactorIsQuiet(t *testing.T) {
	for _, failOn := range []string{"warn", "break"} {
		r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v1-refactor"), "--refs", example(t, "alerts"), "--fail-on", failOn, "--format", "json")
		if r.code != 0 {
			t.Fatalf("--fail-on %s: exit %d, want 0\n%s%s", failOn, r.code, r.stdout, r.stderr)
		}
		rep := parseReport(t, r)
		if rep.Breaks+rep.Warnings != 0 {
			t.Errorf("refactor reported %v", changes(rep))
		}
		if got := changes(rep); len(got) != 1 || got[0] != "info key-added payment captured currency" {
			t.Errorf("refactor findings: %v", got)
		}
	}
}

func TestExamplesAgainstThemselves(t *testing.T) {
	for _, name := range []string{"checkout-v1", "checkout-v2", "checkout-v1-refactor"} {
		r := runTool(t, "--old", example(t, name), "--new", example(t, name), "--fail-on", "warn")
		if r.code != 0 || !strings.Contains(r.stdout, "no changes to existing log messages") {
			t.Errorf("%s against itself: exit %d\n%s%s", name, r.code, r.stdout, r.stderr)
		}
	}
}

func TestList(t *testing.T) {
	r := runTool(t, "--list", "--new", example(t, "checkout-v1"))
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	for _, s := range []string{`"payment failed" [error, slog]`, "  attempt: number", "  elapsed: duration", "  at payments/charge.go:29"} {
		if !strings.Contains(r.stdout, s) {
			t.Errorf("--list output lacks %q:\n%s", s, r.stdout)
		}
	}
	r = runTool(t, "--list", "--new", example(t, "checkout-v1"), "--format", "json")
	var list []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list) != 5 {
		t.Errorf("--list --format json: %v, %d events\n%s", err, len(list), r.stdout)
	}
}

// damagedModules are inputs that must never compare as clean.
func damagedModules(t *testing.T) map[string]string {
	t.Helper()
	good, err := os.ReadFile(filepath.Join(example(t, "checkout-v1"), "payments", "charge.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(good)
	mk := func(files map[string]string) string { return writeModule(t, files) }
	cutAt := strings.Index(src, "s.log.Info(\"payment captured\"")
	buildLog := "# example.com/checkout/payments\npayments/charge.go:26:3: undefined: slogg\n"
	testLog := "--- FAIL: TestCharge (0.00s)\n    charge_test.go:12: got 1, want 2\nFAIL\nexit status 1\nFAIL\texample.com/checkout/payments\t0.004s\n"
	return map[string]string{
		"missing directory":     filepath.Join(t.TempDir(), "nope"),
		"empty directory":       t.TempDir(),
		"empty go.mod":          mk(map[string]string{"go.mod": "", "payments/charge.go": src}),
		"garbage go.mod":        mk(map[string]string{"go.mod": "\x00\x01garbage{{{\n", "payments/charge.go": src}),
		"cut-off go.mod":        mk(map[string]string{"go.mod": "module example.com/chec", "payments/charge.go": src}),
		"build log as go.mod":   mk(map[string]string{"go.mod": buildLog, "payments/charge.go": src}),
		"no packages":           mk(map[string]string{}),
		"empty Go file":         mk(map[string]string{"payments/charge.go": ""}),
		"garbage Go file":       mk(map[string]string{"payments/charge.go": "\x7fELF\x02\x01\x01garbage"}),
		"cut-off Go file":       mk(map[string]string{"payments/charge.go": src[:cutAt+20]}),
		"build log as Go file":  mk(map[string]string{"payments/charge.go": buildLog}),
		"test log as Go file":   mk(map[string]string{"payments/charge.go": testLog}),
		"type error":            mk(map[string]string{"payments/charge.go": strings.Replace(src, "cents <= 0", "cents <= \"0\"", 1)}),
		"no log calls":          mk(map[string]string{"p/p.go": "package p\n\nfunc F() int { return 1 }\n"}),
		"only dynamic messages": mk(map[string]string{"p/p.go": "package p\n\nimport \"log/slog\"\n\nfunc F(m string) { slog.Info(m) }\n"}),
	}
}

func TestDamagedSidesAreRefused(t *testing.T) {
	real := example(t, "checkout-v1")
	for name, dir := range damagedModules(t) {
		for _, which := range []string{"old", "new"} {
			oldDir, newDir := dir, real
			if which == "new" {
				oldDir, newDir = real, dir
			}
			for _, extra := range [][]string{nil, {"--fail-on", "never"}, {"--fail-on", "warn"}, {"--format", "json"}, {"--refs", example(t, "alerts"), "--fail-on", "break"}} {
				args := append([]string{"--old", oldDir, "--new", newDir}, extra...)
				r := runTool(t, args...)
				if r.code != 2 || r.stdout != "" {
					t.Errorf("%s on the %s side %v: exit %d, want 2 with no stdout\nstdout: %s\nstderr: %s", name, which, extra, r.code, r.stdout, r.stderr)
				}
				if r.code == 2 && !strings.Contains(r.stderr, "can't be trusted") {
					t.Errorf("%s on the %s side: stderr doesn't say the result can't be trusted: %s", name, which, r.stderr)
				}
			}
		}
		if name == "cut-off go.mod" {
			continue // still a valid go.mod for another module path; --list of it is correct
		}
		r := runTool(t, "--list", "--new", dir)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("--list on %s: exit %d, want 2\n%s", name, r.code, r.stdout)
		}
	}
}

// A file cut off between two functions still compiles. The lost calls must
// be reported, never a clean result.
func TestCutBetweenFunctionsIsReported(t *testing.T) {
	good, _ := os.ReadFile(filepath.Join(example(t, "checkout-v1"), "payments", "charge.go"))
	src := string(good)
	cut := src[:strings.Index(src, "// Refund refunds an order.")]
	dir := writeModule(t, map[string]string{"go.mod": "module example.com/checkout\n\ngo 1.22\n", "payments/charge.go": cut})
	for _, which := range []string{"old", "new"} {
		oldDir, newDir := example(t, "checkout-v1"), dir
		if which == "old" {
			oldDir, newDir = dir, example(t, "checkout-v1")
		}
		r := runTool(t, "--old", oldDir, "--new", newDir, "--format", "json")
		rep := parseReport(t, r)
		if which == "new" && (r.code != 1 || rep.Warnings != 2) {
			t.Errorf("cut new side: exit %d, %v", r.code, changes(rep))
		}
		if which == "old" && (r.code != 0 || rep.Infos != 2) {
			t.Errorf("cut old side: exit %d, %v", r.code, changes(rep))
		}
	}
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", dir, "--refs", example(t, "alerts"), "--fail-on", "break")
	if r.code != 1 || !strings.Contains(r.stdout, `BREAK message-removed "refund provider error"`) {
		t.Errorf("cut new side with refs: exit %d\n%s", r.code, r.stdout)
	}
}

func TestDamagedRefsAreRefused(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"empty.yaml":       "",
		"garbage.yaml":     "\x89PNG\r\n\x1a\n\x00\x00garbage",
		"words.yaml":       "lorem ipsum dolor sit amet\nconsectetur adipiscing elit\n",
		"cut.json":         `{"panels": [{"targets": [{"expr": "{app=\"checkout\"} | json | msg=\"invalid amount\"`,
		"cut.yaml":         "groups:\n  - name: payments\n    rules:\n      - expr: msg=\"payment failed\" | status >= 50",
		"buildlog.txt":     "# example.com/checkout/payments\npayments/charge.go:26:3: undefined: slogg\n",
		"testlog.txt":      "--- FAIL: TestCharge (0.00s)\n    charge_test.go:12: payment failed\nFAIL\nexit status 1\n",
		"golist.txt":       "go: example.com/checkout@v1.0.0: missing go.sum entry for go.mod file\n",
		"emptydir/.keep":   "",
		"unrelated/a.yaml": "groups:\n  - name: other\n    rules:\n      - expr: up == 0\n",
	}
	writeFiles(t, dir, files)
	oldDir, newDir := example(t, "checkout-v1"), example(t, "checkout-v2")
	refs := []string{"empty.yaml", "garbage.yaml", "words.yaml", "cut.json", "cut.yaml", "buildlog.txt", "testlog.txt", "golist.txt", "emptydir", "unrelated", "missing.yaml"}
	for _, ref := range refs {
		for _, failOn := range []string{"break", "warn", "never"} {
			r := runTool(t, "--old", oldDir, "--new", newDir, "--refs", filepath.Join(dir, ref), "--fail-on", failOn)
			if r.code != 2 || r.stdout != "" {
				t.Errorf("--refs %s --fail-on %s: exit %d, want 2\nstdout: %s\nstderr: %s", ref, failOn, r.code, r.stdout, r.stderr)
			}
		}
		// A damaged ref next to a good one is still refused.
		r := runTool(t, "--old", oldDir, "--new", newDir, "--refs", example(t, "alerts"), "--refs", filepath.Join(dir, ref), "--fail-on", "break")
		if r.code != 2 {
			t.Errorf("--refs alerts --refs %s: exit %d, want 2\n%s", ref, r.code, r.stderr)
		}
	}
	// The quiet refactor must not hide a damaged ref either.
	r := runTool(t, "--old", oldDir, "--new", example(t, "checkout-v1-refactor"), "--refs", filepath.Join(dir, "cut.json"), "--fail-on", "break")
	if r.code != 2 {
		t.Errorf("refactor with a cut-off ref: exit %d, want 2", r.code)
	}
}

func TestRefsDirectorySkipsUnrelatedFiles(t *testing.T) {
	dir := copyDir(t, example(t, "alerts"))
	writeFiles(t, dir, map[string]string{"README.md": "Alert rules for the checkout service.\n", ".git/HEAD": "\x00binary"})
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--refs", dir, "--fail-on", "break", "--format", "json")
	rep := parseReport(t, r)
	if r.code != 1 || rep.Breaks != 4 || len(rep.RefFiles) != 3 {
		t.Errorf("exit %d, %d breaks, ref files %v", r.code, rep.Breaks, rep.RefFiles)
	}
}

func fakeGo(t *testing.T, script string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake go command is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func TestBadGoListOutputIsRefused(t *testing.T) {
	scripts := map[string]string{
		"prints nothing":       "exit 0\n",
		"garbage":              "printf '\\x00\\x01 garbage {{{'\n",
		"cut-off JSON":         "printf '{\"ImportPath\": \"example.com/checkout/pay'\n",
		"build log":            "echo '# example.com/checkout/payments'; echo 'payments/charge.go:26:3: undefined: slogg'\n",
		"fails":                "echo 'go: something broke' >&2; exit 1\n",
		"missing export data":  "printf '{\"ImportPath\":\"example.com/checkout/payments\",\"Dir\":\"/nonexistent\",\"Export\":\"/nonexistent/x.a\",\"CompiledGoFiles\":[\"/nonexistent/a.go\"],\"Module\":{\"Path\":\"example.com/checkout\",\"Main\":true}}'\n",
		"only dependencies":    "printf '{\"ImportPath\":\"fmt\",\"Export\":\"/x\",\"DepOnly\":true}'\n",
		"package with error":   "printf '{\"ImportPath\":\"example.com/checkout/payments\",\"Error\":{\"Err\":\"boom\"}}'\n",
		"no export, has files": "printf '{\"ImportPath\":\"example.com/checkout/payments\",\"CompiledGoFiles\":[\"a.go\"],\"Module\":{\"Path\":\"example.com/checkout\",\"Main\":true}}'\n",
	}
	for name, script := range scripts {
		env := fakeGo(t, script)
		for _, extra := range [][]string{nil, {"--fail-on", "never"}} {
			args := append([]string{"--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2")}, extra...)
			r := runIn(t, "", env, args...)
			if r.code != 2 || r.stdout != "" {
				t.Errorf("fake go %s %v: exit %d, want 2\nstdout: %s\nstderr: %s", name, extra, r.code, r.stdout, r.stderr)
			}
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestBaseFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	writeFiles(t, repo, map[string]string{"README": "x\n"})
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "no module yet")
	git(t, repo, "tag", "nomodule")
	mod := filepath.Join(repo, "svc")
	for _, f := range []string{"go.mod", "payments/charge.go"} {
		data, _ := os.ReadFile(filepath.Join(example(t, "checkout-v1"), f))
		writeFiles(t, mod, map[string]string{f: string(data)})
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "v1")
	git(t, repo, "tag", "v1")

	r := runIn(t, mod, nil, "--base", "HEAD")
	if r.code != 0 {
		t.Errorf("HEAD against an unchanged tree: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	data, _ := os.ReadFile(filepath.Join(example(t, "checkout-v2"), "payments", "charge.go"))
	writeFiles(t, mod, map[string]string{"payments/charge.go": string(data)})
	r = runIn(t, mod, nil, "--base", "v1", "--refs", example(t, "alerts"), "--fail-on", "break")
	if r.code != 1 || !strings.Contains(r.stdout, "logbreak v1 -> .") || !strings.Contains(r.stdout, "4 breaking") {
		t.Errorf("--base v1 against changed tree: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, ref := range []string{"nope", "--help", "nomodule"} {
		r = runIn(t, mod, nil, "--base", ref)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("--base %s: exit %d, want 2\n%s", ref, r.code, r.stderr)
		}
	}
	r = runIn(t, t.TempDir(), nil, "--base", "HEAD")
	if r.code != 2 {
		t.Errorf("--base outside git: exit %d, want 2", r.code)
	}
}

func TestOwnModuleIsRefused(t *testing.T) {
	// This repo has no log calls, so comparing it is untrusted, not clean.
	r := runTool(t, "--old", "..", "--new", "..")
	if r.code != 2 || !strings.Contains(r.stderr, "no slog, zap or logr calls") {
		t.Errorf("own module: exit %d\n%s", r.code, r.stderr)
	}
}

func TestIgnoreMatchingLine(t *testing.T) {
	ignoreDir := t.TempDir()
	ignoreFile := filepath.Join(ignoreDir, "logbreak.ignore")
	os.WriteFile(ignoreFile, []byte("message-changed \"payment failed\" alert updated in rules.yaml\n"), 0o644)

	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--refs", example(t, "alerts"), "--ignore", ignoreFile, "--format", "json")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", r.code, r.stdout, r.stderr)
	}
	rep := parseReport(t, r)
	var found bool
	for _, f := range rep.Findings {
		if f.Message == "payment failed" && f.Change == "message-changed" {
			found = true
			if !f.Ignored {
				t.Errorf("finding %q was not ignored in JSON", f.Message)
			}
			if f.IgnoreReason != "alert updated in rules.yaml" {
				t.Errorf("finding %q ignore_reason = %q, want 'alert updated in rules.yaml'", f.Message, f.IgnoreReason)
			}
		}
	}
	if !found {
		t.Errorf("finding 'payment failed' message-changed not found in report")
	}

	// Test text format output contains "(ignored: alert updated in rules.yaml)"
	rText := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--refs", example(t, "alerts"), "--ignore", ignoreFile)
	if !strings.Contains(rText.stdout, "(ignored: alert updated in rules.yaml)") {
		t.Errorf("text output missing ignore notice:\n%s", rText.stdout)
	}
}

func TestIgnoreStaleLine(t *testing.T) {
	ignoreDir := t.TempDir()
	ignoreFile := filepath.Join(ignoreDir, "logbreak.ignore")
	os.WriteFile(ignoreFile, []byte("message-changed \"nonexistent message\" old change\n"), 0o644)

	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--refs", example(t, "alerts"), "--ignore", ignoreFile, "--format", "json")
	rep := parseReport(t, r)
	if len(rep.StaleIgnores) == 0 {
		t.Errorf("expected stale ignore in report, got none")
	} else if !strings.Contains(rep.StaleIgnores[0], "nonexistent message") {
		t.Errorf("expected stale ignore mentioning 'nonexistent message', got %v", rep.StaleIgnores)
	}

	// Text mode test
	rText := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--refs", example(t, "alerts"), "--ignore", ignoreFile)
	if !strings.Contains(rText.stdout, "stale ignore:") || !strings.Contains(rText.stdout, "nonexistent message") {
		t.Errorf("text output missing stale ignore report:\n%s", rText.stdout)
	}
}

func TestIgnoreMalformedFile(t *testing.T) {
	ignoreDir := t.TempDir()
	badFile := filepath.Join(ignoreDir, "malformed.ignore")
	os.WriteFile(badFile, []byte("bad-change-type \"foo\" some reason\n"), 0o644)

	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--ignore", badFile)
	if r.code != 2 {
		t.Errorf("malformed ignore file: exit %d, want 2\n%s", r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("malformed ignore file produced stdout: %s", r.stdout)
	}
}
