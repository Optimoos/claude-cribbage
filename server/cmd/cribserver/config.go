package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// A config file sets flags, one per line, so a configured server needs no
// long command line:
//
//	# comments and blank lines are ignored
//	http = 127.0.0.1:8080
//	rtc-public-only = true
//	email-key-file = ${CREDENTIALS_DIRECTORY}/email-key
//
// Keys are flag names without the dash. A value may be quoted ("..."), to
// keep leading or trailing spaces or a #. ${NAME} is replaced by the
// environment variable NAME, which must be set. Flags given on the command
// line override the file. Unknown and repeated keys are errors, so a typo
// is not silently ignored.

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// applyConfig sets the flags in fs named by the file at path, except those
// already set on the command line.
func applyConfig(fs *flag.FlagSet, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		where := fmt.Sprintf("%s:%d", path, n)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s: want name = value", where)
		}
		key = strings.TrimPrefix(strings.TrimSpace(key), "-")
		if key == "config" {
			return fmt.Errorf("%s: a config file cannot name another", where)
		}
		if fs.Lookup(key) == nil {
			return fmt.Errorf("%s: unknown setting %q (see cribserver -help)", where, key)
		}
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("%s: %s is already set on line %d", where, key, prev)
		}
		seen[key] = n
		value, err = configValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s: %s: %w", where, key, err)
		}
		if set[key] {
			continue
		}
		if err := fs.Set(key, value); err != nil {
			return fmt.Errorf("%s: %s: %w", where, key, err)
		}
	}
	return sc.Err()
}

// configValue unquotes a value, or strips a trailing comment from an
// unquoted one, then expands ${NAME}.
func configValue(v string) (string, error) {
	if strings.HasPrefix(v, `"`) {
		u, err := strconv.QuotedPrefix(v)
		if err != nil {
			return "", fmt.Errorf("bad quoted value")
		}
		rest := strings.TrimSpace(v[len(u):])
		if rest != "" && rest[0] != '#' {
			return "", fmt.Errorf("unexpected %q after the quoted value", rest)
		}
		v, _ = strconv.Unquote(u)
	} else if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	var missing string
	v = envRef.ReplaceAllStringFunc(v, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		val, ok := os.LookupEnv(name)
		if !ok && missing == "" {
			missing = name
		}
		return val
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return v, nil
}

// writeConfig writes a config file listing every flag with its help as a
// comment. Flags at their defaults are commented out, so a later version's
// defaults still apply to them.
func writeConfig(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "# cribserver configuration: name = value, one per line.")
	fmt.Fprintln(w, "# Command-line flags override these. ${NAME} is an environment variable.")
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" || f.Name == "print-config" {
			return
		}
		fmt.Fprintln(w)
		for _, l := range wrap(f.Usage, 74) {
			fmt.Fprintln(w, "# "+l)
		}
		v := f.Value.String()
		if v == "" || strings.TrimSpace(v) != v || strings.Contains(v, "#") || strings.HasPrefix(v, `"`) {
			v = strconv.Quote(v)
		}
		prefix := ""
		if f.Value.String() == f.DefValue {
			prefix = "# "
		}
		fmt.Fprintf(w, "%s%s = %s\n", prefix, f.Name, v)
	})
}

func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	return append(lines, line)
}
