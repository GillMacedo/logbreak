// Command logbreak compares the log events a Go module writes at two
// revisions and reports changes that can silently break log-based alerts,
// dashboards and saved searches: log messages that were removed or reworded,
// attribute keys that were removed or renamed, values whose kind changed,
// and levels that went up or down.
//
// It reads log/slog, go.uber.org/zap (Logger and SugaredLogger) and
// github.com/go-logr/logr calls. Each side is loaded with `go list -export`,
// and the module's own packages are type-checked from source, so a call is
// recognized by its real type, not by the name of a variable. It never runs
// the code it checks.
//
// With --refs it searches alert rules, dashboards or saved queries for the
// changed messages and keys, and a change that one of them mentions is a
// BREAK.
//
// Exit codes: 0 no finding at or above --fail-on, 1 findings, 2 bad usage or
// an input that can't be trusted (a side that doesn't build, a side with no
// log calls, or a --refs file that is empty, cut off, not text, or mentions
// nothing from the old side). Exit 2 ignores --fail-on.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const version = "0.1.0"

const (
	sevInfo  = "info"
	sevWarn  = "warn"
	sevBreak = "break"
)

func sevRank(s string) int {
	switch s {
	case sevBreak:
		return 3
	case sevWarn:
		return 2
	case sevInfo:
		return 1
	}
	return 0
}

// untrusted marks an error that means the comparison can't be trusted.
type untrusted struct{ msg string }

func (u *untrusted) Error() string { return u.msg }

func distrust(format string, a ...any) error { return &untrusted{fmt.Sprintf(format, a...)} }

type options struct {
	oldDir, newDir string
	base           string
	tags           string
	pkgs           []string
	refs           []string
	ignoreFile     string
	ignoreRules    []*ignoreRule
	format         string
	failOn         string
	list           bool
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("logbreak", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var o options
	var pkgs, refs stringList
	fl.StringVar(&o.oldDir, "old", "", "module directory of the old revision")
	fl.StringVar(&o.newDir, "new", "", "module directory of the new revision (default: current directory)")
	fl.StringVar(&o.base, "base", "", "git ref for the old revision; the module is read from that commit with git archive")
	fl.StringVar(&o.tags, "tags", "", "build tags passed to go list, comma separated")
	fl.Var(&pkgs, "pkg", "package pattern to check, relative to the module (repeatable, default ./...)")
	fl.Var(&refs, "refs", "file or directory of alert rules, dashboards or saved queries to search for changed messages and keys (repeatable)")
	fl.StringVar(&o.ignoreFile, "ignore", "", "file of accepted changes to ignore")
	fl.StringVar(&o.format, "format", "text", "output format: text or json")
	fl.StringVar(&o.failOn, "fail-on", "warn", "lowest severity that exits 1: break, warn or never")
	fl.BoolVar(&o.list, "list", false, "print the log events of --new (or the current directory) and exit")
	showVersion := fl.Bool("version", false, "print the version and exit")
	fl.Usage = func() {
		fmt.Fprintf(stderr, "usage: logbreak (--old DIR | --base GITREF) [--new DIR] [--refs PATH]... [--ignore FILE]\n")
		fmt.Fprintf(stderr, "                [--pkg PATTERN]... [--tags T,...] [--format text|json] [--fail-on break|warn|never]\n")
		fmt.Fprintf(stderr, "       logbreak --list [--new DIR] [--pkg PATTERN]... [--tags T,...] [--format text|json]\n\n")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "logbreak", version)
		return 0
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "logbreak: unexpected argument %q\n", fl.Arg(0))
		return 2
	}
	o.pkgs, o.refs = pkgs, refs
	if o.format != "text" && o.format != "json" {
		fmt.Fprintf(stderr, "logbreak: --format must be text or json, not %q\n", o.format)
		return 2
	}
	if o.failOn != sevBreak && o.failOn != sevWarn && o.failOn != "never" {
		fmt.Fprintf(stderr, "logbreak: --fail-on must be break, warn or never, not %q\n", o.failOn)
		return 2
	}
	if o.newDir == "" {
		o.newDir = "."
	}
	if o.list {
		if o.oldDir != "" || o.base != "" || len(o.refs) > 0 || o.ignoreFile != "" {
			fmt.Fprintln(stderr, "logbreak: --list reads one side; use --new DIR, not --old, --base, --refs or --ignore")
			return 2
		}
		if err := listEvents(o, stdout); err != nil {
			fmt.Fprintf(stderr, "logbreak: %v\n", err)
			fmt.Fprintln(stderr, "logbreak: no result: the input can't be trusted")
			return 2
		}
		return 0
	}
	if o.failOn == sevBreak && len(o.refs) == 0 {
		fmt.Fprintln(stderr, "logbreak: --fail-on break needs --refs: only a change that a ref file mentions is a BREAK, so without refs the run could never fail")
		return 2
	}
	if (o.oldDir == "") == (o.base == "") {
		fmt.Fprintln(stderr, "logbreak: give exactly one of --old DIR or --base GITREF")
		return 2
	}
	if o.ignoreFile != "" {
		rules, err := loadIgnoreFile(o.ignoreFile)
		if err != nil {
			fmt.Fprintf(stderr, "logbreak: %v\n", err)
			return 2
		}
		o.ignoreRules = rules
	}
	rep, err := compare(o)
	if err != nil {
		fmt.Fprintf(stderr, "logbreak: %v\n", err)
		var u *untrusted
		if errors.As(err, &u) {
			fmt.Fprintln(stderr, "logbreak: no result: the comparison can't be trusted")
		}
		return 2
	}
	if o.format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "logbreak: %v\n", err)
			return 2
		}
	} else {
		printText(stdout, rep)
	}
	if o.failOn == "never" {
		return 0
	}
	for _, f := range rep.Findings {
		if !f.Ignored && sevRank(f.Severity) >= sevRank(o.failOn) {
			return 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Loading a side.

type side struct {
	label   string
	dir     string
	modPath string
	fset    *token.FileSet
	calls   []*call
	dynamic []*call // calls whose message isn't a constant
	libs    map[string]int

	checkWrites map[*ast.CallExpr][]*ast.CallExpr // zap Logger.Check call -> CheckedEntry.Write calls
}

type listPkg struct {
	Dir             string
	ImportPath      string
	Name            string
	Export          string
	CompiledGoFiles []string
	DepOnly         bool
	Module          *struct {
		Path string
		Main bool
	}
	Error *struct {
		Pos string
		Err string
	}
	DepsErrors []*struct {
		Pos string
		Err string
	}
}

var goCmd = "go"

func loadSide(label, dir string, o options) (*side, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, distrust("%s side: %v", label, err)
	}
	if !st.IsDir() {
		return nil, distrust("%s side: %s is not a directory", label, dir)
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		return nil, distrust("%s side: no go.mod in %s; point it at the module root", label, dir)
	}
	patterns := o.pkgs
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	args := []string{"list", "-e", "-export", "-compiled", "-deps",
		"-json=Dir,ImportPath,Name,Export,CompiledGoFiles,DepOnly,Module,Error,DepsErrors"}
	if o.tags != "" {
		args = append(args, "-tags", o.tags)
	}
	args = append(args, "--")
	args = append(args, patterns...)
	cmd := exec.Command(goCmd, args...)
	cmd.Dir = abs
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, distrust("%s side doesn't build: go list failed: %v\n%s", label, err, firstLines(errb.String(), 12))
	}
	dec := json.NewDecoder(&out)
	exports := map[string]string{}
	var targets []listPkg
	var problems []string
	for {
		var p listPkg
		err := dec.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, distrust("%s side: go list printed output that isn't valid JSON: %v", label, err)
		}
		if p.ImportPath == "" {
			return nil, distrust("%s side: go list printed a package with no import path", label)
		}
		if p.Error != nil {
			problems = append(problems, fmt.Sprintf("%s: %s", p.ImportPath, oneLine(p.Error.Err)))
		}
		for _, de := range p.DepsErrors {
			problems = append(problems, fmt.Sprintf("%s: %s", p.ImportPath, oneLine(de.Err)))
		}
		if p.ImportPath == "unsafe" {
			continue
		}
		if p.Export == "" && p.Error == nil && !p.DepOnly && len(p.CompiledGoFiles) == 0 {
			continue // a directory with only _test.go files
		}
		if p.Export == "" && p.Error == nil {
			problems = append(problems, fmt.Sprintf("%s: go list gave no export data", p.ImportPath))
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly {
			targets = append(targets, p)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		problems = dedupe(problems)
		if len(problems) > 8 {
			problems = append(problems[:8], fmt.Sprintf("and %d more", len(problems)-8))
		}
		return nil, distrust("%s side doesn't build:\n  %s", label, strings.Join(problems, "\n  "))
	}
	if len(targets) == 0 {
		return nil, distrust("%s side: go list matched no packages in %s", label, dir)
	}
	s := &side{label: label, dir: abs, fset: token.NewFileSet(), libs: map[string]int{}}
	lookup := func(p string) (io.ReadCloser, error) {
		f, ok := exports[p]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(f)
	}
	imp := importer.ForCompiler(s.fset, "gc", lookup)
	checked := 0
	for _, p := range targets {
		if p.Module == nil || !p.Module.Main {
			continue
		}
		if s.modPath == "" {
			s.modPath = p.Module.Path
		} else if s.modPath != p.Module.Path {
			continue
		}
		if err := s.checkPackage(p, imp); err != nil {
			return nil, err
		}
		checked++
	}
	if checked == 0 {
		return nil, distrust("%s side: no packages of the main module matched", label)
	}
	return s, nil
}

func (s *side) checkPackage(p listPkg, imp types.Importer) error {
	var files []*ast.File
	for _, name := range p.CompiledGoFiles {
		fn := name
		if !filepath.IsAbs(fn) {
			fn = filepath.Join(p.Dir, fn)
		}
		f, err := parser.ParseFile(s.fset, fn, nil, parser.SkipObjectResolution)
		if err != nil {
			return distrust("%s side: can't parse %s: %v", s.label, fn, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
		Defs:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: imp}
	if _, err := conf.Check(p.ImportPath, s.fset, files, info); err != nil {
		return distrust("%s side: can't type-check %s: %v", s.label, p.ImportPath, err)
	}
	pkgRel := relPkg(s.modPath, p.ImportPath)
	s.checkWrites = zapCheckWrites(files, info)
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				// Package-level var initializers can log too.
				ast.Inspect(d, func(n ast.Node) bool {
					if ce, ok := n.(*ast.CallExpr); ok {
						s.visit(ce, info, pkgRel, "init")
					}
					return true
				})
				continue
			}
			fn := funcName(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if ce, ok := n.(*ast.CallExpr); ok {
					s.visit(ce, info, pkgRel, fn)
				}
				return true
			})
		}
	}
	return nil
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	star := ""
	if se, ok := t.(*ast.StarExpr); ok {
		star = "*"
		t = se.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	name := "?"
	if id, ok := t.(*ast.Ident); ok {
		name = id.Name
	}
	return "(" + star + name + ")." + fd.Name.Name
}

