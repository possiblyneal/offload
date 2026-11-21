package main

import (
	"os"
	"strings"
	"testing"
)

func TestAListedNpxMatchesItsCallScriptToo(t *testing.T) {
	list := parseCommandList(strings.NewReader("npx\nnpm exec\n"))
	for argv, want := range map[string]string{"npx -c tsc": "npx", "npx -yc tsc": "npx", "npx --call tsc": "npx", "npm exec -c tsc": "npm exec"} {
		if got := watchServeMatch(list, strings.Fields(argv)); got != want {
			t.Errorf("watchServeMatch(%q) = %q, want %q", argv, got, want)
		}
	}
}

func TestWatchServeMatch(t *testing.T) {
	list := parseCommandList(strings.NewReader("# watchers\norca\nxvfb-run\nnpm run dev\n\nvite\nwatch\nentr\npodman\nuvicorn\n"))
	tests := []struct {
		argv []string
		want string
	}{
		{[]string{"orca", "status"}, "orca"},
		{[]string{"/usr/bin/xvfb-run", "true"}, "xvfb-run"},
		{[]string{"npm", "run", "dev"}, "npm run dev"},
		{[]string{"npm", "run", "dev", "--", "--port", "1"}, "npm run dev"},
		{[]string{"npm", "run", "build"}, ""},
		{[]string{"npm", "ci"}, ""},
		{[]string{"go", "run", "./x"}, ""},
		{[]string{"vite"}, "vite"},
		{[]string{"watch", "ls"}, "watch"},
		{[]string{"orcastrate"}, ""},
		// Behind a wrapper or in a shell's -c script, as usesSudo reads them.
		{[]string{"sh", "-c", "npm ci && npm run dev"}, "npm run dev"},
		{[]string{"bash", "-c", "vite"}, "vite"},
		{[]string{"env", "FOO=1", "vite"}, "vite"},
		{[]string{"nice", "-n", "5", "vite"}, "vite"},
		{[]string{"sudo", "-u", "web", "vite"}, "vite"},
		{[]string{"time", "-vo", "t.log", "vite"}, "vite"},
		{[]string{"/usr/bin/time", "-f", "%e", "vite"}, "vite"},
		{[]string{"time", "-o", "t.log", "vite"}, "vite"},
		{[]string{"timeout", "-s", "INT", "5", "vite"}, "vite"},
		{[]string{"timeout", "5", "watch", "ls"}, "watch"},
		{[]string{"nohup", "orca", "status"}, "orca"},
		{[]string{"sh", "-c", "echo vite"}, ""},
		{[]string{"sh", "script.sh"}, ""},
		{[]string{"ionice", "-c3", "podman", "build", "."}, "podman"},
		{[]string{"stdbuf", "-oL", "vite"}, "vite"},
		{[]string{"setpriv", "--groups", "1", "podman", "ps"}, "podman"},
		{[]string{"prlimit", "-p", "1", "-o", "x", "podman", "ps"}, "podman"},
		{[]string{"systemd-run", "--user", "--scope", "-E", "A=1", "podman", "ps"}, "podman"},
		{[]string{"busybox", "env", "podman", "ps"}, "podman"},
		{[]string{"sg", "-", "wheel", "-c", "podman ps"}, "podman"},
		{[]string{"script", "-E", "never", "-c", "podman ps"}, "podman"},
		{[]string{"setsid", "vite"}, "vite"},
		// A package manager's serve script, or a listed program it runs.
		{[]string{"npx", "vite"}, "vite"},
		{[]string{"npx", "-p", "x", "vite", "--port", "1"}, "vite"},
		{[]string{"npx", "--registry", "http://x", "vite"}, "vite"},
		{[]string{"npx", "--foo", "vite"}, "vite"},
		{[]string{"npm", "--registry", "http://x", "exec", "vite"}, "vite"},
		{[]string{"npm", "--registry", "http://x", "run", "dev"}, "npm run dev"},
		{[]string{"npm", "--cache", "/c", "start"}, "npm start"},
		{[]string{"npm", "--foo", "run", "dev"}, "npm run dev"}, // npm reads an unknown option as a switch
		{[]string{"npm", "-desc", "run", "dev"}, "npm run dev"}, // npm's shorthand for --description, not a -c cluster
		{[]string{"npm", "run", "--userconfig", "f", "dev"}, "npm run dev"},
		{[]string{"hugo", "-w"}, "hugo -w"},
		{[]string{"pnpm", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--reporter", "append-only", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--workspace-concurrency", "2", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--foo", "dev"}, "pnpm dev"},   // pnpm reads an unknown option as a switch
		{[]string{"pnpm", "--hoist", "dev"}, "pnpm dev"}, // a switch, not a prefix of --hoist-pattern
		{[]string{"pnpm", "--lockfile", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--tag", "dev", "build"}, "pnpm dev"}, // an install config key is a switch when a script runs
		{[]string{"pnpm", "--store-dir", "dev", "build"}, "pnpm dev"},
		{[]string{"pnpm", "--resume-from", "a", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--color", "always", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--color", "dev"}, "pnpm dev"}, // --color takes always, true or false
		{[]string{"pnpm", "--color", "auto", "dev"}, ""}, // pnpm runs the program auto
		{[]string{"pnpm", "--color", "true", "dev"}, "pnpm dev"},
		{[]string{"pnpm", "--foo", "false", "dev"}, "pnpm dev"}, // nopt gives true or false to any switch
		{[]string{"pnpm", "-s", "true", "dev"}, ""},             // -s is --reporter=silent: pnpm runs the script true
		{[]string{"pnpm", "--scripts-prepend-node-path", "auto", "dev"}, "pnpm dev"},
		{[]string{"npm", "--yes", "true", "run", "dev"}, "npm run dev"},
		{[]string{"npx", "--yes", "true", "vite"}, ""}, // npx runs the program true
		// python -m runs a module as its program.
		{[]string{"python", "-m", "uvicorn", "app:app"}, "uvicorn"},
		{[]string{"python3", "-Bmuvicorn", "app:app"}, "uvicorn"},
		{[]string{"python3", "-X", "dev", "-m", "uvicorn"}, "uvicorn"},
		{[]string{"python3", "--check-hash-based-pycs", "always", "-m", "uvicorn"}, "uvicorn"},
		{[]string{"python3", "-Xm", "uvicorn"}, ""}, // -X takes m as its value; uvicorn is a script
		{[]string{"python3", "-c", "x", "-m", "uvicorn"}, ""},
		{[]string{"uv", "run", "-m", "uvicorn", "app:app"}, "uvicorn"},
		{[]string{"pnpm", "exec", "-c", "vite"}, "vite"}, // pnpm's -c is --shell-mode, a switch
		{[]string{"pnpm", "dlx", "--package", "x", "vite"}, "vite"},
		{[]string{"yarn", "dev"}, "yarn dev"},
		{[]string{"bun", "run", "dev"}, "bun dev"},
		{[]string{"yarn", "run", "vite", "--port", "1"}, "vite"}, // yarn and bun run a program no script names
		{[]string{"bun", "run", "vite"}, "vite"},
		{[]string{"npm", "run", "vite"}, ""},            // npm runs only a script
		{[]string{"npm", "-c", "vite", "exec"}, "vite"}, // npm reads -c before exec too
		{[]string{"npm", "start"}, "npm start"},
		{[]string{"npm", "run", "start"}, "npm start"},
		{[]string{"npm", "run", "serve"}, "npm run serve"},
		{[]string{"npm", "--prefix", "web", "run", "dev"}, "npm run dev"},
		{[]string{"npm", "run", "-w", "web", "dev"}, "npm run dev"},
		{[]string{"npm", "run", "--silent", "dev"}, "npm run dev"},
		{[]string{"npm", "run", "dev", "-w", "web"}, "npm run dev"},
		{[]string{"pnpm", "--filter", "web", "preview"}, "pnpm preview"},
		{[]string{"pnpm", "exec", "vite"}, "vite"},
		{[]string{"npm", "exec", "--", "vite"}, "vite"},
		{[]string{"sh", "-c", "cd web && yarn watch"}, "yarn watch"},
		{[]string{"npm", "test"}, ""},
		{[]string{"npm", "run", "build"}, ""},
		{[]string{"pnpm", "install"}, ""},
		{[]string{"yarn"}, ""},
		{[]string{"npx", "tsc", "--noEmit"}, ""},
		{[]string{"npm", "exec", "vitest", "run"}, ""},
	}
	for _, tt := range tests {
		if got := watchServeMatch(list, tt.argv); got != tt.want {
			t.Errorf("watchServeMatch(%q) = %q, want %q", tt.argv, got, tt.want)
		}
	}
}

// Container tools keep their images and containers under ~/.local/share/containers and
// ~/.cache/containers, and a cache command acts on its cache, which a job gets from the
// remote's own disk. The shipped config/remote-store-commands keeps them here under every
// --where, since remote-local is a hard refusal.
func TestShippedStoreCommandsStayHereUnderEveryWhere(t *testing.T) {
	for _, argv := range [][]string{
		{"podman", "build", "."},
		{"/usr/bin/podman", "pull", "x"},
		{"podman-compose", "up"},
		{"buildah", "bud"},
		{"skopeo", "copy", "a", "containers-storage:b"},
		{"docker", "build", "."},
		{"docker-compose", "up"},
		{"sh", "-c", "make && podman build ."},
		// Behind setpriv, prlimit, systemd-run, busybox, sg and script (findCommand).
		{"setpriv", "--reuid", "1000", "podman", "build", "."},
		{"prlimit", "--nofile=4096", "podman", "build", "."},
		{"systemd-run", "--user", "--scope", "-p", "CPUWeight=50", "podman", "build", "."},
		{"busybox", "env", "podman", "build", "."},
		{"sg", "wheel", "-c", "podman build ."},
		{"script", "-qc", "podman build .", "/dev/null"},
		{"go", "-C", "x", "clean", "-cache"},
		{"uv", "--offline", "cache", "clean"},
		{"pre-commit", "gc"},
		// Run through a JavaScript package manager (packageCommand), with a version (`@2`).
		{"pnpm", "exec", "podman", "build", "."},
		{"npm", "exec", "--", "podman", "build", "."},
		{"pnpm", "docker", "build", "."},
		{"bunx", "podman", "build"},
		{"npx", "docker-compose@2", "up"},
		{"npm", "-c", "docker ps", "x"},
		{"npm", "--call=podman ps", "exec"},
		// Run as another user: the store is still the remote's.
		{"sudo", "podman", "build", "."},
		{"sudo", "-n", "docker", "ps"},
		{"sudo", "-nu", "root", "--", "podman", "ps"},
		{"doas", "-u", "root", "podman", "ps"},
		{"run0", "--user=root", "docker", "ps"},
		{"pkexec", "--user", "root", "podman", "ps"},
		{"su", "-", "root", "-c", "podman ps"},
		{"su", "-lc", "podman build ."},
		{"su", "-cpodman ps"},
		{"su", "--comm", "podman ps"},
		{"su", "--command=podman ps"},
		{"su", "--session-command", "podman ps"},
		{"bash", "-c", "echo $'it\\'s'; podman ps"},
		{"bash", "-c", "echo $'\\''; podman ps"},
		{"su", "-s", "/bin/sh", "root", "-c", "podman ps"},
		{"su", "root", "-c", "true", "-c", "podman ps"},       // the last -c is the one run
		{"su", "root", "-c", "podman ps", "--", "-c", "true"}, // -- ends su's options
		{"env", "-a", "x", "podman", "ps"},
		{"env", "--argv0", "x", "podman", "ps"},
		{"env", "-iS", "podman ps"},
		{"env", "-vS", "podman ps"},
		{"env", "-iu", "FOO", "podman", "ps"},
		{"env", "--split-string=podman ps"},
		{"env", "--argv", "x", "podman", "ps"},
		{"env", "--split", "podman ps"},
		{"su", "root", "--", "-c", "podman ps"}, // the shell's own -c
		{"su", "root", "--", "-c", "-e", "podman ps"},
		{"su", "root", "--", "-c", "--", "podman ps"},
		{"su", "--", "root", "-c", "podman ps"},
		{"su", "-s", "/usr/bin/podman", "root", "--", "ps"}, // su runs its -s shell
		{"su", "--shell=/usr/bin/podman", "root", "--", "ps"},
		{"su", "-s", "/usr/bin/podman", "root", "-c", "ps"},
		{"env", "-S", "-i", "podman", "ps"}, // env reads a -S string's options too
		{"env", "-S-i podman ps"},
		{"env", "-S", "-C / podman ps"},
		{"su", "-s", "/usr/bin/fish", "root", "-c", "podman ps"},
		{"su", "-s", "/bin/rbash", "root", "-c", "podman ps"}, // any shell runs su's -c
		{"su", "-s", "/usr/bin/tmux", "root", "-c", "podman ps"},
		{"fish", "-c", "podman ps"},
		{"tcsh", "-c", "podman ps"},
		{"nice", "--adj", "5", "podman", "ps"}, // a long option's unambiguous prefix
		{"timeout", "--sig", "KILL", "10", "podman", "ps"},
		{"stdbuf", "--out", "L", "podman", "ps"},
		{"uv", "--directory", "x", "run", "podman", "ps"}, // uv's global options before run
		{"uv", "run", "--env-file", ".env", "podman", "ps"},
		{"npx", "-c", "podman ps"},
		// npx-cli.js gives an unknown option the next word, then npm reads that word as the
		// command (npx 10.9.9 ran podman for each of these).
		{"npx", "--foo", "podman", "ps"},
		{"npx", "--no", "podman", "ps"},
		{"npx", "--no-audit", "podman", "ps"},
		{"npx", "--local", "podman", "ps"},
		{"npx", "--no", "-c", "podman ps"},
		{"npx", "--npm=x", "podman", "ps"},
		{"npx", "-n", "x", "podman", "ps"}, // npx drops -n and its value
		{"npm", "exec", "--no", "podman", "ps"},
		{"npm", "--local", "exec", "podman", "ps"},
		{"yarn", "workspaces", "run", "podman", "ps"}, // yarn 1.22.22 ran podman in each workspace
		// nopt splits `--opt=value` and reads the value as positional after a switch, and
		// takes an ambiguous prefix as an unknown switch (npm 10.9.9 ran each command).
		{"npx", "-y=podman", "ps"},
		{"npx", "--yes=podman", "ps"},
		{"npx", "--no=podman", "ps"},
		{"npm", "exec", "--yes=podman", "ps"},
		{"npx", "--message", "-c", "podman ps"},
		{"npm", "--log", "exec", "podman", "ps"},
		{"npm", "--fetch-retry", "exec", "--yes", "podman", "ps"},
		{"npx", "-Z=foo", "podman", "ps"},           // an unknown option keeps its `=` value
		{"npx", "--no-no-registry", "podman", "ps"}, // any no- prefix makes a switch
		{"npm", "exec", "--no-no-registry", "podman", "ps"},
		// An unquoted here-document's body runs its substitutions.
		{"bash", "-c", "cat <<EOF\n$(podman ps)\nEOF"},
		{"bash", "-c", "cat <<-EOF\n\t`podman ps`\n\tEOF"},
		// A here-document's quote opens nothing (bash ran the command after it).
		{"bash", "-c", "cat <<EOF\ndon't panic\nEOF\npodman ps"},
		{"bash", "-c", "x=$(cat <<EOF\nit's\nEOF\n); podman ps"},
		{"bash", "-c", "(( x << y )); podman ps"},
		// Two here-documents on a line, the first body running to the end.
		{"bash", "-c", "cat <<A; cat <<B\n$(podman ps)"},
		{"uv", "run", "--refresh", "podman", "ps"}, // uv takes no prefix: a whole flag, not a value option's start
		{"uv", "run", "--upgrade", "podman", "ps"},
		{"uv", "run", "--reinstall", "podman", "ps"},
		{"uv", "run", "--no-build", "podman", "ps"},
		{"uv", "run", "--no-sources", "podman", "ps"},
		{"uv", "run", "--no-binary", "podman", "ps"},
		{"uv", "run", "--no-build-isolation", "podman", "ps"},
		{"run0", "--shell-prompt-prefix", "x", "podman", "ps"},
		{"sh", "-c", "sudo docker build ."},
		{"time", "-ao", "t.log", "podman", "build", "."},
		// uv's and pipx's tool runners run a program as npx does.
		{"uvx", "podman-compose", "up"},
		{"uvx", "-c", "c.txt", "pre-commit", "gc"},
		{"uv", "tool", "run", "podman-compose", "up"},
		{"pipx", "run", "--python", "3.12", "podman-compose", "up"},
		{"pipx", "run", "-i", "https://pypi.org/simple", "podman-compose", "up"},
		{"pipx", "-q", "--global", "run", "podman-compose", "up"},
		{"uvx", "--torch-backend", "auto", "podman-compose", "up"},
		{"uv", "tool", "run", "--torch-backend", "auto", "podman-compose", "up"},
		{"uvx", "podman-compose==1.5.0", "up"},
		{"pipx", "run", "podman-compose==1.5.0", "up"},
		{"uvx", "podman-compose(>=1.5)", "up"},
		{"pipx", "run", "podman-compose(>=1.5)", "up"},
		{"yarn", "--registry", "https://registry.npmjs.org", "podman-compose", "up"},
		{"yarn", "--emoji", "true", "podman-compose", "up"},
		{"yarn", "-ss", "podman-compose", "up"},
		{"yarn", "-xs", "podman-compose", "up"},
		{"uvx", "--preview-feature", "x", "podman-compose", "up"},
		{"npm", "--color", "always", "exec", "podman", "ps"},
		{"npm", "--browser", "firefox", "exec", "podman", "ps"},
		{"pnpm", "-c", "exec", "podman ps"},
		{"bun", "exec", "podman ps"},
		{"pnpm", "--shell-mode", "exec", "echo hi; podman ps"},
		{"uv", "tool", "uvx", "pre-commit", "gc"},
		{"python3", "-m", "pre_commit", "gc"},
		{"python3", "-Im", "pre_commit", "clean"},
		{"uvx", "--python-preference", "system", "podman-compose", "up"},
	} {
		reason := storeCommandRefusal("../config", argv)
		if reason == "" {
			t.Errorf("config/remote-store-commands does not keep %q here", argv)
			continue
		}
		d := place(placeRequest{Where: "server", Remotes: []string{"server"},
			Slots: map[string]slotState{"server": {Total: 1, Free: 1}}, Facts: refusalFacts{RemoteLocal: reason}})
		if d.Placement != local || d.Rule != "remote-local" {
			t.Errorf("--where server %q: placed %s by %s, want local by remote-local", argv, d.Placement, d.Rule)
		}
	}
	for _, argv := range [][]string{{"go", "test", "./..."}, {"uv", "run", "pytest"}, {"pre-commit", "run"}, {"uvx", "ruff", "check"},
		{"uv", "tool", "list"}, {"uvx", "--from", "podman-compose", "ruff"},
		// Only a name followed by a requirement marker is cut: a URL is no package name.
		{"uvx", "docker+https://example.com/d.whl"},
		// A name a script builds is not seen; the read-only store catches that run.
		{"sh", "-c", `p=pod; "${p}man" run --rm alpine true`},
		// A quoted here-document's body is text.
		{"bash", "-c", "cat <<'EOF'\n$(podman ps)\nEOF"}, {"bash", "-c", "cat <<\\EOF\n`podman ps`\nEOF"}} {
		if reason := storeCommandRefusal("../config", argv); reason != "" {
			t.Errorf("%q refused: %s", argv, reason)
		}
	}
}

// A watcher on the remote never hears of edits made on dev: the shipped list keeps them here.
func TestShippedLocalOnlyKeepsWatchersHere(t *testing.T) {
	f, err := os.Open("../config/local-only")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	list := parseCommandList(f)
	for _, argv := range [][]string{
		{"cargo", "watch", "-x", "test"},
		{"cargo", "+nightly", "watch"},
		{"hugo", "-D", "server"},
		{"npx", "tsc", "--watch"},
		{"tsc", "-w", "-p", "."},
		{"npx", "next", "dev"},
		{"nodemon", "app.js"},
		{"python3", "-m", "mkdocs", "serve"},
		{"poetry", "run", "python", "-m", "uvicorn", "app:app"},
		{"npm", "run", "dev:web"},
		{"pnpm", "watch:css"},
		{"npx", "jest", "--watchAll"},
		{"node", "--watch=src", "app.js"},
		// Options before or between an entry's words.
		{"cargo", "+nightly", "watch"},
		{"python3", "-u", "-m", "http.server"},
		// Task runners and workspaces.
		{"yarn", "workspace", "web", "dev"},
		{"turbo", "dev"},
		{"turbo", "run", "dev", "--filter", "web"},
		{"nx", "serve", "web"},
		{"nx", "run", "web:serve"},
		{"nx", "run", "web:serve:production"},
		{"deno", "task", "dev"},
		{"make", "dev"},
		{"make", "-C", "web", "dev"},
		{"make", "-j", "8", "dev"},
		{"make", "-l", "4", "dev"},
		{"make", "-j", "dev"},
		{"make", "build", "dev"},
		{"just", "build", "dev"},
		{"turbo", "run", "build", "dev"},
		{"turbo", "run", "--concurrency", "20", "dev"},
		{"turbo", "--output-logs", "full", "dev"},
		{"nx", "run-many", "-t", "serve"},
		{"nx", "run-many", "--target=build,serve"},
		{"nx", "run-many", "-t", "build", "serve"},
		{"make", "--", "dev"},
		{"just", "--", "dev"},
		{"python3", "-mhttp.server"},
		{"python3", "-Bm", "http.server"},
		{"python3", "-Ximporttime", "-m", "http.server"},
		{"python3", "-Xfrozen_modules=off", "-m", "http.server"},
		{"python3", "-Wignore::ImportWarning", "-m", "http.server"},
		{"npm", "restart"},
		{"npm", "rum", "dev"},
		{"npm", "urn", "dev"},
		{"just", "web::dev"},
		{"hugo", "serve"},
		{"jekyll", "s"},
		{"jekyll", "server"},
		{"bundle", "exec", "jekyll", "serve"},
		{"fish", "--command", "vite"},
		{"bash", "-c", "builtin exec vite"},
		{"bash", "-c", "coproc vite"},
		{"nx", "affected", "-t", "dev"},
		{"nx", "run", "--project", "web", "--target", "serve"},
		{"npm", "--loglevel", "silent", "run", "dev"},
		{"npm", "run", "test:watch"},
		{"pnpm", "run", "build:watch"},
		{"yarn", "test:watch"},
		{"npm", "run", "start-dev"},
		{"pnpm", "vite"},
		{"vite", "--port", "3000"},
		{"vite", "preview"},
		{"vite", "build", "--watch"},
		{"vite", "build", "-w"},
		// An option vite 6.4.3 does not declare a switch of build takes the next word.
		{"vite", "--open", "build"},
		{"vite", "-d", "build"},
		{"vite", "--force", "build"},
		{"vite", "--strictPort", "build"},
		{"vite", "--host", "build"},
		{"yarn", "--registry", "https://registry.npmjs.org", "dev"},
		{"yarn", "--network-timeout", "1000", "dev"},
		{"yarn", "--emoji", "true", "dev"},
		{"yarn", "--prod", "true", "dev"},
		{"yarn", "--scripts-prepend-node-path", "true", "dev"},
		{"yarn", "--foo", "val", "dev"},
		{"yarn", "--no-foo", "val", "dev"},
		{"yarn", "-x", "val", "dev"},
		{"yarn", "--verbose", "dev"},
		{"yarn", "run", "--emoji", "dev"}, // after run, yarn binds no optional value
		{"vite", "--mode=", "build"},
		{"npx", "vite", "--base=", "build"},
		{"uvx", "--python-preference", "system", "mkdocs", "serve"},
		{"uv", "run", "--python-preference", "system", "mkdocs", "serve"},
		{"uv", "tool", "run", "--python-fetch", "automatic", "mkdocs", "serve"},
		{"uv", "run", "--trusted-host", "x", "--config-settings", "a=b", "--preview-features", "y", "mkdocs", "serve"},
		{"uvx", "--constraint", "c.txt", "--build-constraint", "b.txt", "--override", "o.txt", "mkdocs", "serve"},
		{"uvx", "mkdocs", "serve"},
		{"pipx", "run", "vite"},
		{"uvx", "--from", "mkdocs-material", "mkdocs@latest", "serve"},
		{"uv", "tool", "run", "--with", "x", "mkdocs", "serve"},
		{"uv", "--directory", "x", "tool", "run", "mkdocs", "serve"},
		{"pipx", "run", "--spec", "mkdocs==1.6", "mkdocs", "serve"},
		{"pipx", "-q", "run", "-i", "https://pypi.org/simple", "mkdocs", "serve"},
		{"uvx", "--torch-backend", "cpu", "mkdocs", "serve"},
		{"uvx", "mkdocs==1.6", "serve"}, // a requirement runs its package's executable
		{"uvx", "mkdocs[i18n]", "serve"},
		{"uvx", "mkdocs>=1.5", "serve"},
		{"pipx", "run", "mkdocs==1.6", "serve"},
		{"uvx", "mkdocs(>=1.5)", "serve"},
		{"uvx", "mkdocs\t>=1.5", "serve"},
		{"pipx", "run", "mkdocs(>=1.5)", "serve"},
		{"npm", "run", "dev-server"},
		{"yarn", "nodemon"},
		{"yarn", "tsc", "-w"},
		{"bun", "vite"},
		{"npx", "vite@5"},
		{"vitest", "-w"},
		{"tailwindcss", "-i", "in.css", "-w"},
		{"deno", "run", "--watch-hmr", "main.ts"},
		{"esbuild", "app.ts", "--serve=8000"},
		{"uv", "run", "vite"},
		{"uv", "run", "--reinstall", "vite"},
		{"poetry", "run", "python", "-m", "http.server"},
		{"uv", "run", "-m", "http.server"}, // uv run -m runs python -m
		{"npx", "-c", "vite"},              // npx and npm exec run their -c script in a shell
		{"npx", "--call=vite"},
		{"npm", "exec", "-c", "vite"},
		{"npx", "--registry", "http://x", "-c", "vite"}, // any option but a flag takes a value
		{"npm", "--cache", "/x", "exec", "-c", "vite"},
		{"python3.12", "-m", "http.server"},
		{"yarn", "workspaces", "foreach", "-A", "run", "dev"},
		{"yarn", "workspaces", "foreach", "--all", "-pi", "-j", "4", "run", "dev"},
		{"npm", "--prefix", "x", "exec", "--call", "vite"},
		{"env", "-S", "FOO='a b' vite"},
		{"uv", "run", "--module", "http.server"},
		{"uv", "run", "-qm", "http.server"},
		{"uv", "run", "--with", "x", "-m", "http.server"},
		{"env", "-", "vite"},
		{"astro", "dev"},
		{"nuxt", "dev"},
		{"storybook", "dev"},
		{"mkdocs", "serve"},
		{"uvicorn", "app:app"},
		{"bash", "-c", `FOO="a b" vite`},
		{"bash", "-c", `NODE_OPTIONS="--max-old-space-size=4096 --x" npm run dev`},
		{"just", "dev"},
		{"make", "test-watch"},
		{"bash", "-c", "command vite"},
		{"bash", "-x", "-c", "vite"},
		{"bash", "-o", "pipefail", "-c", "vite"},
		{"bash", "-O", "extglob", "+o", "errexit", "-c", "vite"},
		{"bash", "--rcfile", "x", "-c", "vite"},
		{"bash", "-euo", "pipefail", "-c", "npm run dev"},
		{"sh", "-c", "npm run \\\n  dev"},
		// Long option forms, other names and verbs.
		{"flock", "/tmp/l", "--command", "vite"},
		{"env", "--split-string", "vite --port 1"},
		{"gmake", "dev"},
		{"make", "-l", "4.5", "dev"},
		{"pnpm", "dlx", "vite"},
		{"yarn", "dlx", "vite"},
		{"pnpm", "--filter", "web", "-F", "app", "dev"},
		{"turbo", "-F", "web", "dev"},
		{"vite", "--mode", "build"},
		{"bash", "-c", "{ vite; }"},
		// Watch and reload flags.
		{"jest", "--watchAll=true"},
		{"mocha", "-w"},
		{"webpack", "-w"},
		{"node", "--watch-path=src", "app.js"},
		{"tsc", "-bw"},
		{"bun", "--hot", "app.ts"},
		{"uvicorn", "app:app", "--reload"},
		// Other watchers and servers.
		{"vitest", "watch"},
		{"vitest", "dev"},
		{"watchexec", "make"},
		{"air"},
		{"inotifywait", "-m", "."},
		{"python3", "-m", "http.server"},
		{"hugo", "server"},
		{"env", "-S", "vite --port 1"},
		{"env", "-Svite"},
		{"env", "-C", "web", "vite"},
		{"env", "--chdir", "web", "vite"},
		{"env", "--split-string=vite --port 1"},
	} {
		if watchServeMatch(list, argv) == "" {
			t.Errorf("config/local-only does not keep %q here", argv)
		}
	}
	for _, argv := range [][]string{
		{"npm", "run", "build"},
		{"npm", "run", "develop-docs"},
		{"tsc", "-p", "."},
		{"gcc", "-w", "a.c"},
		{"node", "--watch=false", "app.js"},
		{"go", "test", "./..."},
		{"go", "test", "-run", "clean"},
		{"cargo", "build", "--bin", "watch"},
		{"make"},
		{"make", "build"},
		{"make", "-j", "8", "build"},
		{"npm", "run", "build:dev"},
		{"make", "install-dev"},
		{"pnpm", "build:preview"},
		{"bash", "-c", "make -j4 test-dev"},
		{"npx", "vite", "build"},
		{"pnpm", "vite", "build"},
		{"pnpm", "exec", "vite", "--mode", "x", "build"},
		{"vite", "optimize"},
		{"vite", "--clearScreen", "build"},
		{"vite", "--emptyOutDir", "build"},
		{"vite", "--debug=x", "build"},
		{"vite", "--force", "optimize"},
		{"vite", "--mode=x", "build"},
		{"vite", "--no-clearScreen", "build"},
		{"vite", "-v", "build"},
		{"vite", "--version", "build"},
		{"vite", "--clearScreen", "true", "build"},
		{"nx", "run-many", "-t", "build"},
		{"turbo", "run", "build", "--", "dev"},
		{"just", "web::build"},
		{"npm", "rum", "build"},
		{"yarn", "build"},
		{"pnpm", "tsc", "-p", "."},
		{"npm", "run", "test:unit"},
		{"uv", "run", "pytest"},
		{"bash", "-c", `FOO="a vite" make`},
		{"nx", "run", "web:build"},
		{"turbo", "build"},
		{"jest", "--watchAll=false"},
		{"yarn", "workspace", "web", "build"},
		{"python3", "-m", "pytest"},
		{"vitest", "run"},
		{"env", "-S", "make test"},
		{"curl", "https://example.com/"},
		{"bash", "-c", "command -v docker >/dev/null && echo yes"},
		{"sh", "-c", "command -pv vite || npm i"},
		{"bash", "build.sh", "-c", "vite"},
		{"make", "dev-deps"},
		{"just", "start-db"},
		{"tsc", "--watch", "false"},
		{"tsc", "--watch=0"},
		{"make", "-l", "dev-deps"},
		{"gmake", "build"},
		{"turbo", "-F", "dev", "build"}, // dev is the filter's package
		{"uv", "run", "--with", "vite", "pytest"},
		{"uv", "run", "-wm", "pytest"}, // m is -w's value, not -m
		{"uv", "run", "pytest", "-m", "slow"},
		{"npx", "-c", "vitest run"},
		{"npx", "-p", "vite", "-c", "tsc"}, // -p's value is a package, not the command
		{"npx", "-p", "vite", "tsc"},
		{"serve.1"},                                   // a digits suffix is a minor version only after a major one
		{"cargo", "--color=always", "build", "watch"}, // build is no option's value
		{"pnpm", "dlx", "create-vite", "app"},
	} {
		if m := watchServeMatch(list, argv); m != "" {
			t.Errorf("%q matched %q, want no match", argv, m)
		}
	}
}
