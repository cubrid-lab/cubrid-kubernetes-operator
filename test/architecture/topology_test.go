package architecture_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mermaidBlocks returns the Markdown Mermaid bodies in document order.
// The architecture docs use ordinary fenced blocks without code-fence nesting.
func mermaidBlocks(t *testing.T, filename string) []string {
	t.Helper()

	path := filepath.Join("..", "..", "docs", "architecture", filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	parts := strings.Split(string(raw), "```mermaid\n")
	var blocks []string
	for _, part := range parts[1:] {
		body, _, ok := strings.Cut(part, "\n```")
		if !ok {
			t.Fatalf("%s has an unterminated Mermaid block", filename)
		}
		blocks = append(blocks, body)
	}
	return blocks
}

// The system topology and the client-path page are two projections of the
// same HA installation. Keep the actual SQL graph and the runtime identities
// identical instead of accidentally hardwiring an ordinal as the master.
func TestCanonicalHASQLTopologyIsShared(t *testing.T) {
	topology := mermaidBlocks(t, "topology.md")
	dataPath := mermaidBlocks(t, "data-path.md")
	if len(topology) != 2 || len(dataPath) != 2 {
		t.Fatalf("want two Mermaid diagrams in each page, got topology=%d, data-path=%d", len(topology), len(dataPath))
	}
	if topology[1] != dataPath[0] {
		t.Fatal("topology data-path diagram differs from client/data-path SQL diagram; update both in the same PR")
	}

	for _, identity := range []string{
		"Service demo-rw", "TCP 33000", "Service demo-ro", "TCP 33001",
		"demo-broker-rw", "2 RW Broker Pods",
		"demo-broker-ro", "2 RO Broker Pods",
		"StatefulSet demo", "demo-0", "demo-1", "demo-2",
		"native RW selection: current Master", "DB TCP 1523",
	} {
		if !strings.Contains(topology[1], identity) {
			t.Errorf("canonical SQL topology lacks %q", identity)
		}
	}
	for _, pinned := range []string{
		"demo-0<br/>CUBRID server, master",
		"role=master",
	} {
		if strings.Contains(strings.ToLower(topology[1]), strings.ToLower(pinned)) {
			t.Errorf("SQL diagram hardwires or routes by a master ordinal: %q", pinned)
		}
	}
}