// stripMajor drops a /vN major version suffix, so v1 and v2 of a module compare.
func stripMajor(mod string) string {
	i := strings.LastIndex(mod, "/v")
	if i < 0 {
		return mod
	}
	if _, err := strconv.Atoi(mod[i+2:]); err != nil {
		return mod
	}
	return mod[:i]
}

func relPkg(mod, imp string) string {
	if imp == mod {
		return "."
	}
	return strings.TrimPrefix(imp, mod+"/")
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return "  " + strings.Join(lines, "\n  ")
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	return s
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Recognizing log calls.

const (
	pkgSlog  = "log/slog"
	pkgZap   = "go.uber.org/zap"
	pkgZapC  = "go.uber.org/zap/zapcore"
	pkgLogr  = "github.com/go-logr/logr"
	kindAny  = "any"
	kindStr  = "string"
	kindNum  = "number"
	kindBool = "bool"
	kindDur  = "duration"
	kindTime = "time"
	kindGrp  = "group"
	kindArr  = "array"
)

// call is one log call site.
type call struct {
	lib     string // slog, zap, zap-sugar, logr
	msg     string
	format  bool // msg is a printf template
	level   string
	rank    int // higher is more severe; 0 when the level isn't a constant
	keys    map[string]string
	dynArgs bool // some attributes couldn't be read
	pkg     string
	fn      string
	pos     token.Position
	expr    string // the message expression, for dynamic messages
}

var levelRank = map[string]int{
	"debug": 100, "info": 200, "warn": 300, "error": 400,
	"dpanic": 500, "panic": 600, "fatal": 700,
}

func calleeFunc(ce *ast.CallExpr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch f := ast.Unparen(ce.Fun).(type) {
	case *ast.SelectorExpr:
		id = f.Sel
	case *ast.Ident:
		id = f
	case *ast.IndexExpr: // generic instantiation
		if sel, ok := f.X.(*ast.SelectorExpr); ok {
			id = sel.Sel
		}
	}
	if id == nil {
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

// recvName returns the receiver's type name for a method, or "".
func recvName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := types.Unalias(t).(*types.Named); ok {
		return n.Obj().Name()
	}
	return ""
}

func (s *side) visit(ce *ast.CallExpr, info *types.Info, pkg, fn string) {
	f := calleeFunc(ce, info)
	if f == nil || f.Pkg() == nil {
		return
	}
	path, name, recv := f.Pkg().Path(), f.Name(), recvName(f)
	var c *call
	switch {
	case path == pkgSlog && (recv == "" || recv == "Logger"):
		c = slogCall(ce, info, name)
	case path == pkgZap && recv == "Logger" && name == "Check":
		c = zapCheck(ce, info, s.checkWrites[ce])
	case path == pkgZap && recv == "Logger":
		c = zapCall(ce, info, name)
	case path == pkgZap && recv == "SugaredLogger":
		c = sugarCall(ce, info, name)
	case path == pkgLogr && recv == "Logger":
		c = logrCall(ce, info, name)
	}
	if c == nil {
		return
	}
	c.pkg, c.fn = pkg, fn
	c.pos = s.fset.Position(ce.Pos())
	if rel, err := filepath.Rel(s.dir, c.pos.Filename); err == nil && !strings.HasPrefix(rel, "..") {
		c.pos.Filename = filepath.ToSlash(rel)
	} else {
		c.pos.Filename = filepath.Base(c.pos.Filename)
	}
	if sel, ok := ast.Unparen(ce.Fun).(*ast.SelectorExpr); ok {
		withKeys(sel.X, info, c)
	}
	s.libs[c.lib]++
	if c.msg == "" && c.expr != "" {
		s.dynamic = append(s.dynamic, c)
		return
	}
	s.calls = append(s.calls, c)
}

func newCall(lib string) *call { return &call{lib: lib, keys: map[string]string{}} }

// message reads a message argument. A constant string is the message; a
// fmt.Sprintf with a constant format is a template; anything else is dynamic.
func (c *call) message(e ast.Expr, info *types.Info) {
	if s, ok := constString(e, info); ok {
		c.msg = s
		if c.msg == "" {
			c.msg = `""`
		}
		return
	}
	if ce, ok := ast.Unparen(e).(*ast.CallExpr); ok {
		if f := calleeFunc(ce, info); f != nil && f.Pkg() != nil && f.Pkg().Path() == "fmt" && f.Name() == "Sprintf" && len(ce.Args) > 0 {
			if s, ok := constString(ce.Args[0], info); ok {
				c.msg, c.format = s, true
				return
			}
		}
	}
	c.expr = exprString(e)
}

func exprString(e ast.Expr) string {
	var b bytes.Buffer
	if err := formatNode(&b, e); err != nil {
		return "?"
	}
	s := b.String()
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func formatNode(w io.Writer, e ast.Expr) error {
	return printer.Fprint(w, token.NewFileSet(), e)
}

func constString(e ast.Expr, info *types.Info) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

func constInt(e ast.Expr, info *types.Info) (int64, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil {
		return 0, false
	}
	v := constant.ToInt(tv.Value)
	if v.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(v)
}

func (c *call) setLevel(name string) {
	c.level, c.rank = name, levelRank[name]
}

func slogLevelName(n int64) string {
	switch n {
	case -4:
		return "debug"
	case 0:
		return "info"
	case 4:
		return "warn"
	case 8:
		return "error"
	}
	return fmt.Sprintf("slog level %d", n)
}

func (c *call) slogLevel(e ast.Expr, info *types.Info) {
	n, ok := constInt(e, info)
	if !ok {
		c.level = "dynamic"
		return
	}
	c.level = slogLevelName(n)
	c.rank = 200 + int(n)*25
}

var slogLevels = map[string]string{"Debug": "debug", "Info": "info", "Warn": "warn", "Error": "error"}

func slogCall(ce *ast.CallExpr, info *types.Info, name string) *call {
	c := newCall("slog")
	args := ce.Args
	base := strings.TrimSuffix(name, "Context")
	if lv, ok := slogLevels[base]; ok {
		if base != name {
			if len(args) < 2 {
				return nil
			}
			args = args[1:]
		}
		if len(args) < 1 {
			return nil
		}
		c.setLevel(lv)
		c.message(args[0], info)
		c.slogArgs(args[1:], ce.Ellipsis.IsValid(), info, "")
		return c
	}
	switch name {
	case "Log", "LogAttrs":
		if len(args) < 3 {
			return nil
		}
		c.slogLevel(args[1], info)
		c.message(args[2], info)
		c.slogArgs(args[3:], ce.Ellipsis.IsValid(), info, "")
		return c
	}
	return nil
}

func isNamed(t types.Type, pkg, name string) bool {
	if p, ok := t.(*types.Pointer); ok && name == "" {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

// slogArgs reads slog's alternating key-value arguments and Attr values.
func (c *call) slogArgs(args []ast.Expr, spread bool, info *types.Info, prefix string) {
	if spread && len(args) > 0 {
		c.dynArgs = true
		args = args[:len(args)-1]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		t := info.Types[a].Type
		if t != nil && isNamed(t, pkgSlog, "Attr") {
			c.slogAttr(a, info, prefix)
			continue
		}
		if t != nil {
			if b, ok := t.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
				k, ok := constString(a, info)
				if !ok {
					c.dynArgs = true
					i++
					continue
				}
				kind := kindAny
				if i+1 < len(args) {
					kind = valueKind(info.Types[args[i+1]].Type)
				}
				c.keys[prefix+k] = kind
				i++
				continue
			}
		}
		// slog logs this as !BADKEY; it isn't a stable key.
		c.dynArgs = true
	}
}

var slogAttrKinds = map[string]string{
	"String": kindStr, "Int": kindNum, "Int64": kindNum, "Uint64": kindNum, "Float64": kindNum,
	"Bool": kindBool, "Time": kindTime, "Duration": kindDur, "Any": kindAny, "Group": kindGrp,
}

func (c *call) slogAttr(a ast.Expr, info *types.Info, prefix string) {
	ce, ok := ast.Unparen(a).(*ast.CallExpr)
	if !ok {
		c.dynArgs = true
		return
	}
	f := calleeFunc(ce, info)
	if f == nil || f.Pkg() == nil || f.Pkg().Path() != pkgSlog || recvName(f) != "" || len(ce.Args) == 0 {
		c.dynArgs = true
		return
	}
	kind, ok := slogAttrKinds[f.Name()]
	if !ok {
		c.dynArgs = true
		return
	}
	k, ok := constString(ce.Args[0], info)
	if !ok {
		c.dynArgs = true
		return
	}
	if f.Name() == "Any" && len(ce.Args) > 1 {
		kind = valueKind(info.Types[ce.Args[1]].Type)
	}
	if f.Name() == "Group" {
		if k == "" { // an empty group key inlines its attributes
			c.slogArgs(ce.Args[1:], ce.Ellipsis.IsValid(), info, prefix)
			return
		}
		c.keys[prefix+k] = kindGrp
		c.slogArgs(ce.Args[1:], ce.Ellipsis.IsValid(), info, prefix+k+".")
		return
	}
	c.keys[prefix+k] = kind
}

var (
	errorType  = types.Universe.Lookup("error").Type()
	errorIface = errorType.Underlying().(*types.Interface)
)

// valueKind says how a Go value shows up in a log line, in broad terms.
func valueKind(t types.Type) string {
	if t == nil {
		return kindAny
	}
	if isNamed(t, "time", "Duration") {
		return kindDur
	}
	if isNamed(t, "time", "Time") {
		return kindTime
	}
	if _, ok := t.Underlying().(*types.Interface); ok {
		if types.Identical(t, errorType) || types.Identical(t, errorIface) {
			return kindStr
		}
		return kindAny
	}
	if types.Implements(t, errorIface) {
		return kindStr
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch {
		case u.Info()&types.IsString != 0:
			return kindStr
		case u.Info()&types.IsBoolean != 0:
			return kindBool
		case u.Info()&(types.IsInteger|types.IsFloat) != 0:
			return kindNum
		}
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
			return kindAny
		}
		return kindArr
	case *types.Array:
		return kindArr
	}
	return kindAny
}

var zapLevels = map[string]string{
	"Debug": "debug", "Info": "info", "Warn": "warn", "Error": "error",
	"DPanic": "dpanic", "Panic": "panic", "Fatal": "fatal",
}

var zapLevelNames = map[int64]string{-1: "debug", 0: "info", 1: "warn", 2: "error", 3: "dpanic", 4: "panic", 5: "fatal"}

func zapCall(ce *ast.CallExpr, info *types.Info, name string) *call {
	c := newCall("zap")
	args := ce.Args
	if lv, ok := zapLevels[name]; ok {
		if len(args) < 1 {
			return nil
		}
		c.setLevel(lv)
		c.message(args[0], info)
		c.zapFields(args[1:], ce.Ellipsis.IsValid(), info)
		return c
	}
	if name == "Log" && len(args) >= 2 {
		if n, ok := constInt(args[0], info); ok && zapLevelNames[n] != "" {
			c.setLevel(zapLevelNames[n])
		} else {
			c.level = "dynamic"
		}
		c.message(args[1], info)
		c.zapFields(args[2:], ce.Ellipsis.IsValid(), info)
		return c
	}
	return nil
}

// zapCheck reads `if ce := logger.Check(level, msg); ce != nil { ce.Write(fields...) }`.
func zapCheck(ce *ast.CallExpr, info *types.Info, writes []*ast.CallExpr) *call {
	if len(ce.Args) != 2 {
		return nil
	}
	c := newCall("zap")
	if n, ok := constInt(ce.Args[0], info); ok && zapLevelNames[n] != "" {
		c.setLevel(zapLevelNames[n])
	} else {
		c.level = "dynamic"
	}
	c.message(ce.Args[1], info)
	if len(writes) == 0 {
		c.dynArgs = true
	}
	for _, w := range writes {
		c.zapFields(w.Args, w.Ellipsis.IsValid(), info)
	}
	return c
}

// zapCheckWrites links each Logger.Check call assigned to a variable with the
// Write calls made on that variable.
func zapCheckWrites(files []*ast.File, info *types.Info) map[*ast.CallExpr][]*ast.CallExpr {
	checks := map[types.Object]*ast.CallExpr{}
	isCheck := func(e ast.Expr) *ast.CallExpr {
		ce, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return nil
		}
		f := calleeFunc(ce, info)
		if f == nil || f.Pkg() == nil || f.Pkg().Path() != pkgZap || f.Name() != "Check" || recvName(f) != "Logger" {
			return nil
		}
		return ce
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					if ce := isCheck(x.Rhs[0]); ce != nil {
						if id, ok := x.Lhs[0].(*ast.Ident); ok {
							obj := info.Defs[id]
							if obj == nil {
								obj = info.Uses[id]
							}
							if obj != nil {
								checks[obj] = ce
							}
						}
					}
				}
			case *ast.ValueSpec:
				if len(x.Names) == 1 && len(x.Values) == 1 {
					if ce := isCheck(x.Values[0]); ce != nil {
						if obj := info.Defs[x.Names[0]]; obj != nil {
							checks[obj] = ce
						}
					}
				}
			}
			return true
		})
	}
	out := map[*ast.CallExpr][]*ast.CallExpr{}
	if len(checks) == 0 {
		return out
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ast.Unparen(ce.Fun).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Write" {
				return true
			}
			id, ok := ast.Unparen(sel.X).(*ast.Ident)
			if !ok {
				return true
			}
			if chk, ok := checks[info.Uses[id]]; ok {
				out[chk] = append(out[chk], ce)
			}
			return true
		})
	}
	return out
}

