package main

// Each case runs this test binary as offload-forward, in a process of its own as the
// launcher runs it, when TestMain sees forwardEnv.

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

const forwardEnv = "OFFLOAD_FORWARD_UNDER_TEST"

func TestMain(m *testing.M) {
	if os.Getenv(forwardEnv) == "" {
		os.Exit(m.Run())
	}
	main()
	os.Exit(0)
}

// forwardRun starts offload-forward with one socket per port, each served by a unix
// listener that answers "relayed:" and what it read, and returns its first stdout line.
// The sockets are abstract (`@…`): a temporary directory's path may pass the 108-byte limit.
func forwardRun(t *testing.T, ports ...int) string {
	t.Helper()
	var socks []string
	for _, p := range ports {
		sock := "@offload-t-" + strconv.Itoa(os.Getpid()) + "-" + strconv.Itoa(p) + ".sock"
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 100)
				n, _ := c.Read(buf)
				c.Write(append([]byte("relayed:"), buf[:n]...))
				c.Close()
			}
		}()
		socks = append(socks, sock)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, socks...)
	cmd.Env = append(os.Environ(), forwardEnv+"=1")
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(out).ReadString('\n'); line <- l }()
	select {
	case l := <-line:
		if l == "" {
			cmd.Wait()
			t.Fatalf("offload-forward printed nothing: %s", errOut.String())
		}
		return l
	case <-time.After(10 * time.Second):
		t.Fatalf("offload-forward printed nothing in 10 s: %s", errOut.String())
		return ""
	}
}

// freePort is a loopback port nothing listens on at the time of the call.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// dialRead connects to the loopback port, sends msg and returns all it reads until EOF.
func dialRead(t *testing.T, port int, msg string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatalf("port %d: %v", port, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	return string(got)
}

// "ready" means every listener is up: a connection made right after it is relayed to the
// port's socket. A port already bound in the namespace is skipped, not fatal, and keeps
// answering as its own listener does.
func TestForwardRelaysEachPortAndSkipsOneAlreadyBound(t *testing.T) {
	own, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	go func() {
		for {
			c, err := own.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("own"))
			c.Close()
		}
	}()
	bound, free := own.Addr().(*net.TCPAddr).Port, freePort(t)
	if line := forwardRun(t, bound, free); line != "ready\n" {
		t.Fatalf("first line %q, want \"ready\"", line)
	}
	if got := dialRead(t, free, "ping"); got != "relayed:ping" {
		t.Errorf("the forwarded port answered %q, want the socket's \"relayed:ping\"", got)
	}
	if got := dialRead(t, bound, "ping"); got != "own" {
		t.Errorf("the already-bound port answered %q, want its own listener's \"own\"", got)
	}
}

// Each side's end of output reaches the other, so a client that half-closes after its
// request still gets the whole reply, and the relay passes many connections at once.
func TestForwardPassesOnEachSidesEndOfOutput(t *testing.T) {
	port := freePort(t)
	sock := "@offload-t-" + strconv.Itoa(os.Getpid()) + "-eof-" + strconv.Itoa(port) + ".sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { // echo until the client's end of output, then end its own
				io.Copy(c, c)
				c.(*net.UnixConn).CloseWrite()
				c.Close()
			}()
		}
	}()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, sock)
	cmd.Env = append(os.Environ(), forwardEnv+"=1")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if l, _ := bufio.NewReader(out).ReadString('\n'); l != "ready\n" {
		t.Fatalf("first line %q, want \"ready\"", l)
	}
	msg := bytes.Repeat([]byte("0123456789"), 100_000)
	done := make(chan string, 8)
	for range 8 {
		go func() {
			c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
			if err != nil {
				done <- err.Error()
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			go func() { c.Write(msg); c.(*net.TCPConn).CloseWrite() }()
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, msg) {
				done <- "got " + strconv.Itoa(len(got)) + " bytes back, " + strconv.Quote(errString(err))
				return
			}
			done <- ""
		}()
	}
	for range 8 {
		if e := <-done; e != "" {
			t.Error(e)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
