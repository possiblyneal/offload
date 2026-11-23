package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// pathStart reports whether the `/` at s[i] can begin an absolute path (scriptPaths).
func pathStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	if _, ok := optionLetters(s, i); ok {
		return true
	}
	b := s[i-1]
	if isNameByte(b) && b != '-' || strings.IndexByte("~})*?]%#", b) >= 0 {
		return false
	}
	if b == '"' || b == '\'' {
		// The quote opens a string unless it closes one, which follows a name byte or a
		// closing bracket (`"$d"/run`, `"${d}"/run`, `"$(x)"/run`): so `["/dev/sda"]`,
		// `{"p":"/sys/x"}` and `>"/dev/tty"` begin a path.
		return i == 1 || !isNameByte(s[i-2]) && strings.IndexByte("})]", s[i-2]) < 0
	}
	return true
}

// isVariable reports whether the `$` at s[i] begins a variable or a substitution.
func isVariable(s string, i int) bool {
	return s[i] == '$' && i+1 < len(s) && (isNameByte(s[i+1]) && s[i+1] != '-' && s[i+1] != '.' || s[i+1] == '{' || s[i+1] == '(')
}

// wholeWord reports whether s[i:end] stands as a word of its own, bounded by the start or
// end, a blank, a quote or shell punctuation, and is not inside `((…))` arithmetic. Inside
// quotes it must be all the quote holds (`'/'`, a Python `"/"`), so a sed or awk program's
// `/` (`'s/x/ /'`) is not one; nor is a `/` that slashIsText finds is text.
func wholeWord(s string, i, end int) bool {
	if end < len(s) && strings.IndexByte(" \t\n'\"`;&|)<>}", s[end]) < 0 {
		return false
	}
	if letters, ok := optionLetters(s, i); ok {
		if len(letters) == 1 && strings.Contains("dFts", letters) {
			return false // a delimiter, no directory
		}
	} else if i > 0 && strings.IndexByte(" \t\n'\"`;&|(<>", s[i-1]) < 0 && !braceDefault(s, i) {
		return false
	}
	if o := openQuote(s, i); o >= 0 && (o != i-1 || end >= len(s) || s[end] != s[o]) {
		return false
	}
	cmd := strings.Fields(s[strings.LastIndexAny(s[:i], ";&|(`\n")+1 : i])
	for k := range cmd {
		cmd[k] = strings.Trim(cmd[k], `'"`)
	}
	if slashIsText(cmd, cmd) {
		return false
	}
	return strings.Count(s[:i], "((") <= strings.Count(s[:i], "))")
}

// braceDefault reports whether s[i] begins the word of a `${…}` expansion (`${ROOT:-/}`,
// `${D=/}`), which the shell may use in place of the variable.
func braceDefault(s string, i int) bool {
	j := strings.LastIndex(s[:i], "${")
	return j >= 0 && !strings.Contains(s[j:i], "}") && strings.IndexByte("-=+?", s[i-1]) >= 0
}

// openQuote returns the index of the quote s[i] lies inside, or -1. A command substitution
// still open at i (`"$(ls /)"`, a backtick) starts afresh: quotes outside it do not count.
func openQuote(s string, i int) int {
	o := -1
	for k := substStart(s, i); k < i; k++ {
		switch {
		case o >= 0 && s[k] == s[o]:
			o = -1
		case o < 0 && (s[k] == '\'' || s[k] == '"'):
			o = k
		}
	}
	return o
}

// substStart is the index just past the innermost `$(` or backtick still open at s[i], or 0.
func substStart(s string, i int) int {
	var open []int
	for k := 0; k < i; k++ {
		switch {
		case strings.HasPrefix(s[k:], "$("):
			open = append(open, k+2)
			k++
		case s[k] == '(' && len(open) > 0:
			open = append(open, -1) // a plain group inside, closed by its own `)`
		case s[k] == ')' && len(open) > 0:
			open = open[:len(open)-1]
		case s[k] == '`':
			if n := len(open); n > 0 && open[n-1] > 0 && s[open[n-1]-1] == '`' {
				open = open[:n-1]
			} else {
				open = append(open, k+1)
			}
		}
	}
	for n := len(open) - 1; n >= 0; n-- {
		if open[n] >= 0 {
			return open[n]
		}
	}
	return 0
}

// textTools never take a directory, so a bare `/` among their arguments is text: `tr ' ' /`,
// `echo /`, `expr 4 / 2`.
var textTools = map[string]bool{"tr": true, "echo": true, "printf": true, "expr": true,
	"sed": true, "awk": true, "gawk": true, "mawk": true, "cut": true}

// delimiterOptions are the options whose value is a field delimiter, by command: a `/`
// after one is text, where after `du -s` or `ls -d` it is the root.
var delimiterOptions = map[string][]string{"sort": {"-t", "--field-separator"},
	"column": {"-s", "-t", "--separator"}, "paste": {"-d", "--delimiters"}, "join": {"-t"}}

// slashIsText reports whether a bare `/` word that follows the words before is text, not
// the root directory: an argument of a textTools command, or the value of its delimiter
// option (`sort -t /`, `column -s /`), the command found in words behind its wrappers.
// Behind a shell it never is: `sh -c 'echo; find "$0"' /` passes the / to a script.
func slashIsText(before, words []string) bool {
	for _, w := range words {
		if isShell(filepath.Base(w)) {
			return false
		}
	}
	cmd := findCommand(words, func(cmd []string) string { return filepath.Base(cmd[0]) })
	return textTools[cmd] || len(before) > 0 && slices.Contains(delimiterOptions[cmd], before[len(before)-1])
}

// optionLetters returns the letters of a short option that s[i] directly follows (`-C`
// in `tar -C/x`, `cC` in `-cC/x`, `I` in `CFLAGS=-I/x` or `-Wl,-I/x`): a `-` that starts a
// word, or follows `=` or `,`, then letters only.
func optionLetters(s string, i int) (string, bool) {
	j := i
	for j > 0 && ('a' <= s[j-1] && s[j-1] <= 'z' || 'A' <= s[j-1] && s[j-1] <= 'Z') {
		j--
	}
	if j == i || j == 0 || s[j-1] != '-' {
		return "", false
	}
	if j-1 > 0 && strings.IndexByte(" \t\n;&|('\"=,", s[j-2]) < 0 {
		return "", false
	}
	return s[j:i], true
}

