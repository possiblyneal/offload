package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// jobPathsFor resolves the writable and read-only paths from the Operator's Claude
// Code settings. Missing settings mean no rules: the job's repository alone is writable.
func jobPathsFor(home string, repos []string, excluded func(string) bool) (jobPaths, error) {
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		settings = []byte("{}")
	} else if err != nil {
		return jobPaths{}, err
	}
	return resolvePathsFiltered(settings, home, repos, os.DirFS("/"), excluded)
}

// remoteLocalPath is a configured tool-cache path, under home, that the launcher binds from
// the remote's own disk; rel is it relative to home.
type remoteLocalPath struct {
	path, rel string
	readOnly  bool // a `ro` line: bound read-only
}

// remoteLocalPaths lists the configured tool-cache paths, under home, that exist on dev.
// The launcher skips the others, because a path under dev's read-only root cannot be
// created.
func remoteLocalPaths(configDir, home string) []remoteLocalPath {
	f, err := os.Open(filepath.Join(configDir, "remote-local-paths"))
	if err != nil || home == "" {
		return nil
	}
	defer f.Close()
	lines, _ := contentLines(f)
	var out []remoteLocalPath
	for _, line := range lines {
		rel, readOnly := strings.CutPrefix(line, "ro ")
		p := filepath.Join(home, rel)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, remoteLocalPath{p, filepath.Clean(rel), readOnly})
		}
	}
	return out
}

// remoteLocalRefusal says why the job must run here: its cwd or an argument names a path
// under a remote-local path, which the job would read, write or delete on the remote's
// own copy while dev's stays as it was; or "". An argument is judged as tmpRefusal judges
// one, and its script text by its absolute paths once `~`, `$HOME` and `${HOME}` are read
// as home (expandHome), so `du ~/.cache` and `find ~` count. An environment value is not
// judged: GOMODCACHE and the like name these paths so the tools use the remote's copy.
// A relative word counts once read from any directory the script names (argPaths.fileWords).
// A link (jobLinks) onto or holding one counts as the cwd does: `make` follows it unnamed.
func remoteLocalRefusal(rl []remoteLocalPath, home, cwd string, argv, links []string) string {
	dirs := remoteLocalDirs(rl)
	under := func(p string) string {
		for _, q := range []string{filepath.Clean(p), realPath(p)} {
			if d := within(dirs, q); d != "" {
				return d
			}
		}
		return ""
	}
	const why = ", which a remote job sees as the remote's own copy, not dev's"
	if d := under(cwd); d != "" {
		return fmt.Sprintf("cwd %s is under the remote-local %s%s", cwd, d, why)
	}
	// An argument that holds one (du ~/.cache, rm -r ~/.cache) would list or remove the
	// remote's copy inside it; the cwd may hold one, or every job run from home would stay.
	inOrHolding := func(p string) string {
		if d := under(p); d != "" {
			return d
		}
		for _, q := range []string{filepath.Clean(p), realPath(p)} {
			if d := holding(dirs, q); d != "" {
				return d
			}
		}
		return ""
	}
	for _, l := range links {
		if d := inOrHolding(realPath(l)); d != "" {
			return fmt.Sprintf("link %s reaches the remote-local %s%s", l, d, why)
		}
	}
	// A word cut at an expansion (`.ca$X`, `.ca*`, `.ca{che,x}`) may name any path that starts
	// as it does, as scriptPaths reads an absolute one. A bare one at the cwd (`*`, `$F`) is
	// taken to name only what a glob matches, so a dotted entry (`.cache/go-build`) is left
	// to it and an undotted one (`go/pkg/mod`) is not.
	here := strings.TrimSuffix(cwd, "/") + "/"
	startsOne := func(p string) string {
		pre, ok := expansionPrefix(p)
		if !ok {
			return ""
		}
		for _, d := range dirs {
			if pre == here && strings.HasPrefix(strings.TrimPrefix(d, here), ".") {
				continue
			}
			if strings.HasPrefix(d, pre) || strings.HasPrefix(pre, strings.TrimSuffix(d, "/")+"/") {
				return d
			}
		}
		return ""
	}
	for _, a := range argv {
		r := readArg(a, cwd, home)
		for _, c := range r.candidates() {
			if d := inOrHolding(c); d != "" {
				return fmt.Sprintf("argument %s reaches the remote-local %s%s", c, d, why)
			}
			if d := startsOne(c); d != "" {
				return fmt.Sprintf("argument %s may name the remote-local %s%s", c, d, why)
			}
		}
		for _, p := range r.written {
			if d := inOrHolding(p); d != "" {
				return fmt.Sprintf("argument %q names %s, which reaches the remote-local %s%s", a, p, d, why)
			}
		}
		// A relative word read from a directory the script enters (`cd .cache && du go-build`).
		for _, p := range r.fileWords() {
			if d := under(p); d != "" {
				return fmt.Sprintf("argument %q names %s, which is under the remote-local %s%s", a, p, d, why)
			}
		}
		// Each word from the directory the script is in at that point holds one too (`cd; du .cache`).
		for _, p := range r.steps {
			if d := inOrHolding(p); d != "" {
				return fmt.Sprintf("argument %q names %s, which reaches the remote-local %s%s", a, p, d, why)
			}
			if d := startsOne(p); d != "" {
				return fmt.Sprintf("argument %q names %s, which may name the remote-local %s%s", a, p, d, why)
			}
		}
	}
	return ""
}

