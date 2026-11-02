package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A token's UPID holds "user@realm!tokenid". Its escaped segment must reach
// Proxmox escaped once: escaped twice, "!" arrives as a literal "%21" and
// Proxmox answers "no such task" for a task that exists.
func TestWaitTaskAddressesATokenUPIDExactly(t *testing.T) {
	const upid = "UPID:pve1:0006062F:1C8ABE39:6AE2CDA2:qmdestroy:100:omniview@pve!omniview:"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/tasks/"+upid+"/status" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"data":null,"errors":{"upid":"no such task"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
	}))
	defer srv.Close()
	client, err := NewClient(Cluster{ID: "c", URL: srv.URL, TokenID: "omniview@pve!omniview", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WaitTask(context.Background(), "pve1", upid); err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
}

func TestTaskReadsAddressTheirPaths(t *testing.T) {
	const upid = "UPID:pve1:0001:0002:0003:qmstart:100:root@pam:"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/api2/json/cluster/tasks":
			_, _ = w.Write([]byte(`{"data":[{"upid":"` + upid + `","node":"pve1"}]}`))
		case "/api2/json/nodes/pve1/tasks/" + upid + "/status":
			_, _ = w.Write([]byte(`{"data":{"upid":"` + upid + `","status":"stopped"}}`))
		case "/api2/json/nodes/pve1/tasks/" + upid + "/log":
			_, _ = w.Write([]byte(`{"data":[{"n":1,"t":"starting"},{"n":2,"t":"TASK OK"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client, err := NewClient(Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tasks, err := client.ClusterTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != upid {
		t.Fatalf("ClusterTasks: %+v %v", tasks, err)
	}
	raw, err := client.TaskStatus(ctx, "pve1", upid)
	if err != nil || !strings.Contains(string(raw), `"stopped"`) {
		t.Fatalf("TaskStatus: %s %v", raw, err)
	}
	lines, err := client.TaskLog(ctx, "pve1", upid, 0, 500)
	if err != nil || len(lines) != 2 || lines[1].N != 2 || lines[1].T != "TASK OK" {
		t.Fatalf("TaskLog: %+v %v", lines, err)
	}
	if paths[2] != "/api2/json/nodes/pve1/tasks/"+upid+"/log?limit=500&start=0" {
		t.Fatalf("log read at %s", paths[2])
	}
}

func TestTaskNodeParsesUPIDs(t *testing.T) {
	for upid, want := range map[string]string{"UPID:pve1:0001:x": "pve1", "UPID::x": "", "pve1:x": "", "UPID:pve1": ""} {
		if got, ok := TaskNode(upid); got != want || ok != (want != "") {
			t.Errorf("%s: %q %v", upid, got, ok)
		}
	}
}