// isNameByte reports whether b can continue a file name such as `/tmpfoo` or `/tmp.x`.
func isNameByte(b byte) bool {
	return b == '.' || b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// looseWords splits s at every shell separator, quote and `=`, `:`, `,` or `?` (a URI's
// query: `file:../x.db?mode=ro`): the words a script may use as paths, and more besides,
// for checks that only match an existing file.
func looseWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(" \t\n'\"`;&|()<>=:,?{}", r) })
}

// scriptSteps reads a script (home and pwdForm already read) as the shell would split it
// into words, quotes and backslashes removed and brace lists expanded, and follows its
// `cd` and `pushd` in order (a bare one enters home, `-` the directory before): each word
// is the path it names from the directory the script is in at that point. A best effort,
// not a shell: a subshell's `cd` is taken to last.
func scriptSteps(script, cwd, home string) []string {
	dir, prev := cwd, cwd
	var out, stack []string // stack: pushd's directory stack, below dir
	tokens := shellTokens(script)
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.sep {
			continue
		}
		at := func(w string) string {
			if filepath.IsAbs(w) {
				return filepath.Clean(w)
			}
			return filepath.Join(dir, w)
		}
		if t.text == "popd" && commandPosition(tokens, i) {
			if n := len(stack); n > 0 {
				dir, prev, stack = stack[n-1], dir, stack[:n-1]
				out = append(out, dir)
			}
			continue
		}
		if (t.text == "cd" || t.text == "pushd") && commandPosition(tokens, i) {
			target := home
			for i+1 < len(tokens) && !tokens[i+1].sep && strings.HasPrefix(tokens[i+1].text, "-") && tokens[i+1].text != "-" {
				i++ // -P, -L
			}
			switch {
			case i+1 < len(tokens) && !tokens[i+1].sep:
				i++
				target = tokens[i].text
				if target == "-" {
					target = prev
				}
				target = at(target)
			case t.text == "pushd" && len(stack) > 0:
				target = stack[len(stack)-1] // a bare pushd swaps the top two
				stack = stack[:len(stack)-1]
			}
			if t.text == "pushd" {
				stack = append(stack, dir)
			}
			out = append(out, target)
			dir, prev = target, dir
			continue
		}
		for _, w := range braceExpand(t.text) {
			out = append(out, at(w))
		}
	}
	return out
}

// commandKeywords leave the word after them in command position: `if cd; then`, `{ cd; }`,
// `! cd`, `command cd`.
var commandKeywords = []string{"if", "then", "else", "elif", "do", "while", "until", "{", "!", "time", "command", "builtin"}

// commandPosition reports whether tokens[i] is a command's name: first, after a separator,
// or after a keyword or prefix that is itself in command position.
func commandPosition(tokens []shellToken, i int) bool {
	for ; i > 0 && !tokens[i-1].sep; i-- {
		if !slices.Contains(commandKeywords, tokens[i-1].text) {
			return false
		}
	}
	return true
}

// shellToken is a word of a script, or a separator between commands (`;`, `&&`, `|`, a
// newline, a parenthesis).
type shellToken struct {
	text string
	sep  bool
}