// homeForm matches the shell forms of the home directory expandHome reads: `$HOME`,
// `${HOME}` and `${HOME<op>word}` (set and non-empty, the shell reads HOME), each alone in
// double quotes or bare.
var homeForm = regexp.MustCompile(`^"?(\$HOME\b|\$\{HOME(:?[-=?+][^}]*)?\})"?`)

// tildeUser matches a `~name` prefix: another user's home.
var tildeUser = regexp.MustCompile(`^~([a-z_][a-z0-9_-]*)`)

// remoteLocalDirs are rl's paths, each also by its real path.
func remoteLocalDirs(rl []remoteLocalPath) []string {
	var dirs []string
	for _, r := range rl {
		dirs = append(dirs, r.path)
	}
	return withRealPaths(dirs)
}

// expandHome writes home for each `$HOME`, `${HOME}` (or `${HOME:-x}` and the like) and
// word-initial `~` in a script s, and a user's own home for `~name`, as the shell would
// read them; s as it is when home is "". A quoted `"$HOME"` loses its quotes, so the path
// that follows it (`"$HOME"/code`) is judged as one path.
func expandHome(s, home string) string {
	if home == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		rest := s[i:]
		wordStart := i == 0 || strings.IndexByte(" \t\n=:;&|('\"<>`{,", s[i-1]) >= 0 || braceDefault(s, i)
		ends := func(k int) bool { return k == len(rest) || strings.IndexByte("/ \t\n;&|)'\"<>`},:", rest[k]) >= 0 }
		if m := homeForm.FindString(rest); m != "" && (m[0] == '"') == (m[len(m)-1] == '"') {
			b.WriteString(home)
			i += len(m) - 1
			continue
		}
		if s[i] == '~' && wordStart {
			if ends(1) {
				b.WriteString(home)
				continue
			}
			if m := tildeUser.FindString(rest); m != "" && ends(len(m)) {
				if u, err := user.Lookup(m[1:]); err == nil {
					b.WriteString(u.HomeDir)
					i += len(m) - 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// accountHome is the home directory of the user's passwd entry, where the launcher binds
// the remote-local paths whatever HOME the caller set; "" when it cannot be read.
func accountHome() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// scriptHome is the home a job's shell reads `~` and $HOME in a script as: the HOME of the
// job's environment, which a caller may have set away from its account's; "" when unset.
func scriptHome(env []string) string {
	home := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "HOME="); ok {
			home = v
		}
	}
	return home
}

// explainPaths prints each path class the job gets.
func explainPaths(w io.Writer, j job, remoteLocal []remoteLocalPath) {
	for _, p := range j.writable {
		fmt.Fprintln(w, "offload: writable", p)
	}
	for _, p := range j.readOnly {
		fmt.Fprintln(w, "offload: read-only", p)
	}
	for _, p := range remoteLocal {
		if p.readOnly {
			fmt.Fprintln(w, "offload: remote-local", p.path, "(read-only)")
		} else {
			fmt.Fprintln(w, "offload: remote-local", p.path)
		}
	}
	fmt.Fprintln(w, "offload: private /dev/shm")
	fmt.Fprintln(w, "offload: private /dev/mqueue")
	fmt.Fprintln(w, "offload: empty /run")
}

// privateDirs are the directories the job has its own of, which end with it: /dev/shm and
// /dev/mqueue (its IPC namespace's) and /run (an empty tmpfs only root may write). Dev's
// own files there are invisible to the job; offload-wait fails a job that leaves anything
// in /dev/shm or /dev/mqueue. Dev's /tmp and /var/tmp are the job's own too (sharedTmp).
var privateDirs = []string{"/dev/shm", "/dev/mqueue", "/run"}

// devFiles are the device files that read and write the same on any machine, with bash's
// /dev/tcp and /dev/udp, which name a connection and no file; every other
// path under /dev (a disk, a GPU, a serial port, a terminal) is dev's own hardware, which a
// remote has its own of or lacks.
var devFiles = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom",
	"/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/fd", "/dev/tcp", "/dev/udp"}

// ownProcess are the paths that name the process opening them; under ownRoot, though,
// they name dev's own files again.
var ownProcess = append([]string{"/proc/self", "/proc/thread-self"}, devFiles...)

var ownRoot = []string{"/proc/self/root", "/proc/thread-self/root"}

// hostOnly says what of dev's own machine the cleaned absolute path p names, or "": a device
// (any but devFiles), the kernel's view of dev's hardware under /sys, or a dev process by its pid
// under /proc. On a remote each names that machine's own, or nothing.
func hostOnly(p string) string {
	switch {
	case within([]string{"/dev"}, p) != "" && within(devFiles, p) == "":
		return "dev's own device " + p
	case within([]string{"/sys"}, p) != "":
		return "dev's own kernel and hardware state " + p
	}
	if rest, ok := strings.CutPrefix(p, "/proc/"); ok {
		pid, _, _ := strings.Cut(rest, "/")
		// This wrapper's own pid is what /proc/self resolves to here (/etc/mtab links
		// there), and on the remote names the job's own process.
		if _, err := strconv.Atoi(pid); err == nil && pid != strconv.Itoa(os.Getpid()) {
			return "the dev process " + pid + " (" + p + ")"
		}
	}
	return ""
}

// runAliases are dev's symlinks into /run, which a script may name instead: a path judged
// as a path resolves them, a mention in a script does not.
var runAliases = []string{"/var/run", "/var/lock"}

// hostOnlyAs is hostOnly of q, the cleaned path p or its real path, but "" for a p that names
// the process opening it (/dev/stdin, /dev/fd/N, /proc/self), wherever it runs, though it
// resolves through this wrapper's own files.
func hostOnlyAs(p, q string) string {
	if within(ownProcess, p) != "" && within(ownRoot, p) == "" {
		return ""
	}
	return hostOnly(q)
}

// envExempt are the variables whose value names a private directory or an unexported mount
// but that the job never writes through: the display, which the job has none of; the ssh
// agent, which the launcher replaces with the remote's; tmux's, gpg-agent's and systemd's
// notify sockets; SSH_TTY and GPG_TTY, the caller's terminal; OLDPWD, the shell's previous directory; MEMORY_PRESSURE_WATCH, the
// cgroup file systemd hands every service (Orca's server among them); and XDG_RUNTIME_DIR, set in every login shell of dev, which names dev's
// /run/user/<uid>, absent from the job's root-only /run.
// A write through any of them fails in the job. DBUS_* is dropped too.
var envExempt = map[string]bool{"DISPLAY": true, "WAYLAND_DISPLAY": true, "XAUTHORITY": true, "SSH_AUTH_SOCK": true,
	"TMUX": true, "GPG_AGENT_INFO": true, "NOTIFY_SOCKET": true, "XDG_RUNTIME_DIR": true, "OLDPWD": true,
	"MEMORY_PRESSURE_WATCH": true, "SSH_TTY": true, "GPG_TTY": true}

// tmpRefusal says why the job must run here: its cwd, an argument, TMPDIR or another
// variable names a path on one of the unexported mounts, or in a private directory. A path is
// judged as written and by its real path (symlinks resolved along its longest existing
// prefix), a relative one from the cwd. Every argument word is judged as a path, and so
// is its value after the first `=` (`--out=x`, `of=x`) and after a short option's letter
// (`-ox`). A cwd or argument word that holds an unexported mount (`/`, `/mnt`) refuses
// too: the remote sees, in its place, the directory the mount hides. Any argument or variable that merely mentions a private directory or an
// unexported mount (a shell script's `> /dev/shm/out`, GOCOVERDIR=/tmp/c) refuses too:
// running here loses nothing.
func tmpRefusal(mounts []string, cwd string, env []string, argv []string) string {
	return tmpRefusalIn(mounts, cwd, env, argv, exportView{})
}

func tmpRefusalIn(mounts []string, cwd string, env []string, argv []string, view exportView) string {
	check := func(what, p string) string {
		for _, q := range []string{filepath.Clean(p), realPath(p)} {
			if m := view.missing(mounts, q); m != "" {
				return fmt.Sprintf("%s %s is on dev's %s mount, which is not exported", what, p, m)
			}
			if m := within(privateDirs, q); m != "" {
				return fmt.Sprintf("%s %s is under %s, which the job sees as a private directory removed when it ends", what, p, m)
			}
			if h := hostOnlyAs(filepath.Clean(p), q); h != "" {
				return fmt.Sprintf("%s %s is %s, which a remote has its own of", what, p, h)
			}
		}
		return ""
	}
	home := scriptHome(env)
	if view.root != "" && findCommand(argv, func(cmd []string) string {
		if filepath.Base(cmd[0]) == "go" {
			return "go"
		}
		return ""
	}) != "" {
		var goenv, xdg string
		for _, entry := range env {
			name, value, _ := strings.Cut(entry, "=")
			switch name {
			case "GOENV":
				goenv = value
			case "XDG_CONFIG_HOME":
				xdg = value
			}
		}
		if goenv == "" {
			if xdg == "" {
				xdg = filepath.Join(home, ".config")
			}
			goenv = filepath.Join(xdg, "go", "env")
		}
		if goenv != "off" {
			if _, err := os.Stat(goenv); err == nil {
				if reason := check("Go configuration", goenv); reason != "" {
					return reason
				}
			}
		}
	}
	mentions := func(s string) string {
		s = squashPath(s)
		if d := namesDir(s, append(privateDirs, runAliases...)); d != "" {
			return d + ", which the job sees as a private directory removed when it ends"
		}
		if m := namesDir(s, mounts); view.root == "" && m != "" {
			return "dev's " + m + " mount, which is not exported"
		}
		return ""
	}
	// holds judges a directory a job may walk: the remote would list the directory a mount
	// hides where dev lists the mount.
	holds := func(what, p string) string {
		for _, q := range []string{filepath.Clean(p), realPath(p)} {
			if m := holding(mounts, q); m != "" {
				return fmt.Sprintf("%s %s holds dev's %s mount, which is not exported", what, p, m)
			}
		}
		return ""
	}
	// Every job may write /tmp and /var/tmp; when either is a mount the export does not
	// carry (a PrivateTmp service's), a remote job's files land where the caller cannot see.
	for _, t := range sharedTmp {
		if m := view.missing(mounts, t); m != "" {
			return fmt.Sprintf("%s is on dev's %s mount, which is not exported", t, m)
		}
	}
	if r := check("cwd", cwd); r != "" {
		return r
	}
	if r := holds("cwd", cwd); r != "" {
		return r
	}
	for _, e := range env {
		name, v, _ := strings.Cut(e, "=")
		if name == "TMPDIR" {
			p := v
			if p != "" && !filepath.IsAbs(p) {
				p = cwd + "/" + p // a relative TMPDIR is opened from the cwd, as an argument is
			}
			if r := check("TMPDIR", p); r != "" {
				return r
			}
		}
		if envExempt[name] || strings.HasPrefix(name, "DBUS_") {
			continue
		}
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			if u, err := url.Parse(v); err == nil && u.Scheme != "" && u.Host != "" && (u.Path == "" || u.Path == "/") {
				continue // The proxy endpoint is a network address, not a file path.
			}
		}
		if r := mentions(v); r != "" {
			return name + " names " + r
		}
		written := readArg(v, cwd, home).written
		if view.root != "" {
			written = slices.DeleteFunc(written, func(p string) bool {
				return name == "HOME" && p == view.home || within(view.caches, p) != ""
			})
		}
		if r := scriptRefusal(mounts, written, false, view); r != "" {
			return name + " names " + r
		}
	}
	for i, a := range argv {
		if a == "/" && slashIsText(argv[:i], argv) {
			continue // `cut -d / -f2`, `tr ' ' /`: text, not the root
		}
		ap := readArg(a, cwd, home)
		for _, c := range ap.candidates() {
			if r := check("argument", c); r != "" {
				return r
			}
		}
		for _, c := range ap.candidates() {
			if c == "/" && len(a) == 3 && a[0] == '-' && strings.IndexByte("dFts", a[1]) >= 0 {
				continue // a delimiter (`cut -d/`, `awk -F/`, `sort -t/`, `column -s/`) names no directory
			}
			if r := holds("argument", c); r != "" {
				return r
			}
		}
		if r := mentions(a); r != "" {
			return fmt.Sprintf("argument %q names %s", a, r)
		}
		if r := scriptRefusal(mounts, ap.written, true, view); r != "" {
			return fmt.Sprintf("argument %q names %s", a, r)
		}
	}
	return ""
}

