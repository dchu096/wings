package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestSSHPortPool_FirstNonEmptyAlias(t *testing.T) {
	cfg := DevToolsConfiguration{
		SSHPorts: []int{40100, 40101},
		PortPool: []int{40200},
	}
	got := cfg.SSHPortPool()
	if len(got) != 2 || got[0] != 40100 || got[1] != 40101 {
		t.Fatalf("expected ssh_ports when ports empty, got %v", got)
	}

	cfg.Ports = []int{40000, 40000, 40001}
	got = cfg.SSHPortPool()
	if len(got) != 2 || got[0] != 40000 || got[1] != 40001 {
		t.Fatalf("expected primary ports unique, got %v", got)
	}
}

func TestSanitizeSSHPorts_Skips22AndInvalid(t *testing.T) {
	got := SanitizeSSHPorts([]int{22, 0, -1, 65536, 40100, 22, 40100, 40101})
	if len(got) != 2 || got[0] != 40100 || got[1] != 40101 {
		t.Fatalf("unexpected sanitize result: %v", got)
	}
}

func TestSSHPortPool_EmptyMeansRandom(t *testing.T) {
	cfg := DevToolsConfiguration{Ports: []int{22}}
	if got := cfg.SSHPortPool(); len(got) != 0 {
		t.Fatalf("expected empty after filtering 22, got %v", got)
	}
	if got := (DevToolsConfiguration{}).SSHPortPool(); got != nil && len(got) != 0 {
		t.Fatalf("expected empty pool, got %v", got)
	}
}

func TestFirstNonEmptySSHPortPool(t *testing.T) {
	got := FirstNonEmptySSHPortPool(nil, []int{}, []int{22, 40100}, []int{40200})
	if len(got) != 1 || got[0] != 40100 {
		t.Fatalf("expected first non-empty sanitized, got %v", got)
	}
}

func TestDevToolsPortsJSONRoundTrip(t *testing.T) {
	raw := []byte(`{"enabled":true,"ports":[40000,40001],"ssh_ports":[40000],"port_pool":[40002],"ssh_bind":"0.0.0.0"}`)
	var cfg DevToolsConfiguration
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ports) != 2 || cfg.Ports[0] != 40000 {
		t.Fatalf("json ports: %v", cfg.Ports)
	}
	if pool := cfg.SSHPortPool(); len(pool) != 2 {
		t.Fatalf("SSHPortPool from json: %v", pool)
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var again DevToolsConfiguration
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	if again.Ports[0] != 40000 || again.SSHPorts[0] != 40000 || again.PortPool[0] != 40002 {
		t.Fatalf("round-trip lost fields: %+v", again)
	}
}

func TestDevToolsPortsYAMLRoundTrip(t *testing.T) {
	raw := []byte(`
enabled: true
ssh_bind: "0.0.0.0"
ports:
  - 40100
  - 40101
ssh_ports:
  - 40100
`)
	var cfg DevToolsConfiguration
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if pool := cfg.SSHPortPool(); len(pool) != 2 || pool[0] != 40100 {
		t.Fatalf("yaml pool: %v", pool)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var again DevToolsConfiguration
	if err := yaml.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Ports) != 2 || again.Ports[1] != 40101 {
		t.Fatalf("yaml round-trip: %+v", again)
	}
}
