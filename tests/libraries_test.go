package tests

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// A change case: the old and new body of a function F in package p, and the
// findings logbreak must report. An empty want means the change must stay
// quiet, even with --fail-on warn.
type changeCase struct {
	name     string
	imports  string
	old, new string
	want     []string
}

func runCase(t *testing.T, c changeCase, gomod string, env []string) {
	t.Helper()
	src := func(body string) string {
		return "package p\n\nimport (\n" + c.imports + "\n)\n\n" + body + "\n"
	}
	oldDir := writeModule(t, map[string]string{"go.mod": gomod, "p/p.go": src(c.old)})
	newDir := writeModule(t, map[string]string{"go.mod": gomod, "p/p.go": src(c.new)})
	r := runIn(t, "", env, "--old", oldDir, "--new", newDir, "--format", "json")
	if r.code == 2 {
		t.Fatalf("exit 2\n%s", r.stderr)
	}
	rep := parseReport(t, r)
	var got []string
	for _, f := range changes(rep) {
		if !strings.HasPrefix(f, "info ") {
			got = append(got, f)
		}
	}
	if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
		t.Errorf("findings:\n  %s\nwant:\n  %s\nfull output: %s", strings.Join(got, "\n  "), strings.Join(c.want, "\n  "), r.stdout)
	}
	wantCode := 0
	if len(c.want) > 0 {
		wantCode = 1
	}
	if r.code != wantCode {
		t.Errorf("exit %d, want %d", r.code, wantCode)
	}
}

const slogImports = "\t\"context\"\n\t\"log/slog\"\n\t\"time\"\n\t\"strconv\""

// keep makes every import used.
const keep = "\nvar _ = context.Background\nvar _ = time.Second\nvar _ = strconv.Itoa\n"