// zapFieldKind maps zap field constructors to kinds.
func zapFieldKind(name string) (string, bool) {
	switch name {
	case "String", "Stringp", "Stringer", "ByteString", "ByteStrings", "Error", "NamedError", "Stack", "StackSkip", "Binary":
		if name == "ByteStrings" {
			return kindArr, true
		}
		return kindStr, true
	case "Bool", "Boolp":
		return kindBool, true
	case "Duration", "Durationp":
		return kindDur, true
	case "Time", "Timep":
		return kindTime, true
	case "Object", "Inline", "Dict", "Namespace":
		return kindGrp, true
	case "Any", "Reflect":
		return kindAny, true
	case "Array", "Objects", "ObjectValues", "Stringers", "Strings", "Bools", "Durations", "Times", "Errors":
		return kindArr, true
	}
	for _, p := range []string{"Int", "Uint", "Float", "Complex", "Uintptr"} {
		if strings.HasPrefix(name, p) {
			rest := strings.TrimPrefix(name, p)
			rest = strings.TrimLeft(rest, "0123456789")
			switch rest {
			case "", "p":
				return kindNum, true
			case "s":
				return kindArr, true
			}
		}
	}
	return "", false
}

func (c *call) zapFields(args []ast.Expr, spread bool, info *types.Info) {
	if spread && len(args) > 0 {
		c.dynArgs = true
		args = args[:len(args)-1]
	}
	prefix := ""
	for _, a := range args {
		ce, ok := ast.Unparen(a).(*ast.CallExpr)
		if !ok {
			c.dynArgs = true
			continue
		}
		f := calleeFunc(ce, info)
		if f == nil || f.Pkg() == nil || f.Pkg().Path() != pkgZap || recvName(f) != "" {
			c.dynArgs = true
			continue
		}
		kind, ok := zapFieldKind(f.Name())
		if !ok {
			c.dynArgs = true
			continue
		}
		var key string
		switch f.Name() {
		case "Error":
			key = "error"
		case "Errors":
			if len(ce.Args) > 0 {
				if k, ok := constString(ce.Args[0], info); ok {
					key = k
				}
			}
		case "Inline":
			continue // its keys come from MarshalLogObject
		default:
			if len(ce.Args) == 0 {
				c.dynArgs = true
				continue
			}
			k, ok := constString(ce.Args[0], info)
			if !ok {
				c.dynArgs = true
				continue
			}
			key = k
		}
		if key == "" {
			c.dynArgs = true
			continue
		}
		if f.Name() == "Any" && len(ce.Args) > 1 {
			kind = valueKind(info.Types[ce.Args[1]].Type)
		}
		c.keys[prefix+key] = kind
		if f.Name() == "Namespace" {
			prefix += key + "."
		}
	}
}

