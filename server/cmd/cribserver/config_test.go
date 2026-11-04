package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testFlags struct {
	fs    *flag.FlagSet
	http  *string
	pub   *bool
	every *time.Duration
	keep  *int
	key   *string
	url   *string
}

func newTestFlags() *testFlags {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	t := &testFlags{fs: fs}
	t.http = fs.String("http", "127.0.0.1:8080", "HTTP address")
	t.pub = fs.Bool("rtc-public-only", false, "public candidates only")
	t.every = fs.Duration("backup-every", 24*time.Hour, "backup interval")
	t.keep = fs.Int("backup-keep", 7, "backups kept")
	t.key = fs.String("email-key-file", "", "key file")
	t.url = fs.String("report-url", "https://github.com/Optimoos", "bug reports")
	fs.String("config", "", "config file")
	return t
}

func writeFile(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cribserver.conf")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyConfig(t *testing.T) {
	t.Setenv("CREDS", "/run/creds")
	p := writeFile(t, `
# a comment
http = :9090   # trailing comment
-rtc-public-only=true
backup-every = 6h
backup-keep = 3
email-key-file = ${CREDS}/email-key
report-url = "https://example.com/issues#new"
`)
	f := newTestFlags()
	if err := f.fs.Parse([]string{"-backup-keep", "10"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(f.fs, p); err != nil {
		t.Fatal(err)
	}
	if *f.http != ":9090" || !*f.pub || *f.every != 6*time.Hour || *f.key != "/run/creds/email-key" ||
		*f.url != "https://example.com/issues#new" {
		t.Errorf("got http=%q pub=%v every=%v key=%q url=%q", *f.http, *f.pub, *f.every, *f.key, *f.url)
	}
	if *f.keep != 10 {
		t.Errorf("backup-keep = %d, want the command line's 10", *f.keep)
	}
}

func TestApplyConfigErrors(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"htp = :80", `:1: unknown setting "htp"`},
		{"http :80", ":1: want name = value"},
		{"http = :80\n\nhttp = :81", ":3: http is already set on line 1"},
		{"backup-keep = many", ":1: backup-keep: parse error"},
		{"config = other.conf", "cannot name another"},
		{"email-key-file = ${NO_SUCH_VAR_HERE}/k", "NO_SUCH_VAR_HERE is not set"},
		{`report-url = "https://x" junk`, "after the quoted value"},
		{`report-url = "https://x`, "bad quoted value"},
	} {
		f := newTestFlags()
		err := applyConfig(f.fs, writeFile(t, tc.file))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.file, err, tc.want)
		}
	}
	f := newTestFlags()
	if err := applyConfig(f.fs, filepath.Join(t.TempDir(), "missing.conf")); err == nil {
		t.Error("a missing file was not an error")
	}
}

// What -print-config writes reads back as the same settings.
func TestWriteConfigRoundTrip(t *testing.T) {
	f := newTestFlags()
	if err := f.fs.Parse([]string{"-http", ":9090", "-rtc-public-only", "-report-url", "https://example.com/a#b"}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	writeConfig(&b, f.fs)
	out := b.String()
	for _, want := range []string{"\nhttp = :9090\n", "\nrtc-public-only = true\n", "\n# backup-keep = 7\n", `report-url = "https://example.com/a#b"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "config =") {
		t.Errorf("config itself was written:\n%s", out)
	}
	g := newTestFlags()
	if err := applyConfig(g.fs, writeFile(t, out)); err != nil {
		t.Fatal(err)
	}
	if *g.http != ":9090" || !*g.pub || *g.url != "https://example.com/a#b" || *g.keep != 7 {
		t.Errorf("read back http=%q pub=%v url=%q keep=%d", *g.http, *g.pub, *g.url, *g.keep)
	}
}
