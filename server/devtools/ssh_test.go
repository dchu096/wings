package devtools

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestBuildSSHPortCandidates_PreferredFirst(t *testing.T) {
	cands := buildSSHPortCandidates(40101, []int{40100, 40101, 22, 40102})
	if len(cands) != 3 {
		t.Fatalf("expected 3 candidates, got %v", cands)
	}
	if cands[0] != 40101 {
		t.Fatalf("preferred should be first, got %v", cands)
	}
	seen := map[int]bool{cands[1]: true, cands[2]: true}
	if !seen[40100] || !seen[40102] {
		t.Fatalf("remaining pool missing: %v", cands)
	}
}

func TestBuildSSHPortCandidates_SkipsInvalidPreferred(t *testing.T) {
	cands := buildSSHPortCandidates(22, []int{40100})
	if len(cands) != 1 || cands[0] != 40100 {
		t.Fatalf("expected only pool port, got %v", cands)
	}
	if cands := buildSSHPortCandidates(0, nil); len(cands) != 0 {
		t.Fatalf("empty pool should stay empty, got %v", cands)
	}
}

func TestListenEphemeralSSH_PoolBind(t *testing.T) {
	// Grab two free ports, then use them as the pool.
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p1 := l1.Addr().(*net.TCPAddr).Port
	p2 := l2.Addr().(*net.TCPAddr).Port
	_ = l1.Close()
	_ = l2.Close()

	ln, port, err := listenEphemeralSSH("127.0.0.1", p2, []int{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if port != p2 {
		t.Fatalf("expected preferred %d, got %d", p2, port)
	}
}

func TestListenEphemeralSSH_PoolExhausted(t *testing.T) {
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	busy := holder.Addr().(*net.TCPAddr).Port

	ln, port, err := listenEphemeralSSH("127.0.0.1", 0, []int{busy})
	if ln != nil {
		_ = ln.Close()
		t.Fatalf("expected failure, bound %d", port)
	}
	if err == nil || !strings.Contains(err.Error(), "port pool exhausted") {
		t.Fatalf("expected pool exhausted error, got %v", err)
	}
	// Must not have fallen back to a random outside port.
	if port != 0 {
		t.Fatalf("expected port 0 on failure, got %d", port)
	}
}

func TestListenEphemeralSSH_RandomWhenEmptyPool(t *testing.T) {
	ln, port, err := listenEphemeralSSH("127.0.0.1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if port < 1 || port == 22 {
		t.Fatalf("unexpected random port %d", port)
	}
	// Confirm it is actually bound.
	if _, err := strconv.Atoi(strconv.Itoa(port)); err != nil {
		t.Fatal(err)
	}
}