func sugarCall(ce *ast.CallExpr, info *types.Info, name string) *call {
	c := newCall("zap-sugar")
	args := ce.Args
	if len(args) < 1 {
		return nil
	}
	switch {
	case strings.HasSuffix(name, "w") && zapLevels[strings.TrimSuffix(name, "w")] != "":
		c.setLevel(zapLevels[strings.TrimSuffix(name, "w")])
		c.message(args[0], info)
		c.kvArgs(args[1:], ce.Ellipsis.IsValid(), info)
	case strings.HasSuffix(name, "f") && zapLevels[strings.TrimSuffix(name, "f")] != "":
		c.setLevel(zapLevels[strings.TrimSuffix(name, "f")])
		c.message(args[0], info)
		if c.msg != "" {
			c.format = true
		}
	case zapLevels[strings.TrimSuffix(name, "ln")] != "":
		c.setLevel(zapLevels[strings.TrimSuffix(name, "ln")])
		if len(args) == 1 {
			c.message(args[0], info)
		} else {
			c.expr = exprString(ce)
		}
	default:
		return nil
	}
	return c
}

// kvArgs reads loosely typed key-value pairs, as zap's SugaredLogger and
// logr take them.
func (c *call) kvArgs(args []ast.Expr, spread bool, info *types.Info) {
	if spread && len(args) > 0 {
		c.dynArgs = true
		args = args[:len(args)-1]
	}
	for i := 0; i < len(args); i += 2 {
		k, ok := constString(args[i], info)
		if !ok {
			// A zap.Field passed to a sugared logger is read as a field.
			if ce, isCall := ast.Unparen(args[i]).(*ast.CallExpr); isCall {
				if t := info.Types[args[i]].Type; t != nil && isNamed(t, pkgZapC, "Field") {
					c.zapFields([]ast.Expr{ce}, false, info)
					i--
					continue
				}
			}
			c.dynArgs = true
			continue
		}
		kind := kindAny
		if i+1 < len(args) {
			kind = valueKind(info.Types[args[i+1]].Type)
		}
		c.keys[k] = kind
	}
}

func logrCall(ce *ast.CallExpr, info *types.Info, name string) *call {
	c := newCall("logr")
	args := ce.Args
	switch name {
	case "Info":
		if len(args) < 1 {
			return nil
		}
		c.setLevel("info")
		if sel, ok := ast.Unparen(ce.Fun).(*ast.SelectorExpr); ok {
			if v, ok := logrV(sel.X, info); ok {
				if v != 0 {
					c.level = fmt.Sprintf("V(%d)", v)
					c.rank = 200 - int(v)
				}
			} else if v < 0 {
				c.level, c.rank = "dynamic", 0
			}
		}
		c.message(args[0], info)
		c.kvArgs(args[1:], ce.Ellipsis.IsValid(), info)
		return c
	case "Error":
		if len(args) < 2 {
			return nil
		}
		c.setLevel("error")
		c.message(args[1], info)
		c.keys["error"] = kindStr
		c.kvArgs(args[2:], ce.Ellipsis.IsValid(), info)
		return c
	}
	return nil
}

// logrV finds a V(n) call in a receiver chain. It returns (n, true) for a
// constant level, (-1, false) for a V call with a dynamic level, and
// (0, false) when there's no V call.
func logrV(e ast.Expr, info *types.Info) (int64, bool) {
	var total int64
	found := false
	for {
		ce, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			break
		}
		f := calleeFunc(ce, info)
		sel, ok := ast.Unparen(ce.Fun).(*ast.SelectorExpr)
		if f == nil || !ok || f.Pkg() == nil || f.Pkg().Path() != pkgLogr {
			break
		}
		if f.Name() == "V" && len(ce.Args) == 1 {
			n, ok := constInt(ce.Args[0], info)
			if !ok {
				return -1, false
			}
			total += n
			found = true
		}
		e = sel.X
	}
	return total, found
}

