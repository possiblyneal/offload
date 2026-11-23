package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// parseCommandList reads a command list (config/local-only,
// config/remote-store-commands): one entry per line, `#` comments and blanks skipped. An
// entry is argv[0]'s basename, optionally followed by the arguments that must come next.
func parseCommandList(r io.Reader) [][]string {
	lines, _ := contentLines(r)
	var out [][]string
	for _, line := range lines {
		out = append(out, strings.Fields(line))
	}
	return out
}

// watchServeMatch returns the entry a command argv runs starts with (findCommand), or "".
// A watch flag, a watch or serve task, and a JavaScript package manager's serve script
// also match.
func watchServeMatch(entries [][]string, argv []string) string {
	return findCommand(argv, func(c []string) string {
		return listedRun(entries, c, func(words []string) string {
			if flag := watchFlag(words); flag != "" {
				return words[0] + " " + flag
			}
			for _, task := range taskNames(words) {
				if serveScripts[task] || slices.Contains(strings.Split(task, "-"), "watch") {
					return words[0] + " " + task
				}
			}
			if script, _ := packageCommand(words); servesName(script) {
				if words[0] == "npm" && script != "start" {
					return "npm run " + script
				}
				return words[0] + " " + script
			}
			return ""
		})
	})
}

// listedRun returns the entry of entries the command c runs, its program read as a basename
// less any version (`npx vite@5`, `python3.12`), else what alsoMatch (nil for nothing) says of
// its words, else the same for the program a JavaScript package manager runs
// (packageCommand) or the module `python -m` runs (pythonModule), or "".
func listedRun(entries [][]string, c []string, alsoMatch func(words []string) string) string {
	base := filepath.Base(c[0])
	if i := strings.LastIndexByte(base, '@'); i > 0 {
		base = base[:i]
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 && i+1 < len(base) && base[i-1] >= '0' && base[i-1] <= '9' && strings.Trim(base[i+1:], "0123456789") == "" {
		base = base[:i] // a minor version after a major one: python3.12 runs as python3
	}
	words := append([]string{base}, c[1:]...)
	for _, e := range entries {
		if entryMatches(e, words) && !viteBuild(words) {
			return strings.Join(e, " ")
		}
	}
	if alsoMatch != nil {
		if m := alsoMatch(words); m != "" {
			return m
		}
	}
	if _, runs := packageCommand(words); len(runs) > 0 {
		return listedRun(entries, runs, alsoMatch)
	}
	if module := pythonModule(words); module != nil {
		return listedRun(entries, module, alsoMatch)
	}
	return ""
}

// pythonModule returns the words (argv[0] a basename) of `python -m <module> args…` as
// the module run as a program (`python -m uvicorn app:app` runs `uvicorn app:app`), or
// nil. Options are read as python reads them: -c ends them with a command, -W and -X take
// a value, joined or in the next word, and --check-hash-based-pycs the next word.
func pythonModule(words []string) []string {
	if _, ok := shortValueOptions[words[0]]; !ok {
		return nil
	}
	w := words[1:]
	for len(w) > 0 && len(w[0]) > 1 && w[0][0] == '-' && w[0] != "--" {
		o := w[0]
		if w = w[1:]; strings.HasPrefix(o, "--") {
			if o == "--check-hash-based-pycs" && len(w) > 0 { // python's one long option with a value
				w = w[1:]
			}
			continue
		}
	letters:
		for k := 1; k < len(o); k++ {
			switch o[k] {
			case 'm':
				if k+1 < len(o) {
					return append([]string{o[k+1:]}, w...)
				}
				if len(w) == 0 {
					return nil
				}
				return w
			case 'c':
				return nil
			case 'W', 'X':
				if k+1 == len(o) && len(w) > 0 {
					w = w[1:]
				}
				break letters
			}
		}
	}
	return nil
}

// storeCommandRefusal says why the job must run here when argv runs (findCommand) an entry
// of config/remote-store-commands, matched as config/local-only's are (listedRun): the
// command uses a store the job would get from the remote's disk. "" when none matches or
// the file is missing.
func storeCommandRefusal(configDir string, argv []string) string {
	f, err := os.Open(filepath.Join(configDir, "remote-store-commands"))
	if err != nil {
		return ""
	}
	defer f.Close()
	entries := parseCommandList(f)
	match := findCommand(argv, func(c []string) string { return listedRun(entries, c, nil) })
	if match == "" {
		return ""
	}
	return fmt.Sprintf("the command runs %q, which uses a store config/remote-local-paths keeps on the remote's disk", match)
}

// clusterAt splits a short-option cluster that holds want, a short option such as `-m`, at
// it, as getopt reads the cluster: `-Bm` is `-B -m`, and `-mhttp.server` is `-m http.server`.
// A letter of valued takes the rest of the word as its value, so want after one is not an
// option (`-Ximporttime`). It returns nil for any other word.
func clusterAt(w, want, valued string) []string {
	if len(want) != 2 || want[0] != '-' || want[1] == '-' || len(w) < 3 || w[0] != '-' || w[1] == '-' {
		return nil
	}
	k := strings.IndexByte(w[1:], want[1]) + 1
	if k == 0 || strings.ContainsAny(w[1:k], valued) {
		return nil
	}
	split := []string{want}
	if k > 1 {
		split = []string{w[:k], want}
	}
	if k+1 < len(w) {
		split = append(split, w[k+1:])
	}
	return split
}

// shortValueOptions are, per program, the short options whose value may join them in one
// word (`python3 -Wignore`).
var shortValueOptions = map[string]string{"python": "cmWX", "python3": "cmWX"}

// viteBuild reports whether words run `vite build` or `vite optimize`, which end on their
// own, unlike the dev server bare `vite` starts (`vite`, `vite --port 1`, `vite web`).
// vite 6.4.3's CLI (cac) runs a command only when it is the first word no option took: an
// option without `=` takes the next word unless that command declares it a switch or the
// word starts with `-`, so `vite --force build`, `vite --open build` and `vite --mode= build`
// start the dev server.
func viteBuild(words []string) bool {
	if words[0] != "vite" {
		return false
	}
	for _, command := range []string{"build", "optimize"} {
		rest := words[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "--" {
			name, v, _ := strings.Cut(rest[0], "=")
			rest = rest[1:]
			if v != "" || strings.HasPrefix(name, "--no-") || len(rest) == 0 {
				continue
			}
			// A switch takes a following `true` or `false`; any other option takes the
			// next word that is not an option, `--mode=` included.
			if slices.Contains(viteSwitches[command], name) {
				if rest[0] == "true" || rest[0] == "false" {
					rest = rest[1:]
				}
			} else if !strings.HasPrefix(rest[0], "-") {
				rest = rest[1:]
			}
		}
		if len(rest) > 0 && rest[0] == command {
			return true
		}
	}
	return false
}

// viteSwitches are, per vite command, the options vite 6.4.3 declares with no value: the
// command's own and the global ones.
var viteSwitches = map[string][]string{
	"build":    {"--clearScreen", "-h", "--help", "-v", "--version", "--emptyOutDir", "-w", "--watch", "--app"},
	"optimize": {"--clearScreen", "-h", "--help", "-v", "--version", "--force"},
}

// entryMatches reports whether words (argv[0] a basename) run the local-only entry e: the
// same program, then each further word of e in order, past any options before or between
// them (`go -C x clean`, `uv --offline cache clean`, `cargo +nightly watch`) and the value
// that may follow an option. A non-option word that is not the next one wanted ends the
// match unless it may be such a value; read wrongly, a job only stays here.
func entryMatches(e, words []string) bool {
	if words[0] != e[0] {
		return false
	}
	rest := words[1:]
	for _, want := range e[1:] {
		value := false // the word may be the value of the option before it
		for len(rest) > 0 && rest[0] != want {
			if split := clusterAt(rest[0], want, shortValueOptions[e[0]]); split != nil {
				rest = append(split, rest[1:]...)
				continue
			}
			opt := strings.HasPrefix(rest[0], "-") || strings.HasPrefix(rest[0], "+")
			if !opt && !value {
				return false
			}
			value = opt && !strings.Contains(rest[0], "=")
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return false
		}
		rest = rest[1:]
	}
	return true
}

// shortWatch are the tools whose -w, alone or among other short flags (`tsc -bw`), is watch
// mode; elsewhere -w means something else (gcc's warnings).
var shortWatch = map[string]bool{"tsc": true, "rollup": true, "webpack": true, "mocha": true, "vitest": true, "tailwindcss": true, "hugo": true, "vite": true}

// watchFlag returns the argument of words (argv[0] a basename) that puts a tool in watch
// or reload mode, or "". Over NFS a watcher on the remote never hears of edits made on dev.
func watchFlag(words []string) string {
	for i, w := range words[1:] {
		name, value, _ := strings.Cut(w, "=")
		switch {
		case value == "false" || value == "0":
		case i+2 < len(words) && (words[i+2] == "false" || words[i+2] == "0"): // `--watch false`
		case name == "--watch" || name == "--watchAll" || strings.HasPrefix(name, "--watch-") ||
			name == "--hot" || name == "--reload" || name == "--serve":
			return w
		case shortWatch[words[0]] && len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.ContainsRune(w, 'w') &&
			strings.Trim(w[1:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") == "":
			return w
		}
	}
	return ""
}

// taskNames are the tasks a task runner's words (argv[0] a basename) run: `make build dev`,
// `just dev`, `deno task dev`, `turbo run build dev`, `nx serve web`, `nx run web:serve`,
// `nx run-many -t serve`; none for any other command. A recipe's own arguments may be read
// as tasks too, which only keeps a job here.
func taskNames(words []string) []string {
	switch words[0] {
	case "make", "gmake":
		// -j and -l take a value only when one follows: `make -j dev` builds dev.
		var w []string
		for i, x := range words[1:] {
			if i > 0 && slices.Contains([]string{"-j", "--jobs", "-l", "--load-average", "--max-load"}, words[i]) &&
				x != "" && strings.Trim(x, "0123456789.") == "" {
				continue
			}
			w = append(w, x)
		}
		return targetOperands(w, "-C", "-f", "--file", "--directory", "-I", "-o", "-W", "--include-dir")
	case "just":
		t := targetOperands(words[1:], "-f", "--justfile", "-d", "--working-directory", "--shell", "--dotenv-path")
		for i, x := range t {
			if k := strings.LastIndex(x, "::"); k >= 0 {
				t[i] = x[k+2:] // a module's recipe: `web::dev`
			}
		}
		return t
	case "deno":
		if rest := skipOptions(words[1:]); len(rest) > 1 && rest[0] == "task" {
			if t := operands(rest[1:], "--cwd", "-c", "--config"); len(t) > 0 {
				return t[:1] // the rest are the task's arguments
			}
		}
	case "turbo":
		t := operands(words[1:], "--cwd", "--filter", "-F", "--concurrency", "--output-logs", "--env-mode",
			"--log-order", "--log-prefix", "--cache-dir", "--cache", "--ui", "--graph")
		if len(t) > 0 && t[0] == "run" {
			t = t[1:]
		}
		return t
	case "nx":
		rest := skipOptions(words[1:])
		if len(rest) == 0 {
			return nil
		}
		targets := optionValues(rest[1:], "-t", "--target", "--targets")
		switch rest[0] {
		case "run":
			if f := strings.Split(rest[min(1, len(rest)-1)], ":"); len(f) > 1 {
				return []string{f[1]} // project:target, or project:target:configuration
			}
			return targets
		case "run-many", "affected":
			return targets
		}
		return rest[:1] // nx serve web
	}
	return nil
}

// operands are w's words other than options, an option in withArg's value, and VAR=value.
func operands(w []string, withArg ...string) []string {
	var out []string
	for i := 0; i < len(w); i++ {
		switch x := w[i]; {
		case x == "--":
			i = len(w)
		case strings.HasPrefix(x, "-"):
			if slices.Contains(withArg, x) {
				i++
			}
		case !isAssignment(x):
			out = append(out, x)
		}
	}
	return out
}

// targetOperands are the operands of a make or just command line, which take every word after
// `--` as a target too (`make -- dev`).
func targetOperands(w []string, withArg ...string) []string {
	i := slices.Index(w, "--")
	if i < 0 {
		return operands(w, withArg...)
	}
	return append(operands(w[:i], withArg...), slices.DeleteFunc(slices.Clone(w[i+1:]), isAssignment)...)
}

// optionValues are the values w gives any of names, `--target=x` or every word up to the
// next option (`-t build serve`), each split at commas (`-t build,serve`).
func optionValues(w []string, names ...string) []string {
	var out []string
	for i, x := range w {
		name, value, eq := strings.Cut(x, "=")
		switch {
		case !slices.Contains(names, name):
		case eq:
			out = append(out, strings.Split(value, ",")...)
		default:
			for _, v := range w[i+1:] {
				if strings.HasPrefix(v, "-") {
					break
				}
				out = append(out, strings.Split(v, ",")...)
			}
		}
	}
	return out
}

// servesName reports whether a package script's name starts a server or watcher: one of
// serveScripts as its first `:` or `-` separated part (`dev:web`, `start-dev`), or `watch`
// as any part (`test:watch`); `build:dev` is a build.
func servesName(name string) bool {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == ':' || r == '-' })
	return len(parts) > 0 && serveScripts[parts[0]] || slices.Contains(parts, "watch")
}

// serveScripts are the package.json script names that start a server or watcher.
var serveScripts = map[string]bool{"dev": true, "start": true, "serve": true, "watch": true, "preview": true}

// packageOptionValues are the options of each package manager that take a separate value;
// npm's are read by skipNpmOptions instead, npx's by npxCommand, pnpm's by pnpmTakesNext.
var packageOptionValues = map[string][]string{
	"npm":  nil,
	"npx":  nil,
	"pnpm": nil,
	// yarn 1.22.22's `yarn --help`, every option given a <value>.
	"yarn": {"--cache-folder", "--cwd", "--global-folder", "--https-proxy", "--link-folder", "--modules-folder",
		"--mutex", "--network-concurrency", "--network-timeout", "--otp", "--preferred-cache-folder", "--proxy",
		"--registry", "--use-yarnrc"},
	"bun":  {"--cwd", "--filter", "-F"},
	"bunx": {"-p", "--package"},
}

// yarnTakesNext reports whether yarn 1.22.22 reads the word after option o, before the
// command, as o's value. Its commander parser gives every option but a declared switch the
// next word that is not an option: `--emoji [bool]`, an unknown `--foo` and an undeclared
// `--no-foo` alike, so `yarn --foo val dev` runs dev. After `run` it does not. A short
// cluster is its letters in turn, so its last letter decides (`-xs` ends in the switch -s).
func yarnTakesNext(o, next string) bool {
	if slices.Contains(packageOptionValues["yarn"], o) {
		return true
	}
	if !strings.HasPrefix(o, "--") && len(o) > 2 {
		o = "-" + o[len(o)-1:]
	}
	return !strings.Contains(o, "=") && !slices.Contains(yarnSwitches, o) && !strings.HasPrefix(next, "-")
}

// yarnSwitches are the options yarn 1.22.22's `--help` lists with no value.
var yarnSwitches = prefixed("--", `
	check-files disable-pnp enable-pnp pnp flat focus force frozen-lockfile har
	ignore-engines ignore-optional ignore-platform ignore-scripts json link-duplicates
	no-bin-links no-default-rc no-lockfile non-interactive no-node-version-check no-progress
	offline prefer-offline pure-lockfile silent skip-integrity-check strict-semver
	update-checksums version verbose help`, "-s", "-v", "-h")

// pnpmValueOptions are the options pnpm 9.15 reads a value for before a script or program:
// its global options and run's and exec's, whose type has no Boolean, and the shorthands
// -C (--dir) and -F (--filter). pnpm reads any other option, its install config keys
// included (`--tag`, `--store-dir`), as a switch, and takes no abbreviation (`--hoist` is
// not `--hoist-pattern`). `scripts-prepend-node-path` is typed `[false, true, "auto",
// "warn-only"]`, with no Boolean, so it always takes the next word.
var pnpmValueOptions = prefixed("--", `
	dir filter filter-prod loglevel prefix reporter test-pattern changed-files-ignore-pattern
	workspace-packages use-node-version workspace-concurrency resume-from
	scripts-prepend-node-path`, "-C", "-F")

// pnpmTakesNext reports whether pnpm leaves option o's value to next, more being the
// value options of the command o follows. A switch takes `true` or `false`
// (pnpmBoolValue), and `--color`, typed `["always", Boolean]`, takes `always` too.
func pnpmTakesNext(o, next string, more []string) bool {
	name, _, eq := strings.Cut(o, "=")
	switch {
	case eq:
		return false
	case pnpmBoolValue(name, next):
		return true
	case o == "--color":
		return next == "always"
	}
	return exactTakesNext(o, append(pnpmValueOptions, more...))
}

// pnpmBoolValue reports whether nopt, pnpm 9.15's option parser, gives next to the option
// name (no `=`) as a Boolean's value: it gives `true` or `false` to any option but a
// shorthand that already carries its value (pnpmValuedShorthands), unknown options included.
func pnpmBoolValue(name, next string) bool {
	return (next == "true" || next == "false") && !slices.Contains(pnpmValuedShorthands, strings.TrimLeft(name, "-"))
}

// pnpmValuedShorthands are the shorthands pnpm 9.15 expands to an option with its value: the
// loglevel ones, and -s/--silent for --reporter=silent.
var pnpmValuedShorthands = []string{"s", "q", "d", "dd", "ddd", "silent", "quiet", "verbose"}

// packageCommand reads words (argv[0] a basename) as a JavaScript package manager's
// command line: the package.json script it runs (`npm run --silent dev`, `pnpm dev`,
// `npm start`), or the program it runs (`npx vite`, `pnpm exec vite`), whichever applies;
// `pnpm vite` and `yarn run vite` may be either, so it returns both.
func packageCommand(words []string) (script string, runs []string) {
	opts, ok := packageOptionValues[words[0]]
	if !ok {
		return "", nil
	}
	skip := func(w []string, more ...string) []string {
		switch words[0] {
		case "npm":
			return skipNpmOptions(w)
		case "pnpm":
			return skipWords(w, func(o, next string) bool { return pnpmTakesNext(o, next, more) })
		}
		return skipOptions(w, append(opts, more...)...)
	}
	var rest []string
	switch words[0] {
	case "npx":
		return "", npxCommand(words)
	case "bunx":
		return "", skip(words[1:])
	case "yarn":
		rest = skipWords(words[1:], yarnTakesNext)
	default:
		rest = skip(words[1:])
	}
	if len(rest) == 0 {
		return "", nil
	}
	switch verb := rest[0]; {
	case words[0] == "yarn" && verb == "workspace" && len(rest) > 2: // yarn workspace <name> <script>
		return packageCommand(append([]string{"yarn"}, rest[2:]...))
	case words[0] == "yarn" && verb == "workspaces" && len(rest) > 2 && rest[1] == "run": // yarn 1's workspaces run <script|program>
		return packageCommand(append([]string{"yarn", "run"}, rest[2:]...))
	case words[0] == "yarn" && verb == "workspaces" && len(rest) > 1 && rest[1] == "foreach": // yarn workspaces foreach [options] <command>
		return packageCommand(append([]string{"yarn"}, skipOptions(rest[2:], "--from", "--include", "--exclude", "-j", "--jobs")...))
	case verb == "exec" || verb == "dlx" || verb == "x":
		if words[0] == "pnpm" {
			return "", skip(rest[1:], "--package") // pnpm's -c is --shell-mode, a switch
		}
		return "", skip(rest[1:], "-p", "--package", "-c", "--call")
	case verb == "run" || verb == "run-script" || words[0] == "npm" && (verb == "rum" || verb == "urn"):
		if rest = skip(rest[1:]); len(rest) == 0 {
			return "", nil
		}
		if words[0] == "yarn" || words[0] == "bun" {
			return rest[0], rest // yarn and bun run a program when no script has the name
		}
		return rest[0], nil
	case verb == "start":
		return verb, nil
	case verb == "restart" && (words[0] == "npm" || words[0] == "pnpm"): // stop, then start
		return "start", nil
	case words[0] != "npm":
		return verb, rest // pnpm, yarn and bun run a script, else a program, named as the command
	}
	return "", nil
}
