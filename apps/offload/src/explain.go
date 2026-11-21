package main

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// probeSlots asks a remote how many of its slots are free, for --explain. A remote that
// does not answer is assumed to have them all free, as a run assumes, and the state says
// why it is unknown: the run then finds the remote unreachable, as it would without
// --explain.
func probeSlots(cfg config, name string) slotState {
	total := cfg.slotCount(name)
	out, err := exec.Command("ssh", "-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		cfg.remotes[name], "sudo -n "+launcherPath+" --slots "+strconv.Itoa(total)).Output()
	if err != nil {
		return slotState{Total: total, Free: total, Unknown: err.Error()}
	}
	free, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return slotState{Total: total, Free: total, Unknown: err.Error()}
	}
	return slotState{Total: total, Free: free}
}

// explainPlacement prints the slots, the refusals that apply and the deciding rule.
func explainPlacement(w io.Writer, req placeRequest, d decision) {
	for _, name := range req.Remotes {
		s := req.Slots[name]
		if s.Unknown != "" {
			fmt.Fprintf(w, "offload: slots %s unknown (%s), assumed free\n", name, s.Unknown)
			continue
		}
		fmt.Fprintf(w, "offload: slots %s %d of %d free\n", name, s.Free, s.Total)
	}
	hard, soft := refusals(req.Facts)
	for _, r := range append(hard, soft...) {
		fmt.Fprintf(w, "offload: refusal %s: %s\n", r.Rule, r.Reason)
	}
	fmt.Fprintf(w, "offload: placement %s by %s: %s\n", d.Placement, d.Rule, d.Reason)
}