func TestSlogChanges(t *testing.T) {
	cases := []changeCase{
		{name: "message reworded",
			old:  `func F(l *slog.Logger) { l.Error("payment failed", "order_id", 1) }`,
			new:  `func F(l *slog.Logger) { l.Error("charge failed", "order_id", 1) }`,
			want: []string{"warn message-changed payment failed"}},
		{name: "message removed",
			old:  "func F(l *slog.Logger) { l.Info(\"a\"); l.Warn(\"cache miss\") }\nfunc G(l *slog.Logger) { l.Info(\"started\") }",
			new:  "func F(l *slog.Logger) { l.Info(\"a\") }\nfunc G(l *slog.Logger) { l.Info(\"started\") }",
			want: []string{"warn message-removed cache miss"}},
		{name: "key renamed",
			old:  `func F(id string) { slog.Info("login", "user_id", id) }`,
			new:  `func F(id string) { slog.Info("login", "userId", id) }`,
			want: []string{"warn key-renamed login user_id"}},
		{name: "two keys removed",
			old:  `func F(id string, n int) { slog.Info("login", "user_id", id, "tries", n, "ok", true) }`,
			new:  `func F(id string, n int) { slog.Info("login", "ok", true) }`,
			want: []string{"warn key-removed login tries", "warn key-removed login user_id"}},
		{name: "number became string",
			old:  `func F(code int) { slog.Warn("upstream error", "status", code) }`,
			new:  `func F(code int) { slog.Warn("upstream error", "status", strconv.Itoa(code)) }`,
			want: []string{"warn kind-changed upstream error status"}},
		{name: "level lowered",
			old:  `func F() { slog.Error("disk full") }`,
			new:  `func F() { slog.Warn("disk full") }`,
			want: []string{"warn level-lowered disk full"}},
		{name: "level raised with Log",
			old:  `func F(ctx context.Context) { slog.Log(ctx, slog.LevelInfo, "retrying") }`,
			new:  `func F(ctx context.Context) { slog.Log(ctx, slog.LevelError, "retrying") }`,
			want: []string{"warn level-raised retrying"}},
		{name: "group key renamed",
			old:  `func F(id string) { slog.Info("request", slog.Group("req", "id", id, "path", "/")) }`,
			new:  `func F(id string) { slog.Info("request", slog.Group("req", "request_id", id, "path", "/")) }`,
			want: []string{"warn key-renamed request req.id"}},
		{name: "attr removed from chained With",
			old:  `func F(l *slog.Logger, id string) { l.With("tenant", id).Info("synced", "n", 1) }`,
			new:  `func F(l *slog.Logger, id string) { l.Info("synced", "n", 1) }`,
			want: []string{"warn key-removed synced tenant"}},
		{name: "duration became number",
			old:  `func F(d time.Duration) { slog.Info("done", slog.Duration("took", d)) }`,
			new:  `func F(d time.Duration) { slog.Info("done", slog.Int64("took", d.Milliseconds())) }`,
			want: []string{"warn kind-changed done took"}},
		{name: "Sprintf template changed",
			old:  `func F(n int) { slog.Info(fmt.Sprintf("%d jobs queued", n)) }`,
			new:  `func F(n int) { slog.Info(fmt.Sprintf("%d jobs waiting", n)) }`,
			want: []string{"warn message-changed %d jobs queued"}},
		// Changes that keep every message, key, kind and level must stay quiet.
		{name: "constant instead of literal",
			old: `func F(id string) { slog.Info("login", "user_id", id) }`,
			new: "const msg, key = \"login\", \"user_id\"\nfunc F(id string) { slog.Info(msg, key, id) }"},
		{name: "Info to InfoContext and LogAttrs",
			old: `func F(ctx context.Context, id string) { slog.Info("login", "user_id", id); slog.Error("boom", "n", 1) }`,
			new: `func F(ctx context.Context, id string) { slog.InfoContext(ctx, "login", slog.String("user_id", id)); slog.LogAttrs(ctx, slog.LevelError, "boom", slog.Int("n", 1)) }`},
		{name: "With chain instead of inline",
			old: `func F(l *slog.Logger, id string) { l.Info("synced", "tenant", id, "n", 1) }`,
			new: `func F(l *slog.Logger, id string) { l.With("tenant", id).Info("synced", "n", 1) }`},
		{name: "int to int64 and reordered keys",
			old: `func F(n int, id string) { slog.Info("batch", "size", n, "id", id) }`,
			new: `func F(n int64, id string) { slog.Info("batch", "id", id, "size", n) }`},
		{name: "empty group inlines",
			old: `func F(id string) { slog.Info("login", "user_id", id) }`,
			new: `func F(id string) { slog.Info("login", slog.Group("", "user_id", id)) }`},
		{name: "call moved to another function",
			old: "func F(id string) { slog.Info(\"login\", \"user_id\", id) }",
			new: "func F(id string) { g(id) }\nfunc g(id string) { slog.Info(\"login\", \"user_id\", id) }"},
		{name: "error value and string",
			old: `func F(err error) { slog.Error("save failed", "err", err) }`,
			new: `func F(err error) { slog.Error("save failed", "err", err.Error()) }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.imports = slogImports + "\n\t\"fmt\""
			c.old += keep + "var _ = fmt.Sprint\n"
			c.new += keep + "var _ = fmt.Sprint\n"
			runCase(t, c, "module example.com/m\n\ngo 1.22\n", nil)
		})
	}
}

var (
	depsOnce sync.Once
	depsErr  error
	depsOut  []byte
)

const depsGoMod = "module example.com/m\n\ngo 1.22\n\nrequire (\n\tgithub.com/go-logr/logr v1.4.2\n\tgo.uber.org/zap v1.27.0\n)\n"

// needDeps downloads zap and logr into the module cache, or skips the test
// when that isn't possible (no network and an empty cache).
func needDeps(t *testing.T) []string {
	t.Helper()
	depsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "logbreak-deps")
		if err != nil {
			depsErr = err
			return
		}
		defer os.RemoveAll(dir)
		os.WriteFile(dir+"/go.mod", []byte(depsGoMod), 0o644)
		os.WriteFile(dir+"/p.go", []byte("package p\n\nimport (\n\t_ \"github.com/go-logr/logr\"\n\t_ \"go.uber.org/zap\"\n)\n"), 0o644)
		cmd := exec.Command("go", "mod", "download", "go.uber.org/zap", "go.uber.org/multierr", "github.com/go-logr/logr")
		cmd.Dir = dir
		depsOut, depsErr = cmd.CombinedOutput()
	})
	if depsErr != nil {
		t.Skipf("zap and logr aren't in the module cache and can't be downloaded: %v\n%s", depsErr, depsOut)
	}
	return []string{"GOFLAGS=-mod=mod"}
}

const zapImports = "\t\"time\"\n\t\"go.uber.org/zap\"\n\t\"go.uber.org/zap/zapcore\"\n\t\"github.com/go-logr/logr\""
const zapKeep = "\nvar _ = time.Second\nvar _ zapcore.Level\nvar _ logr.Logger\nvar _ = zap.L\n"

func TestZapAndLogrChanges(t *testing.T) {
	env := needDeps(t)
	cases := []changeCase{
		{name: "zap field renamed",
			old:  `func F(l *zap.Logger, id string) { l.Info("job done", zap.String("job_id", id)) }`,
			new:  `func F(l *zap.Logger, id string) { l.Info("job done", zap.String("job", id)) }`,
			want: []string{"warn key-renamed job done job_id"}},
		{name: "zap Check level lowered",
			old:  `func F(l *zap.Logger) { if ce := l.Check(zapcore.ErrorLevel, "aborting"); ce != nil { ce.Write(zap.Int("n", 1)) } }`,
			new:  `func F(l *zap.Logger) { if ce := l.Check(zapcore.WarnLevel, "aborting"); ce != nil { ce.Write(zap.Int("n", 1)) } }`,
			want: []string{"warn level-lowered aborting"}},
		{name: "zap Check field removed",
			old:  `func F(l *zap.Logger, err error) { if ce := l.Check(zapcore.ErrorLevel, "aborting"); ce != nil { ce.Write(zap.Error(err), zap.Int("n", 1)) } }`,
			new:  `func F(l *zap.Logger, err error) { if ce := l.Check(zapcore.ErrorLevel, "aborting"); ce != nil { ce.Write(zap.Int("n", 1)) } }`,
			want: []string{"warn key-removed aborting error"}},
		{name: "zap namespace renamed",
			old:  `func F(l *zap.Logger) { l.Warn("slow query", zap.Namespace("db"), zap.Int("rows", 1)) }`,
			new:  `func F(l *zap.Logger) { l.Warn("slow query", zap.Namespace("sql"), zap.Int("rows", 1)) }`,
			want: []string{"warn key-removed slow query db", "warn key-removed slow query db.rows"}},
		{name: "sugar key removed",
			old:  `func F(s *zap.SugaredLogger, id string) { s.Infow("login", "user", id, "ip", "x") }`,
			new:  `func F(s *zap.SugaredLogger, id string) { s.Infow("login", "user", id) }`,
			want: []string{"warn key-removed login ip"}},
		{name: "sugar template reworded",
			old:  `func F(s *zap.SugaredLogger, p string, err error) { s.Errorf("could not open %s: %v", p, err) }`,
			new:  `func F(s *zap.SugaredLogger, p string, err error) { s.Errorf("open %s failed: %v", p, err) }`,
			want: []string{"warn message-changed could not open %s: %v"}},
		{name: "logr verbosity raised",
			old:  `func F(l logr.Logger) { l.V(1).Info("reconciling", "name", "x") }`,
			new:  `func F(l logr.Logger) { l.V(4).Info("reconciling", "name", "x") }`,
			want: []string{"warn level-lowered reconciling"}},
		{name: "logr Error became Info",
			old:  `func F(l logr.Logger, err error) { l.Error(err, "sync failed", "name", "x") }`,
			new:  `func F(l logr.Logger, err error) { l.Info("sync failed", "name", "x", "err", err) }`,
			want: []string{"warn key-renamed sync failed error", "warn level-lowered sync failed"}},
		// Quiet cases.
		{name: "zap Check instead of direct call",
			old: `func F(l *zap.Logger, n int) { l.Error("aborting", zap.Int("n", n)) }`,
			new: `func F(l *zap.Logger, n int) { if ce := l.Check(zapcore.ErrorLevel, "aborting"); ce != nil { ce.Write(zap.Int("n", n)) } }`},
		{name: "zap Int to Int64 and With",
			old: `func F(l *zap.Logger, n int) { l.Info("batch", zap.Int("size", n), zap.String("q", "a")) }`,
			new: `func F(l *zap.Logger, n int64) { l.With(zap.String("q", "a")).Info("batch", zap.Int64("size", n)) }`},
		{name: "logr WithValues instead of inline",
			old: `func F(l logr.Logger) { l.Info("reconciling", "name", "x", "ns", "y") }`,
			new: `func F(l logr.Logger) { l.WithValues("ns", "y").Info("reconciling", "name", "x") }`},
		{name: "logr V(0) is info",
			old: `func F(l logr.Logger) { l.Info("ready") }`,
			new: `func F(l logr.Logger) { l.V(0).Info("ready") }`},
		{name: "zap.L and zap.S",
			old: `func F() { zap.L().Info("boot", zap.Duration("took", time.Second)); zap.S().Infow("boot2", "k", 1) }`,
			new: `func F() { lg := zap.L(); lg.Info("boot", zap.Duration("took", 2*time.Second)); zap.S().Infow("boot2", "k", 2) }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.imports = zapImports
			c.old += zapKeep
			c.new += zapKeep
			runCase(t, c, depsGoMod, env)
		})
	}
}
