package main

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// This file reads npm's and npx's command lines as npm 10.9 does: npx-cli.js's rewrite and
// its `--` before the command, then nopt, npm's option parser, over npm's config
// definitions and shorthands. Ported from npm 10.9.9 (bin/npx-cli.js,
// node_modules/nopt/lib/nopt-lib.js, @npmcli/config/lib/definitions). See
// ../docs/third-party-notices.md for attribution and license texts.

// noptKind is how nopt reads an option's value, by its config type.
type noptKind int

const (
	noptUnknown  noptKind = iota
	noptValue             // a type with no Boolean: takes the next word whatever it is
	noptString            // String: takes the next word unless it starts as an option
	noptBool              // Boolean: takes only a following true or false
	noptBoolNull          // [null, Boolean]: a Boolean that also takes null
	noptColor             // ["always", Boolean]: a Boolean that also takes always
	noptBrowser           // [null, Boolean, String]: a Boolean that also takes null or a word not led by one dash
)

// npmKeys are npm 10.9's config keys by kind.
var npmKeys = func() map[string]noptKind {
	m := map[string]noptKind{"browser": noptBrowser, "color": noptColor}
	for kind, keys := range map[noptKind]string{
		noptBool: `all allow-same-version audit bin-links commit-hooks description dev
	diff-ignore-all-space diff-name-only diff-no-prefix diff-text dry-run engine-strict
	force foreground-scripts format-package-lock fund git-tag-version global global-style
	if-present ignore-scripts include-staged include-workspace-root install-links json
	legacy-bundling legacy-peer-deps link long offline omit-lockfile-registry-resolved
	package-lock package-lock-only parseable prefer-dedupe prefer-offline prefer-online
	progress provenance read-only rebuild-bundle save save-bundle save-dev save-exact
	save-optional save-peer save-prod shrinkwrap sign-git-commit sign-git-tag
	strict-peer-deps strict-ssl timing unicode update-notifier usage version versions
	workspaces-update`,
		noptBoolNull: `expect-results optional production workspaces yes`,
		noptString: `call diff-dst-prefix diff-src-prefix editor git heading init-author-email
	init-author-name init-license init.author.email init.author.name init.license message
	pack-destination preid save-prefix scope searchexclude searchopts shell tag
	tag-version-prefix user-agent viewer`,
		noptValue: `_auth access also audit-level auth-type before ca cache cache-max cache-min cafile cert
	cidr cpu depth diff diff-unified expect-result-count fetch-retries fetch-retry-factor
	fetch-retry-maxtimeout fetch-retry-mintimeout fetch-timeout globalconfig https-proxy
	include init-author-url init-module init-version init.author.url init.module
	init.version install-strategy key libc local-address location lockfile-version loglevel
	logs-dir logs-max maxsockets node-options noproxy omit only os otp package prefix
	provenance-file proxy registry replace-registry-host sbom-format sbom-type script-shell
	searchlimit searchstaleness umask userconfig which workspace`,
	} {
		for _, k := range strings.Fields(keys) {
			m[k] = kind
		}
	}
	return m
}()

// npmShorthands are npm 10.9's shorthands and the words each expands to.
var npmShorthands = map[string][]string{
	"enjoy-by": {"--before"}, "d": {"--loglevel", "info"}, "dd": {"--loglevel", "verbose"},
	"ddd": {"--loglevel", "silly"}, "quiet": {"--loglevel", "warn"}, "q": {"--loglevel", "warn"},
	"s": {"--loglevel", "silent"}, "silent": {"--loglevel", "silent"}, "verbose": {"--loglevel", "verbose"},
	"desc": {"--description"}, "help": {"--usage"}, "local": {"--no-global"}, "n": {"--no-yes"},
	"no": {"--no-yes"}, "porcelain": {"--parseable"}, "readonly": {"--read-only"}, "reg": {"--registry"},
	"iwr": {"--include-workspace-root"}, "a": {"--all"}, "c": {"--call"}, "f": {"--force"},
	"g": {"--global"}, "L": {"--location"}, "l": {"--long"}, "m": {"--message"}, "p": {"--parseable"},
	"C": {"--prefix"}, "S": {"--save"}, "B": {"--save-bundle"}, "D": {"--save-dev"}, "E": {"--save-exact"},
	"O": {"--save-optional"}, "P": {"--save-prod"}, "?": {"--usage"}, "H": {"--usage"}, "h": {"--usage"},
	"v": {"--version"}, "w": {"--workspace"}, "ws": {"--workspaces"}, "y": {"--yes"},
}

