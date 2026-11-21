package installed

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A read on a member connects to that member's server over TCP by its own
// hostname, not through the local Unix socket that "localhost" uses, where
// lab members lost about one session in forty after printing the whole result
// (POC-70, #238).
func TestCSQLReadConnectsOverTCPByTheMembersHostname(t *testing.T) {
	var commands [][]string
	h := &haRun{Runner: &Runner{Command: func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, args)
		return "=== <Result of SELECT Command in Line 1> ===\n\n  1 row selected.\n", nil
	}}, guard: func(context.Context) error { return nil }}
	h.cfg.Namespace = "ns"
	if _, err := h.csqlRead(context.Background(), "ha-1")("select 1"); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 {
		t.Fatalf("ran %d commands, want 1: %q", len(commands), commands)
	}
	args := commands[0]
	if !slices.Contains(args, "ha-1") {
		t.Errorf("command %q does not run on the member ha-1", args)
	}
	script := strings.Join(args, " ")
	if strings.Contains(script, "@localhost") {
		t.Errorf("read connects through the local socket: %q", script)
	}
	byHostname := strings.Contains(script, `"$0@$(hostname)"`)
	if !byHostname || args[len(args)-2] != databaseName || args[len(args)-1] != "select 1" {
		t.Errorf("read does not connect to %s by the member's hostname with the query as its argument: %q",
			databaseName, args)
	}
}