// scriptRefusal judges each absolute path written in a script (argPaths.written) as
// tmpRefusal judges a path, as written and by its real path: on an unexported mount, under
// a private directory, naming dev's own machine, or, with holds, holding an unexported
// mount; or "". An environment value is judged without holds: HOME and PATH hold every
// mount below them, and a job that walks one is told so in its arguments.
func scriptRefusal(mounts []string, written []string, holds bool, view exportView) string {
	for _, p := range written {
		for _, q := range []string{p, realPath(p)} {
			at := p
			if q != p {
				at = p + " (" + q + ")"
			}
			if m := view.missing(mounts, q); m != "" {
				return at + ", on dev's " + m + " mount, which is not exported"
			}
			if d := within(append(privateDirs, runAliases...), q); d != "" {
				return at + ", under " + d + ", which the job sees as a private directory removed when it ends"
			}
			if h := hostOnlyAs(p, q); h != "" {
				return h + ", which a remote has its own of"
			}
			if m := holding(mounts, q); holds && m != "" {
				return at + ", which holds dev's " + m + " mount, which is not exported"
			}
		}
	}
	return ""
}

// scriptPaths are the absolute paths written in s (a shell script's `cd /mnt && ls`,
// `${D:-/mnt}`, `{/mnt,/etc}`), cleaned so `..` and `//` are resolved. A path starts at a
// `/` that follows no name character and none of `~ } ) * ? ] % #`, which end a home, a
// variable or a glob (`~/run`, `${HOME}/run`, `x*/run`, `${f%/*}` are not /run), nor a
// quote that closes a word (`"$d"/run`), unless the name characters are a short option's
// letters (`-C/mnt`, `-cC/mnt`, `CFLAGS=-I/mnt`, `-Wl,-L/mnt`); it ends at a blank, a
// quote or shell punctuation.
// One cut short by a glob, a brace or a variable (`/mnt/*`, `/mnt/$X`, `/*`) stands for
// its directory, which the job may walk; a `$` names a variable only before a name
// character, `{` or `(`, so `/^$/` is no variable. A bare `/` counts when cut so, or when
// it stands as a whole word (`find /`, `cd / &&`, `'/'`, `tar -C/`) outside `((…))`
// arithmetic, but not as a delimiter (`cut -d/`, `awk -F/`, `sort -t/`, `column -s/`).
// A bracket glob at the root (`/[t]mp/x`) stands for each path it matches on dev.
// Backslashes are dropped first (`\/dev/shm`).
func scriptPaths(s string) []string {
	s = strings.ReplaceAll(s, "\\", "")
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '/' || !pathStart(s, i) {
			continue
		}
		end := i + 1
		for end < len(s) && !(strings.IndexByte(" \t\n'\"`;&|()<>=:,{}*?[]", s[end]) >= 0 || isVariable(s, end)) {
			end++
		}
		p := s[i:end]
		cut := end < len(s) && (strings.IndexByte("{*?[", s[end]) >= 0 || isVariable(s, end))
		if cut && !strings.HasSuffix(p, "/") {
			p = filepath.Dir(p)
		}
		if p = filepath.Clean(p); p != "/" || cut && s[end] != '[' || wholeWord(s, i, end) {
			out = append(out, p)
		} else if cut && s[end] == '[' {
			// `/[t]mp/x` names what it matches on dev, as the shell expands it; a regex's
			// class (`/[0-9]+/`) matches nothing and names nothing.
			w := end
			for w < len(s) && strings.IndexByte(" \t\n'\"`;&|()<>=:,{}", s[w]) < 0 && !isVariable(s, w) {
				w++
			}
			matches, _ := filepath.Glob(s[i:w])
			out = append(out, matches...)
			end = w
		}
		i = end - 1
	}
	return out
}

