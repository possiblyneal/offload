package integration

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serve answers every connection on l with "hello from dev" until the test ends.
func serve(t *testing.T, l net.Listener) int {
	t.Helper()
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			fmt.Fprintln(c, "hello from dev")
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// dial is a job-side command that connects to host:port and prints the first line.
func dial(host string, port int) []string {
	return []string{"bash", "-c", fmt.Sprintf(
		`exec 3<>/dev/tcp/%s/%d && head -n1 <&3`, host, port)}
}

func TestDevLoopbackAndWildcardListenersAnswerInTheJob(t *testing.T) {
	loop, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wild, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	for name, port := range map[string]int{"loopback only": serve(t, loop), "wildcard": serve(t, wild)} {
		t.Run(name, func(t *testing.T) {
			local, remote := both(t, dial("127.0.0.1", port)...)
			if local.stdout != "hello from dev\n" {
				t.Fatalf("local run did not reach the listener: %v", local)
			}
			same(t, local, remote)
		})
	}
}

func listenerPorts(ss string) []int {
	var ports []int
	for _, line := range strings.Split(ss, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if p, err := strconv.Atoi(f[3][strings.LastIndex(f[3], ":")+1:]); err == nil {
			ports = append(ports, p)
		}
	}
	return ports
}

// serverOnlyPort is a port the server listens on and dev does not.
func serverOnlyPort(t *testing.T) int {
	t.Helper()
	devPorts := map[int]bool{}
	for _, p := range listenerPorts(mustSh(t, false, "ss -ltnH")) {
		devPorts[p] = true
	}
	for _, p := range listenerPorts(mustSh(t, true, "ss -ltnH")) {
		if !devPorts[p] && p > 1024 {
			return p
		}
	}
	t.Skip("every port the server listens on is also a dev port")
	return 0
}

func TestPortsWithoutADevListenerAreRefused(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unused := free.Addr().(*net.TCPAddr).Port
	free.Close()

	server := serverOnlyPort(t)
	for name, argv := range map[string][]string{
		"no listener on dev":   dial("127.0.0.1", unused),
		"server loopback only": dial("127.0.0.1", server),
		"server address":       dial(serverAddr, server),
		"job's gateway": {"bash", "-c", fmt.Sprintf(
			`gw=$(ip -4 route show default | awk '{print $3}'); exec 3<>/dev/tcp/$gw/%d`, server)},
	} {
		t.Run(name, func(t *testing.T) {
			r := execIn(t, repo(t), true, argv...)
			if r.code == 0 || r.stdout != "" {
				t.Errorf("connected: code %d, stdout %q", r.code, r.stdout)
			}
		})
	}
}

func TestOutboundHTTPSWorks(t *testing.T) {
	r := execIn(t, repo(t), true, "curl", "--noproxy", "*", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com")
	if r.code != 0 || r.stdout != "200" {
		t.Errorf("code %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestDevsProxyAnswersInTheJob(t *testing.T) {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		t.Skip("dev has no HTTPS_PROXY")
	}
	r := execIn(t, repo(t), true, "curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com")
	if r.code != 0 || r.stdout != "200" {
		t.Errorf("through %s: code %d, stdout %q, stderr %q", proxy, r.code, r.stdout, r.stderr)
	}
}

// agent starts an ssh-agent of the test's own holding one fresh key, points the test's
// SSH_AUTH_SOCK at it so offload forwards it, and returns the key's fingerprint.
func agent(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(scratchRoot, fmt.Sprintf("agent-%d.sock", os.Getpid()))
	cmd := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); os.Remove(sock) })
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	keyFile := filepath.Join(scratchRoot, fmt.Sprintf("agent-key-%d", os.Getpid()))
	t.Cleanup(func() { os.Remove(keyFile); os.Remove(keyFile + ".pub") })
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	add := exec.Command("ssh-add", keyFile)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)
	out, err := exec.Command("ssh-keygen", "-lf", keyFile+".pub").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))[1]
}

func TestSSHAgentKeysAreListedInTheJob(t *testing.T) {
	fp := agent(t)
	r := execIn(t, repo(t), true, "ssh-add", "-l")
	if r.code != 0 || !strings.Contains(r.stdout, fp) {
		t.Errorf("code %d, stdout %q, stderr %q, want key %s", r.code, r.stdout, r.stderr, fp)
	}
}

func TestOtherUserSocketsFailLoudly(t *testing.T) {
	r := execIn(t, repo(t), true, "systemctl", "--user", "status")
	if r.code == 0 || !strings.Contains(r.stderr, "Failed to connect") {
		t.Errorf("code %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// In a file: an argument that names /run keeps the job local.
	dir := repo(t)
	writeScript(t, dir, "env.sh", `echo "dbus=${DBUS_SESSION_BUS_ADDRESS-unset}"; ls /run`+"\n")
	env := execIn(t, dir, true, "sh", "env.sh")
	if !strings.HasPrefix(env.stdout, "dbus=unset\n") || strings.Contains(env.stdout, "user") {
		t.Errorf("stdout %q", env.stdout)
	}
}

// A container on the server reaches dev through the server's NAT, so it would otherwise
// arrive at dev's export from the server's address with a privileged source port.
func TestBridgedContainerCannotOpenAnNFSConnectionToDev(t *testing.T) {
	out, err := sh(t, true, `sudo nsenter -t "$(sudo podman inspect -f '{{.State.Pid}}' node-exporter)" -n `+
		`timeout 5 bash -c 'exec 3<>/dev/tcp/`+devAddr+`/2049'`)
	if err == nil {
		t.Errorf("a container on the server connected to dev:2049")
	}
	if strings.Contains(out, "No such file") || strings.Contains(out, "inspect") {
		t.Fatalf("the probe did not run: %s", out)
	}
}