// shellTokens splits s at blanks and separators, keeping a quoted or escaped blank in its
// word and dropping the quotes. A redirection's `<` or `>` ends a word, but is no separator.
// A here-document's body is skipped, so a quote in it (`don't`) opens nothing.
func shellTokens(s string) []shellToken {
	var out []shellToken
	var w strings.Builder
	inWord := false
	arith := 0             // open `((`, where `<<` is a shift
	var heredocs []heredoc // here-documents whose bodies start at the next newline
	flush := func() {
		if inWord {
			out = append(out, shellToken{text: w.String()})
		}
		w.Reset()
		inWord = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i++ // a line continuation joins the lines
		case c == '\\' && i+1 < len(s):
			i++
			w.WriteByte(s[i])
			inWord = true
		case c == '#' && !inWord:
			// A comment, to the line's end: a quote in it (`# don't`) opens nothing.
			if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
				i += end - 1
			} else {
				i = len(s)
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			// An ANSI-C quote, where a backslash escapes the next character (`$'it\'s'`).
			end := ansiQuoteEnd(s, i)
			for j := i + 2; j < end; j++ {
				if s[j] == '\\' && j+1 < end {
					j++
				}
				w.WriteByte(s[j])
			}
			inWord = true
			i = end
		case c == '\'' || c == '"':
			// In double quotes a backslash escapes `"`, `\`, `$` and a backtick (`"echo \"x\""`).
			j := i + 1
			for ; j < len(s) && s[j] != c; j++ {
				if c == '"' && strings.HasPrefix(s[j:], "$(") {
					// A substitution's own quotes do not end the word: `"$(echo "a)"; id)"`.
					end := min(substitutionEnd(s, j), len(s)-1)
					w.WriteString(s[j : end+1])
					j = end
					continue
				}
				if c == '"' && s[j] == '\\' && j+1 < len(s) && strings.IndexByte("\"\\$`", s[j+1]) >= 0 {
					j++
				}
				w.WriteByte(s[j])
			}
			inWord = true
			i = j
		case strings.HasPrefix(s[i:], "<<<"):
			flush()
			i += 2 // a here-string
		case strings.HasPrefix(s[i:], "<<") && arith == 0:
			flush()
			if h, end, ok := heredocAt(s, i+2); ok {
				heredocs = append(heredocs, h)
				i = end - 1
			} else {
				i++
			}
		case c == ' ' || c == '\t' || c == '<' || c == '>':
			flush()
		case c == '\n' && len(heredocs) > 0:
			flush()
			out = append(out, shellToken{text: "\n", sep: true})
			for _, h := range heredocs {
				start := min(i+1, len(s)) // past the end once an earlier body ran out
				i = h.bodyEnd(s, start)
				if h.quoted {
					continue
				}
				// The body's substitutions run, each as commands of their own.
				for _, inner := range substitutions(s[start:i]) {
					out = append(out, shellToken{sep: true})
					out = append(out, shellTokens(inner)...)
					out = append(out, shellToken{sep: true})
				}
			}
			heredocs = nil
		case strings.HasPrefix(s[i:], "((") || strings.HasPrefix(s[i:], "))") && arith > 0:
			if c == '(' {
				arith++
			} else {
				arith--
			}
			flush()
			out = append(out, shellToken{text: string(c), sep: true}, shellToken{text: string(c), sep: true})
			i++
		case strings.IndexByte(";&|()\n`", c) >= 0:
			flush()
			out = append(out, shellToken{text: string(c), sep: true})
		default:
			w.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return out
}

// braceLimit bounds a word's brace expansion: `{a,b}` thirty times over is a billion
// words, which would stall the wrapper before placement.
const braceLimit = 4096

// braceExpand expands a word's brace lists (`a/{b,c}/d` is a/b/d and a/c/d), nested ones
// included, as the shell does; a word with none, or with more than braceLimit expansions,
// is itself (looseWords still reads its pieces).
func braceExpand(w string) []string {
	budget := braceLimit
	if out, ok := expandBraces(w, &budget); ok {
		return out
	}
	return []string{w}
}

// expandBraces is braceExpand's recursion, false once the expansions pass the budget.
func expandBraces(w string, budget *int) ([]string, bool) {
	open := strings.IndexByte(w, '{')
	if open < 0 {
		if *budget--; *budget < 0 {
			return nil, false
		}
		return []string{w}, true
	}
	depth, last := 0, open+1
	var parts []string
	for i := open; i < len(w); i++ {
		switch w[i] {
		case '{':
			depth++
		case ',':
			if depth == 1 {
				parts = append(parts, w[last:i])
				last = i + 1
			}
		case '}':
			if depth--; depth == 0 {
				if parts == nil {
					// `{}` or `{x}` is no list: kept as it is, its tail expanded.
					tails, ok := expandBraces(w[i+1:], budget)
					if !ok {
						return nil, false
					}
					var out []string
					for _, t := range tails {
						out = append(out, w[:i+1]+t)
					}
					return out, true
				}
				parts = append(parts, w[last:i])
				var out []string
				for _, p := range parts {
					more, ok := expandBraces(w[:open]+p+w[i+1:], budget)
					if !ok {
						return nil, false
					}
					out = append(out, more...)
				}
				return out, true
			}
		}
	}
	if *budget--; *budget < 0 {
		return nil, false
	}
	return []string{w}, true
}

// argPaths is one argument word read as the paths it may name, once for every rule that
// judges it. Besides its pathCandidates the word is read as a shell script, `~` and $HOME
// as home (expandHome) and pwdForm as the cwd.
type argPaths struct {
	cwd     string
	words   []string // pathCandidates, a relative one as written
	script  string   // the word with home and the cwd read
	written []string // the absolute paths written in the script (scriptPaths)
	steps   []string // each script word from where its cd steps reach (scriptSteps)
	loose   []string // looseWords of the script, a relative one as written
}

func readArg(a, cwd, home string) argPaths {
	script := pwdForm.ReplaceAllLiteralString(expandHome(a, home), cwd)
	return argPaths{
		cwd:     cwd,
		words:   pathCandidates(a),
		script:  script,
		written: scriptPaths(script),
		steps:   scriptSteps(script, cwd, home),
		loose:   looseWords(script),
	}
}

// candidates are the word's pathCandidates, a relative one from the cwd.
func (r argPaths) candidates() []string {
	out := make([]string, len(r.words))
	for i, c := range r.words {
		if !filepath.IsAbs(c) {
			c = r.cwd + "/" + c
		}
		out[i] = c
	}
	return out
}

// fileWords are the absolute paths the word may name, for checks that only match an
// existing file: its candidates, loose words and steps, a relative one from the cwd and
// from every directory the word names (a script's `cd sub && cat f`), and the files a word
// with a glob matches.
func (r argPaths) fileWords() []string {
	cwd := r.cwd
	// Deduplicated: the product of words and bases is what a long script makes slow.
	words := slices.Compact(slices.Sorted(slices.Values(slices.Concat(r.words, r.loose, r.steps))))
	bases := []string{cwd}
	for _, w := range words {
		d := w
		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			bases = append(bases, d)
		}
	}
	bases = slices.Compact(slices.Sorted(slices.Values(bases)))
	var out []string
	for _, w := range words {
		var ps []string
		if filepath.IsAbs(w) {
			ps = []string{w}
		} else {
			for _, b := range bases {
				ps = append(ps, filepath.Join(b, w))
			}
		}
		for _, p := range ps {
			out = append(out, p)
			if strings.ContainsAny(p, "*?[") {
				m, _ := filepath.Glob(p)
				out = append(out, m...)
			}
		}
	}
	return out
}