// expansionPrefix is p up to its first glob character, brace or variable, and whether it
// has one.
func expansionPrefix(p string) (string, bool) {
	for i := 0; i < len(p); i++ {
		if strings.IndexByte("{*?[", p[i]) >= 0 || isVariable(p, i) {
			return p[:i], true
		}
	}
	return p, false
}

// holding returns the first of mounts strictly below the directory p, or "".
func holding(mounts []string, p string) string {
	prefix := strings.TrimSuffix(p, "/") + "/"
	for _, m := range mounts {
		if strings.HasPrefix(m, prefix) {
			return m
		}
	}
	return ""
}

// squashPath collapses every `//` and `/./` in s, so `/dev//shm` and `/dev/./shm` read
// as /dev/shm.
func squashPath(s string) string {
	for r := strings.NewReplacer("//", "/", "/./", "/"); ; {
		next := r.Replace(s)
		if next == s {
			return s
		}
		s = next
	}
}

// pathCandidates are the paths an argument word may name: the word, its value after the
// first `=`, and a short option's attached value (`-o<path>`); and of each, the path of a
// URL (urlPath), and every piece of a `:` or `,` list (`--path=a:b`, `file:x`), with a
// leading `@` dropped (`@args`). Empty
// ones are dropped, and so is one longer than PATH_MAX, which names no path; a list longer
// than that still has its pieces read.
func pathCandidates(w string) []string {
	var out []string
	add := func(c string) {
		pieces := append([]string{c}, strings.FieldsFunc(c, func(r rune) bool { return r == ':' || r == ',' })...)
		if u := urlPath(c); u != "" {
			pieces = append(pieces, u)
		}
		for _, piece := range pieces {
			if piece = strings.TrimPrefix(piece, "@"); piece != "" && len(piece) <= 4096 {
				out = append(out, piece)
			}
		}
	}
	add(w)
	if _, v, ok := strings.Cut(w, "="); ok {
		add(v)
	}
	if len(w) > 2 && w[0] == '-' && w[1] != '-' {
		add(w[2:])
	}
	return out
}