// withKeys adds keys from With calls chained directly onto the receiver,
// such as logger.With("user", id).Info(...).
func withKeys(e ast.Expr, info *types.Info, c *call) {
	for {
		ce, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return
		}
		f := calleeFunc(ce, info)
		if f == nil || f.Pkg() == nil {
			return
		}
		p, n, r := f.Pkg().Path(), f.Name(), recvName(f)
		switch {
		case p == pkgSlog && n == "With" && (r == "" || r == "Logger"):
			c.slogArgs(ce.Args, ce.Ellipsis.IsValid(), info, "")
		case p == pkgSlog && n == "WithGroup":
			// Keys after WithGroup are nested; we don't track the prefix.
			c.dynArgs = true
		case p == pkgZap && r == "Logger" && n == "With":
			c.zapFields(ce.Args, ce.Ellipsis.IsValid(), info)
		case p == pkgZap && r == "SugaredLogger" && n == "With":
			c.kvArgs(ce.Args, ce.Ellipsis.IsValid(), info)
		case p == pkgLogr && r == "Logger" && n == "WithValues":
			c.kvArgs(ce.Args, ce.Ellipsis.IsValid(), info)
		}
		sel, ok := ast.Unparen(ce.Fun).(*ast.SelectorExpr)
		if !ok {
			return
		}
		e = sel.X
	}
}

// ---------------------------------------------------------------------------
// Events: calls grouped by message.

type event struct {
	msg     string
	format  bool
	calls   []*call
	keys    map[string]map[string]bool // key -> kinds
	dynArgs bool
	rank    int // highest constant level
	level   string
	levels  []string
}

func groupEvents(calls []*call) map[string]*event {
	evs := map[string]*event{}
	for _, c := range calls {
		e := evs[c.msg]
		if e == nil {
			e = &event{msg: c.msg, keys: map[string]map[string]bool{}}
			evs[c.msg] = e
		}
		e.calls = append(e.calls, c)
		e.format = e.format || c.format
		e.dynArgs = e.dynArgs || c.dynArgs
		for k, kind := range c.keys {
			if e.keys[k] == nil {
				e.keys[k] = map[string]bool{}
			}
			e.keys[k][kind] = true
		}
		if c.rank > e.rank || e.level == "" {
			e.rank, e.level = c.rank, c.level
		}
		found := false
		for _, l := range e.levels {
			if l == c.level {
				found = true
			}
		}
		if !found {
			e.levels = append(e.levels, c.level)
		}
	}
	return evs
}

func (e *event) pos() string {
	c := e.calls[0]
	return fmt.Sprintf("%s:%d", c.pos.Filename, c.pos.Line)
}

func (e *event) funcs() map[string]bool {
	m := map[string]bool{}
	for _, c := range e.calls {
		m[c.pkg+"."+c.fn] = true
	}
	return m
}

func (e *event) kind(k string) string {
	kinds := e.keys[k]
	if len(kinds) != 1 {
		return kindAny
	}
	for kd := range kinds {
		return kd
	}
	return kindAny
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Refs: alert rules, dashboards and saved queries.

type refFile struct {
	path  string
	arg   string // the --refs argument it came from
	lines []string
}

const maxRefSize = 8 << 20

func loadRefs(paths []string) ([]*refFile, error) {
	var out []*refFile
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, distrust("--refs: %v", err)
		}
		if !st.IsDir() {
			rf, err := readRef(p)
			if err != nil {
				return nil, err
			}
			rf.arg = p
			out = append(out, rf)
			continue
		}
		n := 0
		err = filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if fp != p && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || !d.Type().IsRegular() {
				return nil
			}
			rf, err := readRef(fp)
			if err != nil {
				return err
			}
			rf.path, rf.arg = fp, p
			out = append(out, rf)
			n++
			return nil
		})
		if err != nil {
			var u *untrusted
			if errors.As(err, &u) {
				return nil, err
			}
			return nil, distrust("--refs %s: %v", p, err)
		}
		if n == 0 {
			return nil, distrust("--refs %s: the directory has no files", p)
		}
	}
	return out, nil
}

// buildOutputMarkers are lines that show a file is the output of a failed
// build or test run, not a set of queries.
var buildOutputMarkers = []string{"--- FAIL:", "FAIL\t", "panic: ", "go: ", "# command-line-arguments", "exit status ", "goroutine 1 ["}

func readRef(p string) (*refFile, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, distrust("--refs: %v", err)
	}
	if len(data) > maxRefSize {
		return nil, distrust("--refs %s: larger than %d MB; pass the rule files themselves", p, maxRefSize>>20)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, distrust("--refs %s: the file is empty", p)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, distrust("--refs %s: not a text file", p)
	}
	ext := strings.ToLower(filepath.Ext(p))
	if ext == ".json" {
		if !json.Valid(data) {
			return nil, distrust("--refs %s: not valid JSON; the file may be cut off or damaged", p)
		}
	} else if data[len(data)-1] != '\n' {
		return nil, distrust("--refs %s: the last line has no newline, so the file may be cut off; add a final newline if it's complete", p)
	}
	lines := strings.Split(string(data), "\n")
	for _, l := range lines {
		if compilerError.MatchString(l) {
			return nil, distrust("--refs %s: looks like compiler output (%q), not alert rules or queries", p, strings.TrimSpace(l))
		}
		for _, m := range buildOutputMarkers {
			if strings.HasPrefix(strings.TrimSpace(l), strings.TrimSpace(m)) && looksLikeBuildLine(l, m) {
				return nil, distrust("--refs %s: looks like build or test output (%q), not alert rules or queries", p, strings.TrimSpace(l))
			}
		}
	}
	return &refFile{path: p, lines: lines}, nil
}

// compilerError matches a go build or go vet error line, such as
// "./main.go:3:2: undefined: foo".
var compilerError = regexp.MustCompile(`^\S+\.go:\d+(:\d+)?: `)

func looksLikeBuildLine(l, marker string) bool {
	t := strings.TrimSpace(l)
	switch marker {
	case "FAIL\t":
		return strings.HasPrefix(l, "FAIL\t") || t == "FAIL"
	case "go: ":
		return strings.HasPrefix(l, "go: ")
	case "exit status ":
		rest := strings.TrimPrefix(t, "exit status ")
		_, err := strconv.Atoi(rest)
		return err == nil
	}
	return strings.HasPrefix(l, strings.TrimSpace(marker)) || strings.HasPrefix(t, marker)
}

var validChanges = map[string]bool{
	"message-changed": true,
	"message-removed": true,
	"message-added":   true,
	"key-renamed":     true,
	"key-removed":     true,
	"key-added":       true,
	"kind-changed":    true,
	"level-raised":    true,
	"level-lowered":   true,
	"level-changed":   true,
}

func isKeyChange(c string) bool {
	return strings.HasPrefix(c, "key-") || c == "kind-changed"
}

type ignoreRule struct {
	lineNum    int
	raw        string
	change     string
	msg        string
	key        string
	candKey    string
	candReason string
	fullReason string
	matched    bool
}

