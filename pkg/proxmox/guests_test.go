package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type recorded struct {
	method string
	path   string
	query  url.Values
	form   url.Values
}

// guestServer serves the /cluster/resources guest list and records every
// other request, answering it with data.
func guestServer(t *testing.T, data string, got *[]recorded) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/cluster/resources" {
			_, _ = w.Write([]byte(`{"data":[{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve2","name":"web01"},` +
				`{"id":"lxc/200","type":"lxc","vmid":200,"node":"pve1"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("bad form body %q: %v", body, err)
		}
		if len(body) > 0 && r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("form sent as %q", r.Header.Get("Content-Type"))
		}
		*got = append(*got, recorded{method: r.Method, path: r.URL.Path, query: r.URL.Query(), form: form})
		_, _ = w.Write([]byte(`{"data":` + data + `}`))
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestFindGuestResolvesCurrentNode(t *testing.T) {
	var got []recorded
	client := guestServer(t, `null`, &got)
	ctx := context.Background()

	g, err := client.FindGuest(ctx, "qemu/100")
	if err != nil {
		t.Fatal(err)
	}
	if g.Node != "pve2" || g.Name != "web01" || g.Label() != "web01 (qemu/100)" {
		t.Fatalf("unexpected guest: %+v", g)
	}
	if g, err := client.FindGuest(ctx, "lxc/200"); err != nil || g.Label() != "lxc/200" {
		t.Fatalf("lxc/200: %+v, %v", g, err)
	}
	for _, id := range []string{"qemu/999", "node/pve1", "100"} {
		var nf *GuestNotFoundError
		if _, err := client.FindGuest(ctx, id); !errors.As(err, &nf) || nf.ID != id {
			t.Fatalf("%s: expected GuestNotFoundError, got %v", id, err)
		}
	}
}

func TestGuestStatusActionPostsToStatusPath(t *testing.T) {
	var got []recorded
	client := guestServer(t, `"UPID:pve2:0001:start"`, &got)
	g := GuestRef{ID: "qemu/100", Node: "pve2"}

	upid, err := client.GuestStatusAction(context.Background(), g, "start")
	if err != nil {
		t.Fatal(err)
	}
	if upid != "UPID:pve2:0001:start" {
		t.Fatalf("upid %q", upid)
	}
	if len(got) != 1 || got[0].method != http.MethodPost || got[0].path != "/api2/json/nodes/pve2/qemu/100/status/start" {
		t.Fatalf("unexpected requests: %+v", got)
	}
}

func TestDeleteGuestPurgesAndDestroysDisks(t *testing.T) {
	var got []recorded
	client := guestServer(t, `"UPID:pve1:0002:destroy"`, &got)

	upid, err := client.DeleteGuest(context.Background(), GuestRef{ID: "lxc/200", Node: "pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if upid != "UPID:pve1:0002:destroy" || len(got) != 1 {
		t.Fatalf("upid %q, requests %+v", upid, got)
	}
	r := got[0]
	if r.method != http.MethodDelete || r.path != "/api2/json/nodes/pve1/lxc/200" {
		t.Fatalf("unexpected request %s %s", r.method, r.path)
	}
	if r.query.Get("purge") != "1" || r.query.Get("destroy-unreferenced-disks") != "1" {
		t.Fatalf("unexpected query %v", r.query)
	}
}

func TestGuestConfigSplitsDigest(t *testing.T) {
	var got []recorded
	client := guestServer(t, `{"cores":4,"name":"web01","digest":"abc123"}`, &got)

	config, digest, err := client.GuestConfig(context.Background(), GuestRef{ID: "qemu/100", Node: "pve2"})
	if err != nil {
		t.Fatal(err)
	}
	if digest != "abc123" {
		t.Fatalf("digest %q", digest)
	}
	if _, ok := config["digest"]; ok || config["name"] != "web01" || config["cores"] != json.Number("4") {
		t.Fatalf("unexpected config %v", config)
	}
	if got[0].method != http.MethodGet || got[0].path != "/api2/json/nodes/pve2/qemu/100/config" {
		t.Fatalf("unexpected request %+v", got[0])
	}
}

func TestVMPendingReadsCurrentPendingAndDeletions(t *testing.T) {
	var got []recorded
	client := guestServer(t, `[{"key":"cores","value":"2","pending":"4"},{"key":"net0","delete":2},{"key":"name","pending":""}]`, &got)
	entries, err := client.VMPending(context.Background(), GuestRef{ID: "qemu/100", Node: "pve2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].(map[string]any)["pending"] != "4" ||
		entries[1].(map[string]any)["delete"] != json.Number("2") ||
		entries[2].(map[string]any)["pending"] != "" {
		t.Fatalf("unexpected pending entries: %#v", entries)
	}
	if got[0].method != http.MethodGet || got[0].path != "/api2/json/nodes/pve2/qemu/100/pending" {
		t.Fatalf("unexpected request %+v", got[0])
	}
}

func TestUpdateGuestConfigEncodesForm(t *testing.T) {
	var got []recorded
	client := guestServer(t, `null`, &got)

	err := client.UpdateGuestConfig(context.Background(), GuestRef{ID: "qemu/100", Node: "pve2"},
		map[string]string{"cores": "4", "onboot": "1", "description": "a b&c"},
		[]string{"tags", "balloon"}, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	r := got[0]
	if r.method != http.MethodPut || r.path != "/api2/json/nodes/pve2/qemu/100/config" {
		t.Fatalf("unexpected request %s %s", r.method, r.path)
	}
	want := url.Values{
		"cores": {"4"}, "onboot": {"1"}, "description": {"a b&c"},
		"delete": {"balloon,tags"}, "digest": {"abc123"},
	}
	if r.form.Encode() != want.Encode() {
		t.Fatalf("form %v, want %v", r.form, want)
	}
}

func TestCreateGuestPostsVMIDAndConfig(t *testing.T) {
	var got []recorded
	client := guestServer(t, `"UPID:pve1:0001:create"`, &got)

	upid, err := client.CreateGuest(context.Background(), "pve1", "lxc", 300, map[string]string{"hostname": "ct3", "vmid": "999"})
	if err != nil || upid != "UPID:pve1:0001:create" {
		t.Fatalf("CreateGuest: %q %v", upid, err)
	}
	r := got[0]
	if r.method != http.MethodPost || r.path != "/api2/json/nodes/pve1/lxc" ||
		r.form.Encode() != (url.Values{"vmid": {"300"}, "hostname": {"ct3"}}).Encode() {
		t.Fatalf("unexpected request %+v", r)
	}
}

func TestGuestWriteMethodsPostToTheirPaths(t *testing.T) {
	var got []recorded
	client := guestServer(t, `"UPID:pve2:0001:x"`, &got)
	ctx := context.Background()
	g := GuestRef{ID: "qemu/100", Node: "pve2"}

	calls := []struct {
		call   func() (string, error)
		method string
		path   string
		form   url.Values
	}{
		{func() (string, error) { return client.CloneGuest(ctx, g, url.Values{"newid": {"101"}}) },
			http.MethodPost, "/clone", url.Values{"newid": {"101"}}},
		{func() (string, error) { return client.MigrateGuest(ctx, g, url.Values{"target": {"pve1"}}) },
			http.MethodPost, "/migrate", url.Values{"target": {"pve1"}}},
		{func() (string, error) { return client.CreateSnapshot(ctx, g, "pre", "before upgrade", true) },
			http.MethodPost, "/snapshot", url.Values{"snapname": {"pre"}, "description": {"before upgrade"}, "vmstate": {"1"}}},
		{func() (string, error) { return client.DeleteSnapshot(ctx, g, "pre") },
			http.MethodDelete, "/snapshot/pre", url.Values{}},
		{func() (string, error) { return client.RollbackSnapshot(ctx, g, "pre") },
			http.MethodPost, "/snapshot/pre/rollback", url.Values{}},
	}
	for i, c := range calls {
		upid, err := c.call()
		if err != nil || upid != "UPID:pve2:0001:x" {
			t.Fatalf("call %d: %q %v", i, upid, err)
		}
		r := got[i]
		if r.method != c.method || r.path != "/api2/json/nodes/pve2/qemu/100"+c.path || r.form.Encode() != c.form.Encode() {
			t.Fatalf("call %d: unexpected request %+v", i, r)
		}
	}
}

func TestAgentExecSendsOneCommandValuePerArgument(t *testing.T) {
	var got []recorded
	client := guestServer(t, `{"pid":42}`, &got)
	g := GuestRef{ID: "qemu/100", Node: "pve2"}

	pid, err := client.AgentExec(context.Background(), g, []string{"sh", "-c", "echo hi"}, "in")
	if err != nil || pid != 42 {
		t.Fatalf("AgentExec: %d %v", pid, err)
	}
	r := got[0]
	if r.method != http.MethodPost || r.path != "/api2/json/nodes/pve2/qemu/100/agent/exec" ||
		strings.Join(r.form["command"], "|") != "sh|-c|echo hi" || r.form.Get("input-data") != "in" {
		t.Fatalf("unexpected request %+v", r)
	}
}

func TestAgentExecStatusAndSnapshotsRead(t *testing.T) {
	var got []recorded
	client := guestServer(t, `{"exited":1,"exitcode":3,"out-data":"hi\n","out-truncated":1}`, &got)
	g := GuestRef{ID: "qemu/100", Node: "pve2"}

	status, err := client.AgentExecStatus(context.Background(), g, 42)
	if err != nil || status.Exited != 1 || status.ExitCode == nil || *status.ExitCode != 3 ||
		status.OutData != "hi\n" || status.OutTruncated != 1 {
		t.Fatalf("AgentExecStatus: %+v %v", status, err)
	}
	if r := got[0]; r.method != http.MethodGet || r.path != "/api2/json/nodes/pve2/qemu/100/agent/exec-status" || r.query.Get("pid") != "42" {
		t.Fatalf("unexpected request %+v", r)
	}

	client = guestServer(t, `[{"name":"pre"},{"name":"current"}]`, &got)
	snaps, err := client.GuestSnapshots(context.Background(), g)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("GuestSnapshots: %s %v", snaps, err)
	}
	if r := got[1]; r.method != http.MethodGet || r.path != "/api2/json/nodes/pve2/qemu/100/snapshot" {
		t.Fatalf("unexpected request %+v", r)
	}
}