// findCommand calls match on each command the words run, and returns its first non-empty
// result: the command behind the wrappers its switch names (sudo, su, doas, pkexec and run0
// each offered to match itself first) and `VAR=` assignments, or each command of a script
// a wrapper runs (a shell's -c, `npx -c`, `pnpm -c exec`, `sg`, `script -c`), after a
// separator or a keyword such as `then`. It walks the leading words of one command, skipping
// what only prefixes it.
func findCommand(w []string, match func(cmd []string) string) string {
	for len(w) > 0 {
		base, call := filepath.Base(w[0]), npmCallScript(w)
		if call == "" {
			call = execShellScript(w)
		}
		switch {
		case isAssignment(w[0]) || shellKeywords[w[0]]:
			w = w[1:]
		case base == "env":
			w = envCommand(w[1:])
		case base == "nice":
			w = skipOptions(w[1:], "-n", "--adjustment")
		case base == "exec":
			w = skipOptions(w[1:], "-a")
		case base == "command" && slices.ContainsFunc(leadingOptions(w[1:]), func(o string) bool { return strings.ContainsAny(o, "vV") }):
			return "" // `command -v x` looks x up and runs nothing
		case base == "command" || base == "builtin" || base == "coproc" || base == "nohup" || base == "setsid":
			w = skipOptions(w[1:])
		case becomesRoot(base):
			// match sees the wrapper word itself, as usesSudo needs, then what it runs as
			// another user.
			if m := match(w); m != "" {
				return m
			}
			if base == "su" {
				// The shell may be any program (-s), so su's own -c script is read as a
				// script whatever the shell is.
				cmd, script := suCommand(w[1:])
				if m := findCommand(cmd, match); m != "" || script == "" {
					return m
				}
				return findInScript(script, match)
			}
			w = skipOptions(w[1:], privilegeOptions[base]...)
		case call != "":
			// match sees npm's own words (a listed `npx`), then the script it runs.
			if m := match(w); m != "" {
				return m
			}
			return findInScript(call, match)
		case base == "time":
			w = skipOptions(w[1:], "-f", "--format", "-o", "--output") // GNU time; bash's takes only -p
		case base == "bundle" && len(w) > 1 && w[1] == "exec":
			w = skipOptions(w[2:], "--gemfile")
		case (base == "uv" || base == "poetry") && runArgs(w[1:]) != nil:
			opts := runArgs(w[1:])
			w = skipRunOptions(opts)
			if base == "uv" && len(w) > 0 && runsModule(opts[:len(opts)-len(w)]) {
				w = append([]string{"python", "-m"}, w...)
			}
		case base == "uvx" || base == "uv" && toolRun(w[1:]) != nil:
			if base == "uv" {
				w = toolRun(w[1:])
			}
			w = requirementProgram(skipExactOptions(w[1:], toolValueOptions...))
		case base == "pipx" && len(skipOptions(w[1:])) > 0 && skipOptions(w[1:])[0] == "run": // pipx's own options are switches
			w = requirementProgram(skipOptions(skipOptions(w[1:])[1:], "--spec", "--python", "-i", "--index-url", "--pip-args"))
		case base == "stdbuf":
			w = skipOptions(w[1:], "-i", "-o", "-e", "--input", "--output", "--error")
		case base == "ionice":
			w = skipOptions(w[1:], "-c", "-n", "-p", "-P", "-u", "--class", "--classdata", "--pid", "--pgid", "--uid")
		case base == "taskset":
			if w = skipOptions(w[1:]); len(w) > 0 {
				w = w[1:] // the mask or CPU list
			}
		case base == "chrt":
			if w = skipOptions(w[1:], "-T", "-P", "-D", "--sched-runtime", "--sched-period", "--sched-deadline"); len(w) > 0 {
				w = w[1:] // the priority
			}
		case base == "flock": // flock [options] file (command... | -c script)
			if w = skipOptions(w[1:], "-w", "-E", "--timeout", "--wait", "--conflict-exit-code"); len(w) > 0 {
				w = w[1:] // the lock file
			}
			if len(w) > 1 && (w[0] == "-c" || w[0] == "--command") {
				return findInScript(w[1], match)
			}
		case base == "xargs":
			w = skipOptions(w[1:], "-a", "-d", "-E", "-I", "-L", "-n", "-P", "-s",
				"--arg-file", "--delimiter", "--max-lines", "--max-args", "--max-procs", "--max-chars", "--process-slot-var")
		case base == "timeout":
			if w = skipOptions(w[1:], "-s", "--signal", "-k", "--kill-after"); len(w) > 0 {
				w = w[1:] // the duration
			}
		case base == "setpriv":
			w = skipOptions(w[1:], setprivValueOptions...)
		case base == "prlimit": // a limit's value is attached only (`-n4096`, `--nofile=4096`)
			w = skipOptions(w[1:], "-p", "--pid", "-o", "--output")
		case base == "systemd-run":
			w = skipOptions(w[1:], systemdRunValueOptions...)
		case base == "busybox" && len(w) > 1 && !strings.HasPrefix(w[1], "-"):
			w = w[1:] // the applet
		case base == "sg" && sgScript(w[1:]) != "":
			return findInScript(sgScript(w[1:]), match)
		case base == "script" && scriptCommand(w[1:]) != "":
			return findInScript(scriptCommand(w[1:]), match)
		case isShell(base):
			// Options to the script, or to the script's name: each o or O in a cluster
			// (`-euo pipefail`) takes the next word, and c makes the first operand a script.
			script, i := false, 1
			for ; i < len(w) && len(w[i]) > 1 && (w[i][0] == '-' || w[i][0] == '+'); i++ {
				a := w[i]
				switch {
				case a == "--":
					i++
				case a == "--rcfile" || a == "--init-file":
					i++
					continue
				case base == "fish" && fishCommand(a):
					// fish's --command, any unique prefix of it, as -c.
					if _, v, eq := strings.Cut(a, "="); eq {
						return findInScript(v, match)
					}
					script = true
					continue
				case strings.HasPrefix(a, "--"):
					continue
				default:
					script = script || a[0] == '-' && strings.Contains(a, "c")
					i += strings.Count(a, "o") + strings.Count(a, "O")
					continue
				}
				break
			}
			if script && i < len(w) {
				return findInScript(w[i], match)
			}
			return match(w)
		default:
			return match(w)
		}
	}
	return ""
}

// setprivValueOptions are util-linux 2.41 setpriv's options that take a value.
var setprivValueOptions = []string{"--ambient-caps", "--inh-caps", "--bounding-set", "--ruid", "--euid",
	"--rgid", "--egid", "--reuid", "--regid", "--groups", "--securebits", "--pdeathsig", "--ptracer",
	"--selinux-label", "--apparmor-profile", "--landlock-access", "--landlock-rule", "--seccomp-filter"}

// systemdRunValueOptions are systemd 257 systemd-run's options that take a value.
var systemdRunValueOptions = []string{"-C", "-H", "-M", "-E", "-p", "-u", "--capsule", "--host", "--machine",
	"--setenv", "--property", "--unit", "--description", "--slice", "--expand-environment", "--service-type",
	"--uid", "--gid", "--nice", "--working-directory", "--json", "--background", "--path-property",
	"--socket-property", "--timer-property", "--on-active", "--on-boot", "--on-startup", "--on-unit-active",
	"--on-unit-inactive", "--on-calendar"}