// urlScheme is the scheme and colon of a URL naming a local file (`file:`, `sqlite:`,
// `sqlite+aiosqlite:`). Other schemes name a host's resource, not a path on dev.
var urlScheme = regexp.MustCompile(`^(file|sqlite(\+[A-Za-z0-9_.-]+)?):`)

// urlPath is the path a file or SQLite URL names, its query and fragment dropped, or "":
// `file:../x?mode=rw` is ../x, `file://host/x` and `file:///x` are /x, and SQLAlchemy's
// `sqlite:///rel.db` is relative and `sqlite:////abs.db` absolute.
func urlPath(w string) string {
	scheme := urlScheme.FindString(w)
	if scheme == "" {
		return ""
	}
	rest, _, _ := strings.Cut(w[len(scheme):], "?")
	rest, _, _ = strings.Cut(rest, "#")
	if r, err := url.PathUnescape(rest); err == nil {
		rest = r // `my%20db.sqlite`
	}
	switch {
	case strings.HasPrefix(scheme, "sqlite") && strings.HasPrefix(rest, "///"):
		return rest[3:]
	case strings.HasPrefix(rest, "//"):
		if i := strings.IndexByte(rest[2:], '/'); i >= 0 {
			return rest[2+i:]
		}
		return ""
	}
	return rest
}

