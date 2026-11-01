package logs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdklogs "github.com/omniviewdev/plugin-sdk/pkg/v1/logs"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const testUPID = "UPID:pve1:0001:0002:0003:vzdump::root@pam:"

// fakeTask serves one task's log and status; lines and stopped change as
// the test goes.
type fakeTask struct {
	mu      sync.Mutex
	lines   []string
	stopped bool
	starts  []int
}

func (f *fakeTask) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := "/api2/json/nodes/pve1/tasks/" + testUPID
	switch r.URL.Path {
	case base + "/status":
		status := "running"
		if f.stopped {
			status = "stopped"
		}
		_, _ = w.Write([]byte(`{"data":{"status":"` + status + `"}}`))
	case base + "/log":
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		f.starts = append(f.starts, start)
		data := []proxmox.TaskLogLine{}
		for i := start; i < len(f.lines) && len(data) < limit; i++ {
			data = append(data, proxmox.TaskLogLine{N: int64(i + 1), T: f.lines[i]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newTaskLog(t *testing.T, f *fakeTask) *taskLog {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return &taskLog{client: client, node: "pve1", upid: testUPID, interval: 10 * time.Millisecond}
}

func TestTaskBacklogPagesAndTails(t *testing.T) {
	f := &fakeTask{stopped: true}
	for i := range 1203 {
		f.lines = append(f.lines, "line "+strconv.Itoa(i+1))
	}
	tl := newTaskLog(t, f)

	r, err := tl.stream(context.Background(), sdklogs.LogStreamRequest{TailLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	if string(out) != "line 1202\nline 1203\n" {
		t.Fatalf("tail: %q", out)
	}
	if got := f.starts; len(got) != 3 || got[0] != 0 || got[1] != 500 || got[2] != 1000 {
		t.Fatalf("pages read from %v", got)
	}

	r, err = tl.stream(context.Background(), sdklogs.LogStreamRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = io.ReadAll(r)
	if n := strings.Count(string(out), "\n"); n != 1203 {
		t.Fatalf("whole log has %d lines", n)
	}
}

func TestTaskFollowEndsAfterTheTaskStops(t *testing.T) {
	f := &fakeTask{lines: []string{"starting"}}
	tl := newTaskLog(t, f)

	r, err := tl.stream(context.Background(), sdklogs.LogStreamRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	lines := collect(r)
	next := func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(2 * time.Second):
			t.Fatal("timed out")
			return ""
		}
	}
	if l := next(); l != "starting" {
		t.Fatalf("got %q", l)
	}
	f.mu.Lock()
	f.lines = append(f.lines, "working", "TASK OK")
	f.mu.Unlock()
	if a, b := next(), next(); a != "working" || b != "TASK OK" {
		t.Fatalf("got %q %q", a, b)
	}
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	// Let the follower see the stopped task and end its polling.
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	f.lines = append(f.lines, "after stop")
	reads := len(f.starts)
	f.mu.Unlock()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("followed stream closed after the task stopped")
		}
		t.Fatalf("unexpected line %q", l)
	case <-time.After(150 * time.Millisecond):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != reads {
		t.Fatalf("log polled %d more times after the task stopped", len(f.starts)-reads)
	}
	_ = r.Close()
}

func TestTaskEmptyLogPlaceholderIsDropped(t *testing.T) {
	f := &fakeTask{lines: []string{"no content"}}
	tl := newTaskLog(t, f)

	r, err := tl.stream(context.Background(), sdklogs.LogStreamRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	lines := collect(r)
	f.mu.Lock()
	f.lines = []string{"starting"}
	f.mu.Unlock()
	select {
	case l := <-lines:
		if l != "starting" {
			t.Fatalf("got %q, want the first real line", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	_ = r.Close()
	for _, start := range f.starts {
		if start != 0 {
			t.Fatalf("read from %d before any real line", start)
		}
	}
}

func TestTaskSourcesLabelNodeAndUPID(t *testing.T) {
	src := taskSources(testUPID, nil, sdklogs.LogSessionOptions{})
	if len(src) != 1 || src[0].ID != testUPID || src[0].Labels[labelNode] != "pve1" || src[0].Labels[labelUPID] != testUPID {
		t.Fatalf("sources %+v", src)
	}
	if src := taskSources("node/pve1", nil, sdklogs.LogSessionOptions{}); len(src) != 0 {
		t.Fatalf("non-task sources %+v", src)
	}
}