// sgScript is the script `sg [-] group [-c] command` hands to sh -c, its first word after
// the group, or "" with none.
func sgScript(w []string) string {
	if len(w) > 0 && w[0] == "-" {
		w = w[1:]
	}
	if len(w) > 1 && w[1] == "-c" {
		w = w[1:]
	}
	if len(w) < 2 {
		return ""
	}
	return w[1]
}

// scriptCommand is the script util-linux 2.41 script runs with -c (`-c x`, `-qc x`, `-cx`,
// `--command=x`, `--comm x`; of several the last), or "". Its getopt permutes, so an option
// may follow the typescript file.
func scriptCommand(w []string) string {
	values := []string{"-I", "-O", "-B", "-T", "-m", "-E", "-o", "--log-in", "--log-out", "--log-io",
		"--log-timing", "--logging-format", "--echo", "--output-limit"}
	command := ""
	for i := 0; i < len(w); i++ {
		o := w[i]
		switch {
		case o == "--":
			return command
		case strings.HasPrefix(o, "--"):
			name, v, eq := strings.Cut(o, "=")
			switch {
			case len(name) > 2 && strings.HasPrefix("--command", name) && eq:
				command = v
			case len(name) > 2 && strings.HasPrefix("--command", name) && i+1 < len(w):
				i++
				command = w[i]
			case !eq && takesNext(o, values):
				i++
			}
		case strings.HasPrefix(o, "-") && len(o) > 1:
			for j := 1; j < len(o); j++ {
				if o[j] == 't' { // -t takes an attached value only
					break
				}
				if !slices.Contains(values, "-"+o[j:j+1]) && o[j] != 'c' {
					continue
				}
				v := o[j+1:]
				if v == "" && i+1 < len(w) {
					i++
					v = w[i]
				}
				if o[j] == 'c' {
					command = v
				}
				break
			}
		}
	}
	return command
}

// leadingOptions are w's words before its first operand or `--`.
func leadingOptions(w []string) []string {
	i := 0
	for i < len(w) && strings.HasPrefix(w[i], "-") && w[i] != "--" {
		i++
	}
	return w[:i]
}

// envCommand is the command env's arguments run: the words after its options. A -S string
// (`env -S 'vite --port 1'`) is split into words that env reads on from where it stood,
// options first.
func envCommand(w []string) []string {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
		o := w[0]
		w = w[1:]
		if o == "--" {
			return w
		}
		if name, v, eq := strings.Cut(o, "="); strings.HasPrefix(o, "--") {
			// env takes any unambiguous prefix; each of these is one from its third letter.
			is := func(full string) bool { return len(name) > 2 && strings.HasPrefix(full, name) }
			switch {
			case is("--split-string") && eq:
				w = append(splitString(v), w...)
			case is("--split-string") && len(w) > 0:
				w = append(splitString(w[0]), w[1:]...)
			case !eq && (is("--unset") || is("--chdir") || is("--argv0")) && len(w) > 0:
				w = w[1:]
			}
			continue
		}
		// A short cluster ends at its first letter that takes a value: the rest of the
		// word, else the next word.
		if j := strings.IndexAny(o[1:], "uCaS"); j >= 0 {
			v := o[j+2:]
			if v == "" && len(w) > 0 {
				v, w = w[0], w[1:]
			}
			if o[j+1] == 'S' {
				w = append(splitString(v), w...)
			}
		}
	}
	if len(w) > 0 && w[0] == "-" {
		w = w[1:] // `env - cmd`, as -i
	}
	return w
}

// splitString is the words of env's -S value, which env splits as the shell does, quotes
// and escapes included (`-S "FOO='a b' sudo id"` is three words); a separator such as `;`
// is only a word to env.
func splitString(v string) []string {
	var words []string
	for _, t := range shellTokens(v) {
		words = append(words, t.text)
	}
	return words
}

// fishCommand reports whether a is fish's --command, by a prefix getopt_long takes for it
// (`--c` is unique among fish's options), with or without its `=value`.
func fishCommand(a string) bool {
	name, _, _ := strings.Cut(a, "=")
	return len(name) >= 3 && strings.HasPrefix("--command", name)
}

var shellKeywords = map[string]bool{"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true}

func isShell(base string) bool {
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "yash", "posh", "fish", "csh", "tcsh":
		return true
	}
	return false
}

// isAssignment reports whether w is a shell `NAME=value` word.
func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// skipOptions drops the leading option words of a wrapper: those starting with "-", and
// the word after each that leaves its value to it (takesNext); a lone "--" ends them.
func skipOptions(w []string, withArg ...string) []string {
	return skipWords(w, func(o, _ string) bool { return takesNext(o, withArg) })
}

// skipRunOptions is skipExactOptions over runValueOptions for uv and poetry.
func skipRunOptions(w []string) []string {
	return skipExactOptions(w, runValueOptions...)
}

// skipExactOptions is skipOptions for a parser that takes a long option only spelt in
// full (uv, poetry): `--refresh` is a flag of its own, not a prefix of
// `--refresh-package`.
func skipExactOptions(w []string, withArg ...string) []string {
	return skipWords(w, func(o, _ string) bool { return exactTakesNext(o, withArg) })
}

// exactTakesNext is takesNext for a parser that takes a long option only spelt in full.
func exactTakesNext(o string, withArg []string) bool {
	if strings.HasPrefix(o, "--") {
		return slices.Contains(withArg, o)
	}
	return takesNext(o, withArg)
}

// execShellScript is the command line of `pnpm -c|--shell-mode exec <words>`, which pnpm
// 9.15 runs through a shell when the option comes before exec (after it, exec rejects
// it), or of `bun exec <script>`, which bun's documentation says it runs in Bun's shell
// (unverified: bun is not installed here); else "".
func execShellScript(w []string) string {
	if filepath.Base(w[0]) == "bun" && len(w) > 2 && w[1] == "exec" {
		return strings.Join(w[2:], " ")
	}
	if filepath.Base(w[0]) != "pnpm" {
		return ""
	}
	shell := false
	for i := 1; i < len(w); i++ {
		o := w[i]
		if !strings.HasPrefix(o, "-") || o == "--" {
			if !shell || o != "exec" {
				return ""
			}
			return strings.Join(skipWords(w[i+1:], func(o, next string) bool { return pnpmTakesNext(o, next, nil) }), " ")
		}
		name, v, eq := strings.Cut(o, "=")
		if name == "--shell-mode" || !strings.HasPrefix(name, "--") && strings.ContainsRune(name, 'c') {
			shell = !eq || v != "false"
		}
		if !eq && i+1 < len(w) && pnpmTakesNext(o, w[i+1], nil) {
			i++
		}
	}
	return ""
}

