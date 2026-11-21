package main

import (
	"reflect"
	"testing"
)

func TestParseListenersKeepsWhatAnswersOnLoopback(t *testing.T) {
	ss := `LISTEN 0      4096       127.0.0.1:9877       0.0.0.0:*
LISTEN 0      4096         0.0.0.0:3000       0.0.0.0:*
LISTEN 0      4096               *:3005             *:*
LISTEN 0      128             [::]:8123          [::]:*
LISTEN 0      4096           [::1]:5000          [::]:*
LISTEN 0      1024       10.89.0.1:53         0.0.0.0:*
LISTEN 0      4096   127.0.0.53%lo:53         0.0.0.0:*
LISTEN 0      4096     192.0.2.50:3113       0.0.0.0:*
`
	want := []forward{
		{port: 3000, dest: "127.0.0.1"},
		{port: 3005, dest: "127.0.0.1"},
		{port: 5000, dest: "::1"},
		{port: 8123, dest: "::1"},
		{port: 9877, dest: "127.0.0.1"},
	}
	if got := parseListeners(ss); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestParseListenersPrefersIPv4AndListsEachPortOnce(t *testing.T) {
	ss := `LISTEN 0 4096 [::]:2049 [::]:*
LISTEN 0 4096 0.0.0.0:2049 0.0.0.0:*
LISTEN 0 4096 [::1]:7000 [::]:*
LISTEN 0 4096 127.0.0.1:7000 0.0.0.0:*
`
	want := []forward{{port: 2049, dest: "127.0.0.1"}, {port: 7000, dest: "127.0.0.1"}}
	if got := parseListeners(ss); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestForwardSocketAndSSHSpec(t *testing.T) {
	v4 := forward{port: 8765, dest: "127.0.0.1"}
	v6 := forward{port: 5000, dest: "::1"}
	if got := v4.socket("ab12"); got != "/tmp/offload-ab12-8765.sock" {
		t.Errorf("socket = %s", got)
	}
	if got := v4.sshSpec("ab12"); got != "/tmp/offload-ab12-8765.sock:127.0.0.1:8765" {
		t.Errorf("spec = %s", got)
	}
	if got := v6.sshSpec("ab12"); got != "/tmp/offload-ab12-5000.sock:[::1]:5000" {
		t.Errorf("spec = %s", got)
	}
}
