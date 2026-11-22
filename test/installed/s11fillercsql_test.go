package installed

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The graceful-stop write filler connects to the member's server over TCP by
// the member Pod's own hostname, not through the local Unix socket that
// "localhost" uses, where lab members lost about one session in forty after
// printing the whole result (POC-70, #238). The filler still sends each
// statement exactly once.
func TestFillerCSQLConnectsOverTCPByTheMembersHostname(t *testing.T) {
	var commands [][]string
	h := &haRun{Runner: &Runner{Command: func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, args)
		return "", nil
	}}, guard: func(context.Context) error { return nil }}
	h.cfg.Namespace = "ns"
	h.fillerSQL(context.Background(), fillerSeed)
	if len(commands) != 1 {
		t.Fatalf("ran %d commands, want 1 (a write is never sent again): %q", len(commands), commands)
	}
	args := commands[0]
	if !slices.Contains(args, s11GracefulStopMember) {
		t.Errorf("command %q does not run on the member %s", args, s11GracefulStopMember)
	}
	script := strings.Join(args, " ")
	if strings.Contains(script, "@localhost") {
		t.Errorf("filler connects through the local socket: %q", script)
	}
	if !strings.Contains(script, `"$0@$(hostname)"`) ||
		args[len(args)-2] != databaseName || args[len(args)-1] != fillerSeed {
		t.Errorf("filler does not connect to %s by the member's hostname with the statement as its argument: %q",
			databaseName, args)
	}
}