// prefixed is each word of words with prefix before it, then extra.
func prefixed(prefix, words string, extra ...string) []string {
	var out []string
	for _, w := range strings.Fields(words) {
		out = append(out, prefix+w)
	}
	return append(out, extra...)
}

// runsModule reports whether uv run's option words include -m/--module, which runs the
// command as a Python module (`uv run -m http.server` is `python -m http.server`).
func runsModule(opts []string) bool {
	for _, o := range opts {
		if o == "--module" {
			return true
		}
		if !strings.HasPrefix(o, "-") || strings.HasPrefix(o, "--") {
			continue
		}
		for _, c := range o[1:] {
			if c == 'm' {
				return true
			}
			if slices.Contains(runValueOptions, "-"+string(c)) {
				break // the rest of the word is this option's value
			}
		}
	}
	return false
}

func skipWords(w []string, takes func(o, next string) bool) []string {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
		if w[0] == "--" {
			return w[1:]
		}
		if len(w) > 1 && takes(w[0], w[1]) {
			w = w[2:]
		} else {
			w = w[1:]
		}
	}
	return w
}

// takesNext reports whether the option word o leaves its value to the next word: o is one
// of withArg, or a cluster of short options whose first one in withArg is its last letter
// (`-ao FILE`; in `-n5` the value is the rest of the word).
func takesNext(o string, withArg []string) bool {
	if slices.Contains(withArg, o) {
		return true
	}
	if strings.HasPrefix(o, "--") {
		// getopt_long takes any unambiguous prefix (`nice --adj 5`); one a tool runs at all
		// is unambiguous, so a prefix of a value option is that option.
		return len(o) > 2 && !strings.Contains(o, "=") && slices.ContainsFunc(withArg, func(a string) bool {
			return strings.HasPrefix(a, "--") && strings.HasPrefix(a, o)
		})
	}
	for i := 1; i < len(o); i++ {
		if slices.Contains(withArg, "-"+o[i:i+1]) {
			return i == len(o)-1
		}
	}
	return false
}

// runValueOptions are the options of `uv run` (uv 0.12, its global ones included) and
// poetry's global `-C`/`--directory` and `-P`/`--project` that take a value, with the
// hidden options and aliases uv 0.12.1 accepts though `--help` lists none of them.
var runValueOptions = []string{"--extra", "--no-extra", "--group", "--no-group", "--only-group",
	"--no-editable-package", "--env-file", "-w", "--with", "--with-editable", "--with-requirements", "--package",
	"--python-platform", "--index", "--default-index", "-i", "--index-url", "--extra-index-url", "-f",
	"--find-links", "--index-strategy", "--keyring-provider", "-P", "--upgrade-package", "--upgrade-group",
	"--resolution", "--prerelease", "--prerelease-package", "--fork-strategy", "--exclude-newer",
	"--exclude-newer-package", "--no-sources-package", "--reinstall-package", "--link-mode", "-C",
	"--config-setting", "--config-settings-package", "--no-build-isolation-package", "--no-build-package",
	"--no-binary-package", "--cache-dir", "--refresh-package", "-p", "--python", "--color",
	"--allow-insecure-host", "--directory", "--project", "--config-file",
	"--python-preference", "--python-fetch", "--preview-features", "--preview-feature", "--trusted-host", "--config-settings"}

// toolValueOptions are the options of `uv tool run` and `uvx` (uv 0.12) that take a value:
// `uv run`'s and the tool's own requirement sources.
var toolValueOptions = append(slices.Clip(runValueOptions), "--from", "-c", "--constraints", "--constraint",
	"-b", "--build-constraints", "--build-constraint", "--overrides", "--override", "--torch-backend")

// requirementProgram returns w with its first word, a Python requirement that uvx and
// pipx run read (`mkdocs==1.6`, `mkdocs[i18n]`, `mkdocs(>=1.5)`, `ruff@latest`), cut to
// the package name whose executable they run: PEP 508's name (letters, digits, `.`, `_`,
// `-`) followed by an extras `[`, or by what uv 0.12.1's parser expects after one. A word
// whose name is followed by anything else (a URL, a path) is left whole.
func requirementProgram(w []string) []string {
	if len(w) == 0 {
		return w
	}
	i := strings.IndexFunc(w[0], func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r))
	})
	if i > 0 && strings.ContainsRune("[@(<=>~!; \t", rune(w[0][i])) {
		return append([]string{w[0][:i]}, w[1:]...)
	}
	return w
}

// runArgs is what follows `run` in uv's or poetry's words after their own options, or nil.
func runArgs(w []string) []string {
	if w = skipRunOptions(w); len(w) == 0 || w[0] != "run" {
		return nil
	}
	return w[1:]
}

// toolRun returns w (uv's arguments) from `run` on when they are `uv [options] tool
// [options] run`, else nil.
func toolRun(w []string) []string {
	if w = skipRunOptions(w); len(w) < 2 || w[0] != "tool" {
		return nil
	}
	if w = skipRunOptions(w[1:]); len(w) == 0 || w[0] != "run" && w[0] != "uvx" { // `uv tool uvx` is run's alias
		return nil
	}
	return w
}

// becomesRoot reports whether name is a way of becoming root that runs a command
// (sudoedit runs none, so usesSudo adds it).
func becomesRoot(name string) bool {
	_, ok := privilegeOptions[name]
	return ok
}