func loadIgnoreFile(filePath string) ([]*ignoreRule, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, distrust("--ignore %s: %v", filePath, err)
	}
	if !utf8.Valid(data) {
		return nil, distrust("--ignore %s: file is not UTF-8 text", filePath)
	}
	lines := strings.Split(string(data), "\n")
	var rules []*ignoreRule
	for i, l := range lines {
		lineNum := i + 1
		rule, err := parseIgnoreLine(l, lineNum)
		if err != nil {
			return nil, distrust("--ignore %s:%d: %v", filePath, lineNum, err)
		}
		if rule != nil {
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

func unquotePrefix(s string) (unquoted, rest string, err error) {
	if len(s) == 0 || s[0] != '"' {
		return "", s, errors.New("expected '\"'")
	}
	var buf strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		if escaped {
			switch c {
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			case '\\':
				buf.WriteByte('\\')
			case '"':
				buf.WriteByte('"')
			default:
				buf.WriteByte('\\')
				buf.WriteByte(c)
			}
			escaped = false
		} else if c == '\\' {
			escaped = true
		} else if c == '"' {
			return buf.String(), s[i+1:], nil
		} else {
			buf.WriteByte(c)
		}
	}
	return "", "", errors.New("unclosed quote")
}

func parseIgnoreLine(line string, lineNum int) (*ignoreRule, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil, nil
	}
	sp := strings.IndexAny(trimmed, " \t")
	if sp < 0 {
		return nil, errors.New("missing message and reason")
	}
	change := trimmed[:sp]
	if !validChanges[change] {
		return nil, fmt.Errorf("unknown change type %q", change)
	}
	rem := strings.TrimSpace(trimmed[sp:])
	if rem == "" {
		return nil, errors.New("missing message and reason")
	}

	var msg string
	if rem[0] == '"' {
		q, after, err := unquotePrefix(rem)
		if err != nil {
			return nil, fmt.Errorf("invalid quoted message: %v", err)
		}
		msg = q
		rem = strings.TrimSpace(after)
	} else {
		nextSp := strings.IndexAny(rem, " \t")
		if nextSp < 0 {
			return nil, errors.New("missing reason")
		}
		msg = rem[:nextSp]
		rem = strings.TrimSpace(rem[nextSp:])
	}

	if rem == "" {
		return nil, errors.New("missing reason")
	}

	rule := &ignoreRule{
		lineNum:    lineNum,
		raw:        trimmed,
		change:     change,
		msg:        msg,
		fullReason: rem,
	}

	if isKeyChange(change) {
		if rem[0] == '"' {
			q, after, err := unquotePrefix(rem)
			if err == nil && strings.TrimSpace(after) != "" {
				rule.key = q
				rule.fullReason = strings.TrimSpace(after)
				return rule, nil
			}
		}
		nextSp := strings.IndexAny(rem, " \t")
		if nextSp > 0 {
			firstWord := rem[:nextSp]
			restAfterWord := strings.TrimSpace(rem[nextSp:])
			if restAfterWord != "" {
				rule.candKey = firstWord
				rule.candReason = restAfterWord
			}
		}
	}
	return rule, nil
}

func applyIgnores(r *Report, rules []*ignoreRule) {
	if len(rules) == 0 {
		return
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		for _, rule := range rules {
			if rule.change != f.Change || rule.msg != f.Message {
				continue
			}
			matched := false
			reason := rule.fullReason
			if isKeyChange(rule.change) {
				if rule.key != "" {
					if rule.key == f.Key {
						matched = true
					}
				} else if rule.candKey != "" && rule.candKey == f.Key {
					matched = true
					reason = rule.candReason
				} else if rule.candKey == "" {
					matched = true
				}
			} else {
				matched = true
			}
			if matched {
				f.Ignored = true
				f.IgnoreReason = reason
				rule.matched = true
			}
		}
	}
	for _, rule := range rules {
		if !rule.matched {
			r.StaleIgnores = append(r.StaleIgnores, rule.raw)
		}
	}
}

func refPath(p string) string { return filepath.ToSlash(p) }

// findMessage returns the ref lines that contain a message. Templates are
// searched by their longest literal part.
func findMessage(refs []*refFile, msg string, format bool) []string {
	needle := msg
	if format {
		needle = longestLiteral(msg)
	}
	if len(strings.TrimSpace(needle)) < 3 {
		return nil
	}
	esc, _ := json.Marshal(needle)
	escNeedle := strings.Trim(string(esc), `"`)
	escNeedle2 := strings.ReplaceAll(escNeedle, `\"`, `\\\"`)
	var out []string
	for _, rf := range refs {
		for i, l := range rf.lines {
			if strings.Contains(l, needle) || strings.Contains(l, escNeedle) || strings.Contains(l, escNeedle2) {
				out = append(out, fmt.Sprintf("%s:%d", refPath(rf.path), i+1))
			}
		}
	}
	return out
}

func longestLiteral(tmpl string) string {
	best, cur := "", strings.Builder{}
	flush := func() {
		if s := strings.TrimSpace(cur.String()); len(s) > len(best) {
			best = s
		}
		cur.Reset()
	}
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] == '%' {
			flush()
			if i+1 < len(tmpl) && tmpl[i+1] == '%' {
				i++
				continue
			}
			j := i + 1
			for j < len(tmpl) && strings.IndexByte("+-# 0123456789.*[]", tmpl[j]) >= 0 {
				j++
			}
			i = j
			continue
		}
		cur.WriteByte(tmpl[i])
	}
	flush()
	return best
}

