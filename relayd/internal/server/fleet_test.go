package server

import (
	"testing"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

func TestLivenessHeartbeatDoesNotEraseTelemetry(t *testing.T) {
	session := &Session{}
	sample := &relayv1.Heartbeat{
		SampledAtUnixMs:      time.Now().UnixMilli(),
		MemoryUsedMib:        123,
		MemoryUsageAvailable: true,
	}
	session.Beat(sample)
	session.Beat(&relayv1.Heartbeat{})

	got, receivedAt := session.Usage()
	if got != sample {
		t.Fatal("empty liveness heartbeat replaced telemetry")
	}
	if receivedAt.IsZero() {
		t.Fatal("telemetry receipt time was not recorded")
	}
}
