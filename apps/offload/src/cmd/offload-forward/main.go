// Command offload-forward answers dev's loopback ports on a job's own loopback.
//
//	usage: offload-forward SOCKET...
//
// Run in the job's network namespace but the host's mount namespace, so the sockets keep
// their paths when the job's root is pivoted. Each SOCKET is /tmp/offload-<id>-<port>.sock,
// a unix socket that sshd binds on this machine when the job's ssh connection carries
// `-R SOCKET:127.0.0.1:<port>`; it leads to that port on dev. This listens on
// 127.0.0.1:<port> and relays each connection to the socket. A port already bound in the
// namespace is skipped. Prints "ready" once every listener is up: a connection made from
// then on is relayed.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var portOf = regexp.MustCompile(`-([0-9]+)\.sock$`)

func main() {
	// The launcher's job half ignores these, and the relay must outlive a signal forwarded
	// to the job (setsid keeps it out of the group too); Go would reset all but SIGHUP and
	// SIGINT to fatal, so they are ignored here again.
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT,
		syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGUSR2)
	var listeners []net.Listener
	var socks []string
	for _, sock := range os.Args[1:] {
		m := portOf.FindStringSubmatch(sock)
		if m == nil {
			fmt.Fprintf(os.Stderr, "offload-forward: %s: not /tmp/offload-<id>-<port>.sock\n", sock)
			os.Exit(1)
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+m[1])
		if err != nil {
			continue // the port is taken in the namespace
		}
		listeners, socks = append(listeners, l), append(socks, sock)
	}
	fmt.Println("ready")
	var wg sync.WaitGroup
	for i, l := range listeners {
		wg.Go(func() { serve(l, socks[i]) })
	}
	wg.Wait()
}

func serve(l net.Listener, sock string) {
	for {
		c, err := l.Accept()
		if err != nil {
			time.Sleep(50 * time.Millisecond) // out of descriptors, say: try again shortly
			continue
		}
		go relay(c.(*net.TCPConn), sock)
	}
}

// relay copies each way between the client and the socket, passing on each side's end of
// output as the other's, until both are done.
func relay(client *net.TCPConn, sock string) {
	defer client.Close()
	c, err := net.Dial("unix", sock)
	if err != nil {
		return
	}
	up := c.(*net.UnixConn)
	defer up.Close()
	var wg sync.WaitGroup
	wg.Go(func() { pump(client, up) })
	pump(up, client)
	wg.Wait()
}

func pump(dst interface {
	io.Writer
	CloseWrite() error
}, src io.Reader) {
	_, _ = io.Copy(dst, src)
	_ = dst.CloseWrite()
}