func isWordByte(b byte) bool {
	return b == '_' || b == '-' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// findKey returns the ref lines that mention a key as a whole word.
func findKey(refs []*refFile, key string) []string {
	var out []string
	for _, rf := range refs {
		for i, l := range rf.lines {
			if containsWord(l, key) {
				out = append(out, fmt.Sprintf("%s:%d", refPath(rf.path), i+1))
			}
		}
	}
	return out
}

func containsWord(l, w string) bool {
	if w == "" {
		return false
	}
	for start := 0; ; {
		i := strings.Index(l[start:], w)
		if i < 0 {
			return false
		}
		i += start
		j := i + len(w)
		if (i == 0 || !isWordByte(l[i-1])) && (j >= len(l) || !isWordByte(l[j])) {
			return true
		}
		start = i + 1
	}
}

// keyInQueryPosition reports whether the line uses the key the way a query
// filter does: followed by a comparison or a colon.
func keyInQueryPosition(l, key string) bool {
	for start := 0; ; {
		i := strings.Index(l[start:], key)
		if i < 0 {
			return false
		}
		i += start
		j := i + len(key)
		if (i == 0 || !isWordByte(l[i-1])) && (j >= len(l) || !isWordByte(l[j])) {
			rest := strings.TrimLeft(l[j:], `"\ `)
			if rest != "" && strings.IndexByte("=!<>:~", rest[0]) >= 0 {
				return true
			}
		}
		start = i + 1
	}
}

// ---------------------------------------------------------------------------
// Comparing.

type Finding struct {
	Severity     string   `json:"severity"`
	Change       string   `json:"change"`
	Message      string   `json:"message"`
	Key          string   `json:"key,omitempty"`
	Detail       string   `json:"detail"`
	Pos          string   `json:"pos"`
	Refs         []string `json:"refs,omitempty"`
	Ignored      bool     `json:"ignored,omitempty"`
	IgnoreReason string   `json:"ignore_reason,omitempty"`
}

type Report struct {
	Old          string    `json:"old"`
	New          string    `json:"new"`
	OldEvents    int       `json:"old_events"`
	NewEvents    int       `json:"new_events"`
	OldCalls     int       `json:"old_calls"`
	NewCalls     int       `json:"new_calls"`
	Libraries    []string  `json:"libraries"`
	RefFiles     []string  `json:"ref_files,omitempty"`
	Findings     []Finding `json:"findings"`
	NotChecked   []string  `json:"not_checked,omitempty"`
	StaleIgnores []string  `json:"stale_ignores,omitempty"`
	Breaks       int       `json:"breaks"`
	Warnings     int       `json:"warnings"`
	Infos        int       `json:"infos"`
	Ignored      int       `json:"ignored,omitempty"`
}

func compare(o options) (*Report, error) {
	oldDir, label := o.oldDir, o.oldDir
	if o.base != "" {
		abs, err := filepath.Abs(o.newDir)
		if err != nil {
			return nil, err
		}
		dir, cleanup, err := extractGitRev(abs, o.base)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		oldDir, label = dir, o.base
	}
	oldS, err := loadSide("old", oldDir, o)
	if err != nil {
		return nil, err
	}
	oldS.label = label
	newS, err := loadSide("new", o.newDir, o)
	if err != nil {
		return nil, err
	}
	newS.label = o.newDir
	if stripMajor(oldS.modPath) != stripMajor(newS.modPath) {
		return nil, distrust("the two sides are different modules: %s and %s; point both at the same module", oldS.modPath, newS.modPath)
	}
	if len(oldS.calls) == 0 {
		return nil, distrust("old side: no slog, zap or logr calls with a constant message in %s; nothing to compare", label)
	}
	if len(newS.calls) == 0 {
		return nil, distrust("new side: no slog, zap or logr calls with a constant message in %s; check --pkg and --tags, or the build", o.newDir)
	}
	refs, err := loadRefs(o.refs)
	if err != nil {
		return nil, err
	}
	oldEv, newEv := groupEvents(oldS.calls), groupEvents(newS.calls)
	if err := checkRefsRelevant(refs, o.refs, oldEv); err != nil {
		return nil, err
	}
	r := &Report{
		Old: label, New: o.newDir,
		OldEvents: len(oldEv), NewEvents: len(newEv),
		OldCalls: len(oldS.calls), NewCalls: len(newS.calls),
		Findings: []Finding{},
	}
	libs := map[string]bool{}
	for l := range oldS.libs {
		libs[l] = true
	}
	for l := range newS.libs {
		libs[l] = true
	}
	r.Libraries = sortedKeys(libs)
	for _, rf := range refs {
		r.RefFiles = append(r.RefFiles, refPath(rf.path))
	}
	if f := diffEvents(oldEv, newEv, refs); f != nil {
		r.Findings = f
	}
	applyIgnores(r, o.ignoreRules)
	if len(oldS.dynamic)+len(newS.dynamic) > 0 {
		ex, at := firstDynamic(newS, oldS)
		r.NotChecked = append(r.NotChecked, fmt.Sprintf("%d call sites with a message that isn't a constant (old %d, new %d), such as %s at %s",
			len(oldS.dynamic)+len(newS.dynamic), len(oldS.dynamic), len(newS.dynamic), ex, at))
	}
	dynOld, dynNew := 0, 0
	for _, e := range oldEv {
		if e.dynArgs {
			dynOld++
		}
	}
	for _, e := range newEv {
		if e.dynArgs {
			dynNew++
		}
	}
	if dynOld+dynNew > 0 {
		r.NotChecked = append(r.NotChecked, fmt.Sprintf("attributes that aren't constant keys (a spread args..., a variable, WithGroup or a helper) in %d old and %d new messages", dynOld, dynNew))
	}
	for _, f := range r.Findings {
		if f.Ignored {
			r.Ignored++
			continue
		}
		switch f.Severity {
		case sevBreak:
			r.Breaks++
		case sevWarn:
			r.Warnings++
		default:
			r.Infos++
		}
	}
	return r, nil
}

func firstDynamic(a, b *side) (string, string) {
	s := a
	if len(s.dynamic) == 0 {
		s = b
	}
	c := s.dynamic[0]
	return c.expr, fmt.Sprintf("%s:%d", c.pos.Filename, c.pos.Line)
}

// checkRefsRelevant refuses refs that mention nothing from the old side: they
// can't turn any change into a BREAK, so passing them would only look safe.
func checkRefsRelevant(refs []*refFile, given []string, oldEv map[string]*event) error {
	if len(refs) == 0 {
		return nil
	}
	keys := map[string]bool{}
	for _, e := range oldEv {
		for k := range e.keys {
			keys[k] = true
		}
	}
	mentions := func(rf *refFile) bool {
		one := []*refFile{rf}
		for _, e := range oldEv {
			if len(findMessage(one, e.msg, e.format)) > 0 {
				return true
			}
		}
		for k := range keys {
			for _, l := range rf.lines {
				if keyInQueryPosition(l, k) {
					return true
				}
			}
		}
		return false
	}
	// Each --refs argument must mention something. Files found in a
	// directory may be unrelated, but at least one of them must.
	hit := map[string]bool{}
	for _, rf := range refs {
		if mentions(rf) {
			hit[rf.arg] = true
		}
	}
	for _, g := range given {
		if !hit[g] {
			return distrust("--refs %s mentions no log message or key from the old side, so it can't show which changes break it; check that it's the right path", g)
		}
	}
	return nil
}

func diffEvents(oldEv, newEv map[string]*event, refs []*refFile) []Finding {
	var out []Finding
	sev := func(hits []string) string {
		if len(hits) > 0 {
			return sevBreak
		}
		return sevWarn
	}
	// Pair removed messages with added ones in the same function, in source order.
	var removed, added []*event
	for _, m := range sortedKeys(oldEv) {
		if newEv[m] == nil {
			removed = append(removed, oldEv[m])
		}
	}
	for _, m := range sortedKeys(newEv) {
		if oldEv[m] == nil {
			added = append(added, newEv[m])
		}
	}
	byPos := func(evs []*event) {
		sort.SliceStable(evs, func(i, j int) bool {
			a, b := evs[i].calls[0].pos, evs[j].calls[0].pos
			if a.Filename != b.Filename {
				return a.Filename < b.Filename
			}
			return a.Line < b.Line
		})
	}
	byPos(removed)
	byPos(added)
	pairs := map[*event]*event{}
	used := map[*event]bool{}
	for _, ro := range removed {
		of := ro.funcs()
		for _, an := range added {
			if used[an] {
				continue
			}
			shared := false
			for f := range an.funcs() {
				if of[f] {
					shared = true
				}
			}
			if shared && an.calls[0].lib == ro.calls[0].lib {
				pairs[ro] = an
				used[an] = true
				break
			}
		}
	}
	for _, ro := range removed {
		hits := findMessage(refs, ro.msg, ro.format)
		if an, ok := pairs[ro]; ok {
			out = append(out, Finding{
				Severity: sev(hits), Change: "message-changed", Message: ro.msg,
				Detail: fmt.Sprintf("no longer logged; %s now logs %s (%s), which may be its new wording", an.calls[0].fn, strconv.Quote(an.msg), an.level),
				Pos:    an.pos(), Refs: hits,
			})
			out = append(out, diffKeys(ro, an, refs)...)
			out = append(out, diffLevel(ro, an, refs, hits)...)
			continue
		}
		out = append(out, Finding{
			Severity: sev(hits), Change: "message-removed", Message: ro.msg,
			Detail: fmt.Sprintf("no longer logged (was %s in %s)", ro.level, ro.calls[0].fn),
			Pos:    "was " + ro.pos(), Refs: hits,
		})
	}
	for _, m := range sortedKeys(oldEv) {
		ne := newEv[m]
		if ne == nil {
			continue
		}
		oe := oldEv[m]
		out = append(out, diffKeys(oe, ne, refs)...)
		out = append(out, diffLevel(oe, ne, refs, findMessage(refs, oe.msg, oe.format))...)
	}
	for _, an := range added {
		if used[an] {
			continue
		}
		out = append(out, Finding{
			Severity: sevInfo, Change: "message-added", Message: an.msg,
			Detail: fmt.Sprintf("new %s message in %s", an.level, an.calls[0].fn),
			Pos:    an.pos(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := sevRank(out[i].Severity), sevRank(out[j].Severity); a != b {
			return a > b
		}
		if out[i].Message != out[j].Message {
			return out[i].Message < out[j].Message
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func diffKeys(oe, ne *event, refs []*refFile) []Finding {
	var out []Finding
	sev := func(hits []string) string {
		if len(hits) > 0 {
			return sevBreak
		}
		return sevWarn
	}
	var gone, fresh []string
	for _, k := range sortedKeys(oe.keys) {
		if ne.keys[k] == nil {
			gone = append(gone, k)
		}
	}
	for _, k := range sortedKeys(ne.keys) {
		if oe.keys[k] == nil {
			fresh = append(fresh, k)
		}
	}
	// A single removed key and a single added key of the same kind is a rename.
	if len(gone) == 1 && len(fresh) == 1 && oe.kind(gone[0]) == ne.kind(fresh[0]) {
		hits := findKey(refs, gone[0])
		out = append(out, Finding{
			Severity: sev(hits), Change: "key-renamed", Message: ne.msg, Key: gone[0],
			Detail: fmt.Sprintf("key %q is now %q", gone[0], fresh[0]),
			Pos:    ne.pos(), Refs: hits,
		})
		gone, fresh = nil, nil
	}
	for _, k := range gone {
		hits := findKey(refs, k)
		d := fmt.Sprintf("key %q is no longer logged with this message", k)
		if ne.dynArgs {
			d += "; the new call also passes attributes the tool can't read, so it may still be there"
		}
		out = append(out, Finding{
			Severity: sev(hits), Change: "key-removed", Message: ne.msg, Key: k,
			Detail: d, Pos: ne.pos(), Refs: hits,
		})
	}
	for _, k := range fresh {
		out = append(out, Finding{
			Severity: sevInfo, Change: "key-added", Message: ne.msg, Key: k,
			Detail: fmt.Sprintf("new key %q (%s)", k, ne.kind(k)), Pos: ne.pos(),
		})
	}
	for _, k := range sortedKeys(oe.keys) {
		if ne.keys[k] == nil {
			continue
		}
		ok, nk := oe.kind(k), ne.kind(k)
		if ok == nk || ok == kindAny || nk == kindAny {
			continue
		}
		hits := findKey(refs, k)
		out = append(out, Finding{
			Severity: sev(hits), Change: "kind-changed", Message: ne.msg, Key: k,
			Detail: fmt.Sprintf("value of %q changed from %s to %s", k, ok, nk),
			Pos:    ne.pos(), Refs: hits,
		})
	}
	return out
}

func diffLevel(oe, ne *event, refs []*refFile, hits []string) []Finding {
	if oe.rank == 0 || ne.rank == 0 || oe.rank == ne.rank {
		return nil
	}
	s := sevWarn
	if len(hits) > 0 {
		s = sevBreak
	}
	change, verb := "level-lowered", "lowered"
	if ne.rank > oe.rank {
		change, verb = "level-raised", "raised"
	}
	// Point at a new call that has the new level.
	at := ne.calls[0]
	for _, c := range ne.calls {
		if c.rank == ne.rank {
			at = c
			break
		}
	}
	detail := fmt.Sprintf("level %s from %s to %s", verb, oe.level, ne.level)
	stillOld := false
	for _, l := range ne.levels {
		if l == oe.level {
			stillOld = true
		}
	}
	if stillOld && len(ne.calls) > 1 {
		detail = fmt.Sprintf("now also logged at %s by %s; other calls still log it at %s", ne.level, at.fn, oe.level)
	}
	return []Finding{{
		Severity: s, Change: change, Message: ne.msg,
		Detail: detail,
		Pos:    fmt.Sprintf("%s:%d", at.pos.Filename, at.pos.Line), Refs: hits,
	}}
}

// ---------------------------------------------------------------------------
// Output.

func printText(w io.Writer, r *Report) {
	fmt.Fprintf(w, "logbreak %s -> %s: %d log messages compared (%s), %d call sites\n",
		r.Old, r.New, r.OldEvents, strings.Join(r.Libraries, ", "), r.OldCalls+r.NewCalls)
	if len(r.RefFiles) > 0 {
		fmt.Fprintf(w, "  searched %s: %s\n", plural(len(r.RefFiles), "ref file"), strings.Join(r.RefFiles, ", "))
	}
	for _, f := range r.Findings {
		target := strconv.Quote(f.Message)
		if f.Key != "" {
			target += " " + f.Key
		}
		if f.Ignored {
			fmt.Fprintf(w, "  %-5s %s %s (ignored: %s)\n", strings.ToUpper(f.Severity), f.Change, target, f.IgnoreReason)
		} else {
			fmt.Fprintf(w, "  %-5s %s %s\n", strings.ToUpper(f.Severity), f.Change, target)
		}
		fmt.Fprintf(w, "        %s\n", f.Detail)
		if f.Pos != "" {
			if strings.HasPrefix(f.Pos, "was ") {
				fmt.Fprintf(w, "        %s\n", f.Pos)
			} else {
				fmt.Fprintf(w, "        at %s\n", f.Pos)
			}
		}
		if len(f.Refs) > 0 {
			refs := f.Refs
			more := ""
			if len(refs) > 4 {
				more = fmt.Sprintf(" and %d more", len(refs)-4)
				refs = refs[:4]
			}
			fmt.Fprintf(w, "        used in %s%s\n", strings.Join(refs, ", "), more)
		}
	}
	for _, n := range r.NotChecked {
		fmt.Fprintf(w, "  not checked: %s\n", n)
	}
	for _, s := range r.StaleIgnores {
		fmt.Fprintf(w, "  stale ignore: %s\n", s)
	}
	if r.Breaks == 0 && r.Warnings == 0 {
		fmt.Fprintf(w, "  no changes to existing log messages, keys or levels")
		if r.Infos > 0 {
			fmt.Fprintf(w, " (%s)", plural(r.Infos, "addition"))
		}
		if r.Ignored > 0 {
			fmt.Fprintf(w, " (%s ignored)", plural(r.Ignored, "finding"))
		}
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, "  %d breaking, %s, %s\n", r.Breaks, plural(r.Warnings, "warning"), plural(r.Infos, "addition"))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

type listedEvent struct {
	Message string            `json:"message"`
	Format  bool              `json:"format,omitempty"`
	Levels  []string          `json:"levels"`
	Keys    map[string]string `json:"keys"`
	Partial bool              `json:"partial_keys,omitempty"`
	Library string            `json:"library"`
	Pos     []string          `json:"pos"`
}

func listEvents(o options, w io.Writer) error {
	s, err := loadSide("new", o.newDir, o)
	if err != nil {
		return err
	}
	if len(s.calls) == 0 {
		return distrust("no slog, zap or logr calls with a constant message in %s", o.newDir)
	}
	evs := groupEvents(s.calls)
	var list []listedEvent
	for _, m := range sortedKeys(evs) {
		e := evs[m]
		le := listedEvent{Message: m, Format: e.format, Levels: e.levels, Keys: map[string]string{}, Partial: e.dynArgs, Library: e.calls[0].lib}
		for k := range e.keys {
			le.Keys[k] = e.kind(k)
		}
		for _, c := range e.calls {
			le.Pos = append(le.Pos, fmt.Sprintf("%s:%d", c.pos.Filename, c.pos.Line))
		}
		list = append(list, le)
	}
	if o.format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	for _, le := range list {
		fmt.Fprintf(w, "%s [%s, %s]\n", strconv.Quote(le.Message), strings.Join(le.Levels, "/"), le.Library)
		for _, k := range sortedKeys(le.Keys) {
			fmt.Fprintf(w, "  %s: %s\n", k, le.Keys[k])
		}
		if le.Partial {
			fmt.Fprintf(w, "  (more attributes the tool can't read)\n")
		}
		fmt.Fprintf(w, "  at %s\n", strings.Join(le.Pos, ", "))
	}
	if len(s.dynamic) > 0 {
		fmt.Fprintf(w, "not listed: %s with a message that isn't a constant\n", plural(len(s.dynamic), "call site"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// git: extract the old revision of the module into a temporary directory.

func extractGitRev(newDir, ref string) (string, func(), error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = newDir
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return out.String(), nil
	}
	prefix, err := git("rev-parse", "--show-prefix")
	if err != nil {
		return "", nil, distrust("--base needs --new to be inside a git work tree: %v", err)
	}
	prefix = strings.TrimSpace(prefix)
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, distrust("--base: %v", err)
	}
	top = strings.TrimSpace(top)
	commit, err := git("rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", nil, distrust("--base %q is not a commit in this repository", ref)
	}
	commit = strings.TrimSpace(commit)
	tmp, err := os.MkdirTemp("", "logbreak-base-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	cmd := exec.Command("git", "archive", "--format=tar", commit)
	cmd.Dir = top
	var errb bytes.Buffer
	cmd.Stderr = &errb
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, err
	}
	xerr := untar(pipe, tmp)
	werr := cmd.Wait()
	if xerr != nil || werr != nil {
		cleanup()
		return "", nil, distrust("git archive %s failed: %v %v %s", ref, xerr, werr, strings.TrimSpace(errb.String()))
	}
	return filepath.Join(tmp, filepath.FromSlash(prefix)), cleanup, nil
}

func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks and other entries are skipped: they could point outside dst.
		}
	}
}
