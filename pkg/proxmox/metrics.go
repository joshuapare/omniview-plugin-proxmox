package proxmox

import (
	"time"

	sdkdiag "github.com/omniviewdev/plugin-sdk/pkg/diagnostics"
)

var (
	apiCalls, _         = sdkdiag.Default().Counter("omniview.plugin.self.api.calls", "1")
	apiDuration, _      = sdkdiag.Default().Histogram("omniview.plugin.self.api.duration", "s", []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30})
	terminalSessions, _ = sdkdiag.Default().Counter("omniview.plugin.self.terminal.sessions", "1")
	taskWaits, _        = sdkdiag.Default().Counter("omniview.plugin.self.task.waits", "1")
	connectionChecks, _ = sdkdiag.Default().Counter("omniview.plugin.self.connection.checks", "1")
	sourcePolls, _      = sdkdiag.Default().Counter("omniview.plugin.self.source.polls", "1")
)

func metricAttr(key sdkdiag.Key, value string) sdkdiag.Attr {
	if !sdkdiag.ValidValue(value) {
		value = "other"
	}
	return sdkdiag.Attr{Key: key, Value: value}
}

func APICall(operation, outcome string, d time.Duration) {
	switch operation {
	case "read", "write", "delete":
	default:
		operation = "other"
	}
	switch outcome {
	case "ok", "client_error", "server_error", "failed":
	default:
		outcome = "other"
	}
	apiCalls.Add(1, metricAttr(sdkdiag.KeyOperation, operation), metricAttr(sdkdiag.KeyOutcome, outcome))
	apiDuration.Observe(d.Seconds(), metricAttr(sdkdiag.KeyOperation, operation))
}
func TerminalSession(state string) {
	switch state {
	case "opened", "failed", "closed":
	default:
		state = "other"
	}
	terminalSessions.Add(1, metricAttr(sdkdiag.KeyState, state))
}
func TaskWait(outcome string) {
	switch outcome {
	case "ok", "warnings", "failed", "timeout", "canceled":
	default:
		outcome = "other"
	}
	taskWaits.Add(1, metricAttr(sdkdiag.KeyOutcome, outcome))
}
func ConnectionCheck(outcome string) {
	switch outcome {
	case "connected", "unauthorized", "forbidden", "certificate_error", "connection_error", "timeout":
	default:
		outcome = "other"
	}
	connectionChecks.Add(1, metricAttr(sdkdiag.KeyOutcome, outcome))
}
func SourcePoll(outcome string) {
	switch outcome {
	case "ok", "terminated", "failed", "canceled":
	default:
		outcome = "other"
	}
	sourcePolls.Add(1, metricAttr(sdkdiag.KeyOutcome, outcome))
}