// privilegeOptions are the options of each way of becoming root that take a value; su's
// are read by suCommand.
var privilegeOptions = map[string][]string{
	"su": nil,
	"sudo": {"-u", "-g", "-h", "-p", "-C", "-D", "-R", "-T", "-U", "-r", "-t", "--user", "--group", "--host",
		"--prompt", "--close-from", "--chdir", "--chroot", "--command-timeout", "--other-user", "--role", "--type"},
	"doas":   {"-u", "-C"},
	"pkexec": {"--user"},
	"run0": {"-u", "--user", "-g", "--group", "-D", "--chdir", "--unit", "--property", "--description",
		"--slice", "--setenv", "--nice", "--machine", "--background", "--shell-prompt-prefix"},
}

// suCommand is the command su runs, and su's own -c script, read from su's arguments
// as its getopt reads them: the shell (the last `-s`/`--shell`, else sh standing in for
// the user's), then `-c script`
// when su was given one (`-c x`, `-lc x`, `-cx`, `--command=x`, `--comm x`,
// `--session-command x`; of several the last), then the words su hands the shell: its
// operands after the optional `-` and the user, then all after `--`. A short cluster ends
// at its first letter that takes a value, and a long option may be any unambiguous prefix.
func suCommand(w []string) (cmd []string, script string) {
	shell, args := "sh", []string{}
	for i := 0; i < len(w); i++ {
		x := w[i]
		if x == "--" {
			args = append(args, w[i+1:]...)
			break
		}
		next := ""
		if i+1 < len(w) {
			next = w[i+1]
		}
		if name, v, eq := strings.Cut(x, "="); strings.HasPrefix(x, "--") {
			is := func(full string, min int) bool { return len(name) >= min && strings.HasPrefix(full, name) }
			command, sh := is("--command", 3) || is("--session-command", 4), is("--shell", 4)
			if !command && !sh && !is("--group", 3) && !is("--supp-group", 4) && !is("--whitelist-environment", 3) {
				continue // takes no value
			}
			value := v
			if !eq {
				value = next
				i++
			}
			if command {
				script = value
			} else if sh {
				shell = value
			}
			continue
		}
		if !strings.HasPrefix(x, "-") || x == "-" {
			args = append(args, x)
			continue
		}
		if j := strings.IndexAny(x[1:], "csgGw"); j >= 0 {
			value := x[j+2:]
			if value == "" {
				value = next
				i++
			}
			switch x[j+1] {
			case 'c':
				script = value
			case 's':
				shell = value
			}
		}
	}
	if len(args) > 0 && args[0] == "-" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:] // the user
	}
	cmd = []string{shell}
	if script != "" {
		cmd = append(cmd, "-c", script)
	}
	return append(cmd, args...), script
}

// findInScript is findCommand over each command of a shell script, its words split as the
// shell splits them (`FOO="a b" vite` is an assignment, then vite).
func findInScript(script string, match func(cmd []string) string) string {
	var cmd []string
	for _, t := range append(shellTokens(script), shellToken{sep: true}) {
		if t.sep || t.text == "{" || t.text == "}" {
			if r := findCommand(cmd, match); r != "" {
				return r
			}
			cmd = nil
			continue
		}
		// A substitution inside double quotes (`x="$(sudo cat f)"`) runs its commands too.
		for _, inner := range substitutions(t.text) {
			if r := findInScript(inner, match); r != "" {
				return r
			}
		}
		cmd = append(cmd, t.text)
	}
	return ""
}

// substitutions are the scripts of the `$(…)` and backtick substitutions in a word.
func substitutions(w string) []string {
	var out []string
	for i := 0; i < len(w); i++ {
		switch {
		case strings.HasPrefix(w[i:], "$("):
			j := substitutionEnd(w, i)
			out = append(out, w[i+2:min(j, len(w))])
			i = j
		case w[i] == '`':
			end := strings.IndexByte(w[i+1:], '`')
			if end < 0 {
				end = len(w) - i - 1
			}
			out = append(out, w[i+1:i+1+end])
			i += end + 1
		}
	}
	return out
}

// substitutionEnd is the index of the `)` that closes the `$(` at s[i], or len(s) when
// none does. As bash does, it skips quoted text, escaped characters, comments, `${…}`
// expansions, here-document bodies, and nested parentheses and substitutions, and does
// not close at a `)` that ends a `case` pattern (`$(case $x in a) id;; esac)`).
func substitutionEnd(s string, i int) int {
	depth := 0
	subEnd := -1           // the `)` that closed the last nested substitution or `$((`
	var arith []arithOpen  // each open `((`
	var cases []int        // the parenthesis depth each open `case` sits at
	var heredocs []heredoc // here-documents whose bodies start at the next newline
	for j := i + 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '\\':
			j++
		case c == '$' && j+1 < len(s) && s[j+1] == '\'':
			j = ansiQuoteEnd(s, j)
		case c == '\'':
			k := strings.IndexByte(s[j+1:], '\'')
			if k < 0 {
				return len(s)
			}
			j += k + 1
		case c == '"':
			j = doubleQuoteEnd(s, j)
		case strings.HasPrefix(s[j:], "$(") && !strings.HasPrefix(s[j:], "$(("):
			j = substitutionEnd(s, j)
			subEnd = j
		case c == '#' && strings.IndexByte(" \t\n;&|()", s[j-1]) >= 0 && j-1 != subEnd:
			// After a substitution's `)` a `#` is mid-word (`$(echo a)#b`), after any other
			// `)` a comment.
			k := strings.IndexByte(s[j:], '\n')
			if k < 0 {
				return len(s)
			}
			j += k - 1 // the newline still ends the line
		case strings.HasPrefix(s[j:], "${"):
			j = braceEnd(s, j+1)
		case strings.HasPrefix(s[j:], "<<<"):
			j += 2 // a here-string
		case strings.HasPrefix(s[j:], "<<") && len(arith) == 0:
			if h, end, ok := heredocAt(s, j+2); ok {
				heredocs = append(heredocs, h)
				j = end - 1
			}
		case c == '\n' && len(heredocs) > 0:
			for _, h := range heredocs {
				j = h.bodyEnd(s, j+1)
			}
			heredocs = nil
			j-- // the newline ending the last delimiter line ends a command too
		case strings.HasPrefix(s[j:], "(("): // arithmetic, where `<<` is a shift
			depth += 2
			arith = append(arith, arithOpen{depth, s[j-1] == '$'})
			j++
		case c == '(':
			depth++
		case c == ')' && len(arith) > 0 && arith[len(arith)-1].depth == depth && strings.HasPrefix(s[j:], "))"):
			if arith[len(arith)-1].expansion {
				subEnd = j + 1 // `$((1))#x` is one word
			}
			arith = arith[:len(arith)-1]
			if depth -= 2; depth == 0 {
				return j + 1
			}
			j++
		case c == ')':
			if n := len(cases); n > 0 && cases[n-1] == depth {
				continue // a case pattern's
			}
			if depth--; depth == 0 {
				return j
			}
		case commandWordAt(s, i+2, j, "case"):
			cases = append(cases, depth)
		case len(cases) > 0 && commandWordAt(s, i+2, j, "esac"):
			cases = cases[:len(cases)-1]
		}
	}
	return len(s)
}

