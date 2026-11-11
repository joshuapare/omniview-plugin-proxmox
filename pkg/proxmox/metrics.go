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
	apiCalls.Add(1, metricAttr(sdkdiag.KeyOperation, operation), metricAttr(sdkdiag.KeyOutcome, outcome))
	apiDuration.Observe(d.Seconds(), metricAttr(sdkdiag.KeyOperation, operation))
}
func TerminalSession(state string) { terminalSessions.Add(1, metricAttr(sdkdiag.KeyState, state)) }
func TaskWait(outcome string)      { taskWaits.Add(1, metricAttr(sdkdiag.KeyOutcome, outcome)) }
func ConnectionCheck(outcome string) {
	connectionChecks.Add(1, metricAttr(sdkdiag.KeyOutcome, outcome))
}
func SourcePoll(outcome string) { sourcePolls.Add(1, metricAttr(sdkdiag.KeyOutcome, outcome)) }