// noptOption is one option nopt read: its config key (an unknown one as written, its
// dashes and `no-` stripped) and its value, "" for a Boolean.
type noptOption struct {
	key, value string
}

// noptParsed is what nopt makes of npm's words.
type noptParsed struct {
	options []noptOption
	// remain is the positional words before `--`, then every word after it.
	remain []string
	// rest is the words from the first positional one on (a value split from its `=`
	// included) as written, or those after a `--` that comes first.
	rest []string
}

// call is the value of the last --call option, "" for none: the script `npm exec` runs.
func (p noptParsed) call() string {
	script := ""
	for _, o := range p.options {
		if o.key == "call" {
			script = o.value
		}
	}
	return script
}

var (
	noptOptionLed = regexp.MustCompile(`^-{1,2}[^-]+`)
	noptShortLed  = regexp.MustCompile(`^-[^-]`)
	noptDashes    = regexp.MustCompile(`^-{2,}$`)
	npmKeyList    = keysOf(npmKeys)
	npmShortList  = keysOf(npmShorthands)
)

// noptParse reads w, npm's words after its own name, as nopt does: options anywhere
// before `--`, each read by its kind, shorthands expanded and unique prefixes resolved.
func noptParse(w []string) noptParsed {
	args := slices.Clone(w)
	var p noptParsed
	positional := func(rest []string) {
		if p.rest == nil {
			p.rest = rest
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if noptDashes.MatchString(arg) {
			positional(args[i+1:])
			p.remain = append(p.remain, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || len(arg) == 1 {
			positional(args[i:])
			p.remain = append(p.remain, arg)
			continue
		}
		hadEq := false
		if name, v, ok := strings.Cut(arg, "="); ok {
			hadEq = true
			arg = name
			args = slices.Concat(args[:i], []string{name, v}, args[i+1:])
		}
		if sh := noptShorthand(arg); sh != nil {
			args = slices.Concat(args[:i], sh, args[i+1:])
			if arg != sh[0] {
				i--
				continue
			}
		}
		key := strings.TrimLeft(arg, "-")
		negated := false // nopt makes a switch of any `no-` form, `--no-no-x` too
		for strings.HasPrefix(strings.ToLower(key), "no-") {
			negated = true
			key = key[3:]
		}
		key = noptAbbrev(key)
		kind := npmKeys[key]
		la, hasLa := "", i+1 < len(args)
		if hasLa {
			la = args[i+1]
		}
		if negated || kind == noptBool || kind == noptBoolNull || kind == noptColor || kind == noptBrowser ||
			kind == noptUnknown && !hadEq {
			value := ""
			if la == "true" || la == "false" {
				value, la, hasLa = la, "", false
				i++
			}
			if hasLa && (la == "always" && kind == noptColor ||
				la == "null" && (kind == noptBoolNull || kind == noptBrowser) ||
				kind == noptBrowser && !noptShortLed.MatchString(la)) {
				value = la
				i++
			}
			p.options = append(p.options, noptOption{key, value})
			continue
		}
		taken := hasLa
		if kind == noptString && hasLa && noptOptionLed.MatchString(la) {
			la, taken = "", false
		}
		if taken && noptDashes.MatchString(la) {
			la, taken = "", false
		}
		p.options = append(p.options, noptOption{key, la})
		if taken {
			i++
		}
	}
	return p
}

// noptShorthand is what nopt's resolveShort expands arg (an option word) to, or nil: an
// exact key is no shorthand, an exact shorthand is, then a word whose letters are all
// one-letter shorthands, then a unique prefix of a key is none, a unique prefix of a
// shorthand is.
func noptShorthand(arg string) []string {
	a := strings.TrimLeft(arg, "-")
	if _, ok := npmKeys[a]; ok {
		return nil
	}
	if sh, ok := npmShorthands[a]; ok {
		return sh
	}
	if a != "" && !strings.ContainsFunc(a, func(r rune) bool { _, ok := npmShorthands[string(r)]; return !ok }) {
		var out []string
		for _, r := range a {
			out = append(out, npmShorthands[string(r)]...)
		}
		return out
	}
	if _, ok := uniquePrefix(a, npmKeyList); ok {
		return nil
	}
	if s, ok := uniquePrefix(a, npmShortList); ok {
		return npmShorthands[s]
	}
	return nil
}

// noptAbbrev is the config key key abbreviates (a unique prefix), or key itself.
func noptAbbrev(key string) string {
	if _, ok := npmKeys[key]; ok {
		return key
	}
	if k, ok := uniquePrefix(key, npmKeyList); ok {
		return k
	}
	return key
}

// uniquePrefix is the one word of words that p is a prefix of, as the abbrev package
// reads it: a word that is itself a prefix of others still abbreviates only to itself.
func uniquePrefix(p string, words []string) (string, bool) {
	if p == "" {
		return "", false
	}
	var match string
	n := 0
	for _, w := range words {
		if strings.HasPrefix(w, p) {
			match = w
			n++
		}
	}
	return match, n == 1
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// npxArgs is npx's words (its name first) as npx-cli.js hands them to `npm exec`, its name
// dropped: -p is --package, --shell is --script-shell, --no-install is --yes=false, a
// shorthand is expanded (a value after `=` its own word), the removed --npm, --node-arg
// and -n are dropped with their values, and `--` goes before the first word it reads as
// no option or value.
func npxArgs(w []string) []string {
	args := slices.Clone(w[1:])
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			return slices.Concat(args[:i], []string{"--"}, args[i:])
		}
		key, v, hasV := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch key {
		case "p", "shell":
			args[i] = map[string]string{"p": "--package", "shell": "--script-shell"}[key]
			if hasV {
				args[i] += "=" + v
			}
		case "no-install":
			args[i] = "--yes=false"
		default:
			if sh, ok := npmShorthands[key]; ok && !slices.Contains(npxRemoved, key) {
				a := slices.Clone(sh)
				if hasV {
					a = append(a, v)
				}
				args = slices.Concat(args[:i], a, args[i+1:])
				i--
				continue
			}
		}
		if slices.Contains(npxRemoved, key) {
			args = slices.Delete(args, i, i+1)
			i--
		}
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		if !hasV && !npxSwitch(key) && (slices.Contains(npxOptions, key) || !strings.HasPrefix(next, "-")) {
			if slices.Contains(npxRemoved, key) {
				if i+1 < len(args) {
					args = slices.Delete(args, i+1, i+2)
				}
			} else {
				i++
			}
		}
	}
	return args
}

// npxSwitch reports whether npx-cli.js reads key as taking no value: npm's keys whose type
// has Boolean, its removed switches, and a few it names.
func npxSwitch(key string) bool {
	switch npmKeys[key] {
	case noptBool, noptBoolNull, noptColor, noptBrowser:
		return true
	}
	return slices.Contains([]string{"always-spawn", "ignore-existing", "shell-auto-fallback",
		"no-install", "quiet", "q", "version", "v", "help", "h"}, key)
}

// npxOptions are the options npx-cli.js gives the next word even when it starts with `-`;
// of them it drops npm, node-arg and n with their values (npxRemoved).
var npxOptions = []string{"package", "p", "cache", "userconfig", "call", "c", "shell", "npm", "node-arg", "n"}

var npxRemoved = []string{"npm", "node-arg", "n"}

// npxCommand is the command npx runs: the words npm exec reads as positional.
func npxCommand(w []string) []string {
	return noptParse(npxArgs(w)).remain
}

// npmCallScript is the script of `npx -c|--call <script>` or `npm exec|x -c <script>`,
// which npm runs through a shell, or "". npm reads options anywhere before `--`, before
// `exec` too (`npm -c <script> exec`); with any other command it runs no call.
func npmCallScript(w []string) string {
	switch filepath.Base(w[0]) {
	case "npx":
		return noptParse(npxArgs(w)).call()
	case "npm":
		if p := noptParse(w[1:]); len(p.remain) > 0 && (p.remain[0] == "exec" || p.remain[0] == "x") {
			return p.call()
		}
	}
	return ""
}

// skipNpmOptions is w from the first word npm reads as positional (after a leading `--`).
func skipNpmOptions(w []string) []string {
	return noptParse(w).rest
}