// realPath is p with the symlinks of its longest existing prefix resolved, the rest kept.
// It resolves from the raw path, so `link/..` climbs from the link's target as the kernel
// does. A dangling symlink is followed to the path it names, where a write through it
// would create the file.
func realPath(p string) string {
	for hops := 0; hops < 40; hops++ {
		target, err := os.Readlink(p)
		if err != nil {
			break
		}
		if _, err := os.Stat(p); err == nil {
			break // it resolves, so EvalSymlinks below follows it
		}
		if !filepath.IsAbs(target) {
			// Kept raw, not cleaned, so a `..` in the target climbs from where the link's
			// directory really is, as the kernel and EvalSymlinks walk it.
			dir := "."
			if k := strings.LastIndexByte(p, '/'); k >= 0 {
				dir = p[:k+1]
			}
			target = dir + "/" + target
		}
		p = target
	}
	parts := strings.Split(p, "/")
	for k := len(parts); k > 0; k-- {
		prefix := strings.Join(parts[:k], "/")
		if prefix == "" {
			prefix = "/"
		}
		if r, err := filepath.EvalSymlinks(prefix); err == nil {
			return filepath.Join(append([]string{r}, parts[k:]...)...)
		}
	}
	return filepath.Clean(p)
}

// namesDir returns the first of dirs that s mentions as a path: the name starts s or
// follows a separator (space, quote, `=`, `:`, `>`, `/` and the like, never a name
// character, so `~/code/tmp` does not count), and ends s or is followed by one, so
// `/runner` is not `/run`.
func namesDir(s string, dirs []string) string {
	for _, d := range dirs {
		for i := 0; ; {
			j := strings.Index(s[i:], d)
			if j < 0 {
				break
			}
			j += i
			end := j + len(d)
			if (j == 0 || strings.IndexByte(" \t\n'\"`=:,;|&<>([{@/", s[j-1]) >= 0) &&
				(end == len(s) || !isNameByte(s[end])) {
				return d
			}
			i = j + 1
		}
	}
	return ""
}