// arithOpen is an open `((`: the parenthesis depth it sits at, and whether it is a `$((`
// expansion rather than an arithmetic command.
type arithOpen struct {
	depth     int
	expansion bool
}

// ansiQuoteEnd is the index of the `'` closing the ANSI-C quote `$'` at s[i], or len(s);
// a backslash in it escapes the next character.
func ansiQuoteEnd(s string, i int) int {
	j := i + 2
	for ; j < len(s) && s[j] != '\''; j++ {
		if s[j] == '\\' {
			j++
		}
	}
	return min(j, len(s))
}

// doubleQuoteEnd is the index of the `"` closing the one at s[i], or len(s), skipping
// escaped characters and nested substitutions.
func doubleQuoteEnd(s string, i int) int {
	j := i + 1
	for ; j < len(s) && s[j] != '"'; j++ {
		if s[j] == '\\' {
			j++
		} else if strings.HasPrefix(s[j:], "$(") {
			j = substitutionEnd(s, j)
		}
	}
	return j
}

// braceEnd is the index of the `}` closing the `{` at s[i] of a `${…}`, or len(s).
func braceEnd(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch {
		case s[j] == '\\':
			j++
		case strings.HasPrefix(s[j:], "$'"):
			j = ansiQuoteEnd(s, j)
		case s[j] == '\'':
			if k := strings.IndexByte(s[j+1:], '\''); k >= 0 {
				j += k + 1
			} else {
				return len(s)
			}
		case s[j] == '"':
			j = doubleQuoteEnd(s, j)
		case strings.HasPrefix(s[j:], "$("):
			j = substitutionEnd(s, j)
		case s[j] == '{':
			depth++
		case s[j] == '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return len(s)
}

// heredoc is a pending here-document: its delimiter, whether `<<-` strips the leading tabs
// of its lines, and whether the delimiter was quoted.
type heredoc struct {
	delim  string
	tabs   bool
	quoted bool // a quoted delimiter leaves the body as text; else it expands substitutions
}

// heredocAt reads the operator's word after the `<<` that ends at s[i]: `-`, blanks, then
// the delimiter, unquoted. It returns the index after the word, and false where the word
// is no here-document's (`1<<2`, an arithmetic shift).
func heredocAt(s string, i int) (heredoc, int, bool) {
	var h heredoc
	if i < len(s) && s[i] == '-' {
		h.tabs = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i == len(s) || s[i] >= '0' && s[i] <= '9' {
		return h, i, false
	}
	var d strings.Builder
	for ; i < len(s) && strings.IndexByte(" \t\n;&|()<>", s[i]) < 0; i++ {
		switch c := s[i]; c {
		case '\'', '"':
			h.quoted = true
			k := strings.IndexByte(s[i+1:], c)
			if k < 0 {
				k = len(s) - i - 1
			}
			d.WriteString(s[i+1 : i+1+k])
			i += k + 1
		case '\\':
			h.quoted = true
			if i+1 < len(s) {
				i++
				d.WriteByte(s[i])
			}
		default:
			d.WriteByte(c)
		}
	}
	h.delim = d.String()
	return h, i, h.delim != ""
}

// bodyEnd is the index of the newline ending h's delimiter line, for a body that starts
// at s[i], or len(s).
func (h heredoc) bodyEnd(s string, i int) int {
	for i < len(s) {
		end := strings.IndexByte(s[i:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += i
		}
		line := s[i:end]
		if h.tabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delim {
			return end
		}
		i = end + 1
	}
	return len(s)
}

// commandWordAt reports whether word starts s[j], a whole word in command position: first
// after start, after a separator, or after a reserved word that a command follows
// (`then case`), blanks aside.
func commandWordAt(s string, start, j int, word string) bool {
	if !strings.HasPrefix(s[j:], word) || j+len(word) < len(s) && strings.IndexByte(" \t\n;&|)", s[j+len(word)]) < 0 {
		return false
	}
	k := j - 1
	for k >= start && (s[k] == ' ' || s[k] == '\t') {
		k--
	}
	if k < start || strings.IndexByte(";&|(\n", s[k]) >= 0 {
		return true
	}
	for _, r := range commandLeads {
		if b := k + 1 - len(r); b >= start && s[b:k+1] == r && commandWordAt(s, start, b, r) {
			return true
		}
	}
	return false
}

// commandLeads are the reserved words a command can follow directly.
var commandLeads = []string{"if", "then", "do", "else", "elif", "while", "until", "{", "!", "time"}

// bareCd is a script's `cd` with no argument, which enters HOME.
var bareCd = regexp.MustCompile(`(^|[;&|(\s])cd\s*($|[;&|)])`)

// pwdForm is how a script names its starting directory, bare or alone in double quotes
// (`"$PWD"/x` is one path, as expandHome reads `"$HOME"/x`).
var pwdForm = regexp.MustCompile(`"(` + cwdWords + `)"|` + cwdWords)

// cwdWords are the spellings of the starting directory: $PWD, ${PWD}, ${PWD%/}, and pwd,
// pwd -P, pwd -L, realpath . and readlink -f . substituted by $(…) or backticks.
const cwdWords = `\$\{PWD(%/)?\}|\$PWD\b|\$\((pwd( -[PL])?|realpath \.|readlink -f \.)\)|` +
	"`(pwd( -[PL])?|realpath \\.|readlink -f \\.)`"
