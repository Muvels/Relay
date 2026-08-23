package agent

import (
	"context"
	"encoding/json"
	"testing"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// Every Relay container must carry an explicit, bounded log driver. Inheriting
// the daemon default means either unbounded json-file growth (a service that
// logs for weeks fills the disk) or a driver the agent cannot read back.
func TestContainerConfigPinsABoundedLogDriver(t *testing.T) {
	e := NewDockerExecutor(nil, t.TempDir())
	cfg, err := e.baseConfig(context.Background(),
		&relayv1.RunSpec{RunId: "run_1", ImageTag: "relay-img:x"}, runPaths{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostConfig.LogConfig.Type != "json-file" {
		t.Fatalf("log driver is %q, want json-file", cfg.HostConfig.LogConfig.Type)
	}
	for _, key := range []string{"max-size", "max-file"} {
		if cfg.HostConfig.LogConfig.Config[key] == "" {
			t.Fatalf("log driver has no %s, so it is unbounded", key)
		}
	}
	// The Engine API reads this off HostConfig, so the wire shape matters.
	var wire struct {
		HostConfig struct {
			LogConfig struct {
				Type   string            `json:"Type"`
				Config map[string]string `json:"Config"`
			} `json:"LogConfig"`
		} `json:"HostConfig"`
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.HostConfig.LogConfig.Type != "json-file" ||
		wire.HostConfig.LogConfig.Config["max-size"] == "" {
		t.Fatalf("LogConfig did not survive serialization: %s", encoded)
	}
}
