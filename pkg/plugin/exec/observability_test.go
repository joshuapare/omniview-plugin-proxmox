package exec

import (
	"net/http"
	"strings"
	"testing"
	"time"

	logging "github.com/omniviewdev/plugin-sdk/log"
	sdkexec "github.com/omniviewdev/plugin-sdk/pkg/v1/exec"
)

func TestShellOpenFailureIsLoggedWithClassOnly(t *testing.T) {
	exporter, sink := captureObs(t)
	f := &fakePVE{t: t}
	s := startSession(t, handlerFor(t, keyNode, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "qemu/nope"}),
	})
	_ = stoppedWithExecError(t, s)
	time.Sleep(20 * time.Millisecond)

	var found bool
	for _, r := range sink.Records() {
		if r.Message == "shell target invalid" && r.Level == logging.LevelWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("no Warn for invalid target: %+v", sink.Records())
	}
	if strings.Contains(dump(exporter, sink), "qemu/nope") {
		t.Error("resource data leaked into log")
	}
}

func TestShellErrorOmitsProviderText(t *testing.T) {
	f := &fakePVE{t: t, termproxy: func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider says fake-provider-secret", http.StatusInternalServerError)
	}}
	s := startSession(t, handlerFor(t, keyNode, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "node/ares"}),
	})
	execErr := stoppedWithExecError(t, s)
	all := execErr.Title + execErr.Message + execErr.Suggestion + execErr.Error()
	if strings.Contains(all, "fake-provider-secret") || strings.Contains(all, "provider says") {
		t.Fatalf("provider text in client-facing error: %+v", execErr)
	}
	if !strings.Contains(execErr.Message, "api, HTTP 500") {
		t.Fatalf("Message = %q, want the class", execErr.Message)
	}
}