// specialFileRefusal names a FIFO, socket or device node outside /dev, /proc and /sys
// that an argument names, or a FIFO a variable names, or "": the export carries the node,
// not what is behind it, so a remote job would open the remote's own end of it. A FIFO's
// has no writer or reader there, and the job blocks forever. Arguments and values are read
// as argPaths.fileWords reads them. A variable naming a socket is left alone: the session's own
// (SSH_AUTH_SOCK, the bus) are in every environment, and a remote job fails at once on one.
func specialFileRefusal(cwd, home string, argv, env []string) string {
	// special names the first node of kinds that s names.
	special := func(s string, kinds fs.FileMode) string {
		for _, p := range readArg(s, cwd, home).fileWords() {
			st, err := os.Stat(p)
			if err != nil || st.Mode()&kinds == 0 || within([]string{"/dev", "/proc", "/sys"}, realPath(p)) != "" {
				continue
			}
			kind := "a device node"
			switch {
			case st.Mode()&fs.ModeNamedPipe != 0:
				kind = "a FIFO"
			case st.Mode()&fs.ModeSocket != 0:
				kind = "a socket"
			}
			return fmt.Sprintf("%s is %s on dev, which a remote job would open as the remote's own", p, kind)
		}
		return ""
	}
	for _, a := range argv {
		if r := special(a, fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice); r != "" {
			return r
		}
	}
	for _, e := range env {
		_, v, _ := strings.Cut(e, "=")
		if r := special(v, fs.ModeNamedPipe); r != "" {
			return r
		}
	}
	return ""
}
