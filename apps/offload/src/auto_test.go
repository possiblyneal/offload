package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBashAutomaticRouting(t *testing.T) {
	adapter, err := filepath.Abs("../scripts/auto.bash")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go": `if [ "$1" = env ]; then printf '%s\n' "$GOFLAGS"; exit; fi
printf 'go'; printf ' <%s>' "$@"; printf '\n'`,
		"offload": `printf 'offload'; printf ' <%s>' "$@"; printf '\n'; exit 42`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, command, want string
		code                int
	}{
		{"build", `go build './with space' </dev/null`, "offload <--> <" + bin + "/go> <build> <./with space>", 42},
		{"test", `go test ./... </dev/null`, "offload <--> <" + bin + "/go> <test> <./...>", 42},
		{"other command", `go version </dev/null`, "go <version>", 0},
		{"pipe", `printf input | go test`, "go <test>", 0},
		{"closed", `go test 0<&-`, "go <test>", 0},
		{"custom executor", `go test -exec=wrapper </dev/null`, "go <test> <-exec=wrapper>", 0},
		{"custom arguments", `go test -args -interactive </dev/null`, "go <test> <-args> <-interactive>", 0},
		{"unknown test flag", `go test ./... -myflag=1 </dev/null`, "go <test> <./...> <-myflag=1>", 0},
		{"standard test flags", `go test -run TestSmoke -count=1 ./... </dev/null`, "offload <--> <" + bin + "/go> <test> <-run> <TestSmoke> <-count=1> <./...>", 42},
		{"prefixed driver flag", `go test ./... -test.race=1 </dev/null`, "go <test> <./...> <-test.race=1>", 0},
		{"prefixed coverage flag", `go test ./... -test.cover=1 </dev/null`, "go <test> <./...> <-test.cover=1>", 0},
		{"real test alias", `go test ./... -test.run TestSmoke </dev/null`, "offload <--> <" + bin + "/go> <test> <./...> <-test.run> <TestSmoke>", 42},
		{"driver flags", `go test -race -tags=smoke ./... </dev/null`, "offload <--> <" + bin + "/go> <test> <-race> <-tags=smoke> <./...>", 42},
		{"dash-prefixed value", `go test ./... -run -smoke </dev/null`, "offload <--> <" + bin + "/go> <test> <./...> <-run> <-smoke>", 42},
		{"custom flag after value", `go test ./... -run -smoke -myflag </dev/null`, "go <test> <./...> <-run> <-smoke> <-myflag>", 0},
		{"boolean followed by custom flag", `go test ./... -v -myflag </dev/null`, "go <test> <./...> <-v> <-myflag>", 0},
		{"positional test argument", `go test ./... -v custom </dev/null`, "go <test> <./...> <-v> <custom>", 0},
		{"explicit output", `go build -o "$HOME/local go tool" </dev/null`, "go <build> <-o> <" + home + "/local go tool>", 0},
		{"go flags", `GOFLAGS=-toolexec=wrapper go build </dev/null`, "go <build>", 0},
		{"opt out", `OFFLOAD_AUTO=0 go build </dev/null`, "go <build>", 0},
		{"already offloaded", `OFFLOAD_JOB=1 go test </dev/null`, "go <test>", 0},
		{"child shell", `/bin/bash -c 'go test </dev/null'`, "go <test>", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("/bin/bash", "-c", `. "$1"; `+tt.command, "bash", adapter)
			cmd.Env = []string{"HOME=" + home, "PATH=" + bin}
			out, err := cmd.CombinedOutput()
			code := exitStatus(t, err)
			if code != tt.code || strings.TrimSpace(string(out)) != tt.want {
				t.Fatalf("got %q (exit %d, %v), want %q (exit %d)", out, code, err, tt.want, tt.code)
			}
		})
	}
}
