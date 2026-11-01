package resource

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// fakePVE answers the guest list, guest configs and task status, and
// records write requests. Writes answer write (status, body).
type fakePVE struct {
	t          *testing.T
	mu         sync.Mutex
	writes     []recorded
	writeCode  int
	writeBody  string
	taskStatus string // "stopped" or "running"
	exitStatus string
}

type recorded struct {
	method string
	path   string
	query  url.Values
	form   url.Values
}

func newFakePVE(t *testing.T) (*fakePVE, *proxmox.Client) {
	t.Helper()
	f := &fakePVE{t: t, writeCode: http.StatusOK, writeBody: `{"data":"UPID:pve2:00001:task"}`, taskStatus: "stopped", exitStatus: "OK"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return f, client
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	switch {
	case r.Method == http.MethodGet && path == "/cluster/resources":
		_, _ = w.Write([]byte(`{"data":[{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve2","name":"web01","status":"running"},` +
			`{"id":"lxc/200","type":"lxc","vmid":200,"node":"pve1","name":"ct","status":"stopped"}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/config"):
		_, _ = w.Write([]byte(`{"data":{"cores":2,"scsi0":"local-lvm:vm-100-disk-0,size=8G","digest":"d1"}}`))
	case r.Method == http.MethodGet && strings.Contains(path, "/tasks/"):
		if path != "/nodes/pve2/tasks/UPID:pve2:00001:task/status" {
			f.t.Errorf("task polled at %s", path)
		}
		_, _ = w.Write([]byte(`{"data":{"status":"` + f.taskStatus + `","exitstatus":"` + f.exitStatus + `"}}`))
	default:
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		f.writes = append(f.writes, recorded{method: r.Method, path: path, query: r.URL.Query(), form: form})
		w.WriteHeader(f.writeCode)
		_, _ = w.Write([]byte(f.writeBody))
	}
}

var ctx = context.Background()

var (
	vms = guestKind(typeVM, "Virtual Machines", "virtual machine", "qemu")
	cts = guestKind(typeContainer, "Containers", "container", "lxc")
)

func opError(t *testing.T, k *kind, err error) *resourcev2.ResourceOperationError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var opErr *resourcev2.ResourceOperationError
	if !errors.As(k.ClassifyError(err), &opErr) {
		t.Fatalf("not classified: %v", err)
	}
	return opErr
}

func ref(typ, id string) resourcev2.ResourceReference {
	return reference(providerID, "test-authority", typ, id)
}

func act(k *kind, client *proxmox.Client, action, id string) (map[string]any, error) {
	v, err := k.action(ctx, client, resourcev2.ActionRequest{
		Route: resourcev2.AccessRoute{Connection: "c"}, Reference: ref(k.typ, id), Action: action,
	})
	if err != nil {
		return nil, err
	}
	native, err := resourcev2.NativeFromValue(v)
	if err != nil {
		return nil, err
	}
	return native.(map[string]any), nil
}

func mutate(k *kind, client *proxmox.Client, op, id string, input map[string]any) (resourcev2.Record, error) {
	in, err := resourcev2.ValueFromNative(input)
	if err != nil {
		return resourcev2.Record{}, err
	}
	return k.mutate(ctx, client, resourcev2.MutationRequest{
		Route: resourcev2.AccessRoute{Connection: "c"}, Reference: ref(k.typ, id), Operation: op, Input: in,
	})
}

// kindExpectation is the write surface one type declares.
type kindExpectation struct {
	writes    []resourcev2.Operation
	actions   string
	dangerous []string
	watched   bool
}

func TestKindsDeclareTheirWrites(t *testing.T) {
	guestWrites := []resourcev2.Operation{
		resourcev2.OperationAction, resourcev2.OperationMutationUpdate, resourcev2.OperationDelete,
	}
	want := map[string]kindExpectation{
		typeVM: {
			writes: guestWrites, watched: true, dangerous: []string{"stop"},
			actions: "start,shutdown,reboot,stop,suspend,resume,config",
		},
		typeContainer: {
			writes: guestWrites, watched: true, dangerous: []string{"stop"},
			actions: "start,shutdown,reboot,stop,config",
		},
	}
	allWrites := []resourcev2.Operation{
		resourcev2.OperationAction, resourcev2.OperationCreate, resourcev2.OperationMutationUpdate, resourcev2.OperationDelete,
	}
	for _, k := range append(append([]*kind{}, staticKinds...), cephKinds...) {
		exp, ok := want[k.typ]
		if !ok {
			exp = kindExpectation{watched: true}
		}
		r := k.registration()
		var declared []resourcev2.Operation
		for _, c := range k.resourceType().Capabilities {
			declared = append(declared, c.Operation)
		}
		for _, op := range allWrites {
			if got, w := containsOp(declared, op), containsOp(exp.writes, op); got != w {
				t.Errorf("%s: declares %s = %v, want %v", k.typ, op, got, w)
			}
		}
		if got := containsOp(declared, resourcev2.OperationUpdate); got != exp.watched || (r.Watch != nil) != exp.watched {
			t.Errorf("%s: watched = %v/%v, want %v", k.typ, got, r.Watch != nil, exp.watched)
		}
		if (r.Mutate != nil) != (len(exp.writes) > 0) {
			t.Errorf("%s: mutate registered = %v", k.typ, r.Mutate != nil)
		}
		if (r.Action != nil) != (exp.actions != "") {
			t.Errorf("%s: action registered = %v", k.typ, r.Action != nil)
		}
		var ids []string
		for _, a := range r.Actions {
			if a.Scope != resourcev2.ActionScopeInstance {
				t.Errorf("%s %s: scope %q", k.typ, a.ID, a.Scope)
			}
			if a.ParamsSchema == nil {
				t.Errorf("%s %s: no params schema", k.typ, a.ID)
			}
			if a.Dangerous != slices.Contains(exp.dangerous, a.ID) {
				t.Errorf("%s %s: dangerous %v", k.typ, a.ID, a.Dangerous)
			}
			ids = append(ids, a.ID)
		}
		if got := strings.Join(ids, ","); got != exp.actions {
			t.Errorf("%s: actions %s, want %s", k.typ, got, exp.actions)
		}
	}
}

func containsOp(ops []resourcev2.Operation, op resourcev2.Operation) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

func TestLifecycleActionPostsAndWaitsForTask(t *testing.T) {
	f, client := newFakePVE(t)

	res, err := act(vms, client, "start", "qemu/100")
	if err != nil {
		t.Fatal(err)
	}
	if res["message"] != "Started web01 (qemu/100)." || res["upid"] != "UPID:pve2:00001:task" {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(f.writes) != 1 || f.writes[0].method != http.MethodPost || f.writes[0].path != "/nodes/pve2/qemu/100/status/start" {
		t.Fatalf("unexpected writes %+v", f.writes)
	}

	f.exitStatus = "can't lock file '/var/lock/qemu-server/lock-100.conf' - got timeout"
	_, err = act(vms, client, "stop", "qemu/100")
	opErr := opError(t, vms, err)
	if opErr.Code != "PROVIDER_ERROR" || opErr.Title != "Task failed" || !strings.Contains(opErr.Message, "got timeout") {
		t.Fatalf("unexpected error %+v", opErr)
	}
	if f.writes[1].path != "/nodes/pve2/qemu/100/status/stop" {
		t.Fatalf("stop posted to %s", f.writes[1].path)
	}
}

func TestLifecycleActionStillRunningAfterBound(t *testing.T) {
	f, client := newFakePVE(t)
	f.taskStatus = "running"
	f.exitStatus = ""
	defer func(d time.Duration) { taskWaitBound = d }(taskWaitBound)
	taskWaitBound = 50 * time.Millisecond

	res, err := act(vms, client, "shutdown", "qemu/100")
	if err != nil {
		t.Fatal(err)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "still running") {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestActionRejectsOtherKindsAndUnknownActions(t *testing.T) {
	_, client := newFakePVE(t)

	_, err := act(cts, client, "start", "qemu/100")
	if opErr := opError(t, cts, err); opErr.Code != "NOT_FOUND" {
		t.Fatalf("container kind acted on a VM: %+v", opErr)
	}
	_, err = act(cts, client, "suspend", "lxc/200")
	if opErr := opError(t, cts, err); opErr.Code != "UNSUPPORTED" {
		t.Fatalf("container suspend: %+v", opErr)
	}
}

func TestConfigActionReturnsConfigDigestAndNode(t *testing.T) {
	_, client := newFakePVE(t)

	res, err := act(vms, client, "config", "qemu/100")
	if err != nil {
		t.Fatal(err)
	}
	config, _ := res["config"].(map[string]any)
	if res["digest"] != "d1" || res["node"] != "pve2" || config["cores"] != int64(2) {
		t.Fatalf("unexpected result %+v", res)
	}
	if _, ok := config["digest"]; ok {
		t.Fatal("digest left in config")
	}
}

func TestDeleteDestroysGuestAndWaits(t *testing.T) {
	f, client := newFakePVE(t)

	rec, err := mutate(vms, client, "delete", "qemu/100", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reference.Address.ID != "qemu/100" || rec.Body.Map["name"].String != "web01" {
		t.Fatalf("unexpected result %+v", rec)
	}
	w := f.writes[0]
	if w.method != http.MethodDelete || w.path != "/nodes/pve2/qemu/100" ||
		w.query.Get("purge") != "1" || w.query.Get("destroy-unreferenced-disks") != "1" {
		t.Fatalf("unexpected delete %+v", w)
	}
}

func TestUpdateEncodesChangesAndReturnsGuest(t *testing.T) {
	f, client := newFakePVE(t)
	f.writeBody = `{"data":null}`

	input := map[string]any{
		"cores": 4, "memory": 2048.5, "onboot": true, "protection": false, "hostname": "ct2",
		"description": "", "tags": nil, "digest": "d1",
	}
	rec, err := mutate(cts, client, "update", "lxc/200", input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reference.Address.ID != "lxc/200" || rec.Body.Map["id"].String != "lxc/200" {
		t.Fatalf("unexpected result %+v", rec)
	}
	if _, ok := rec.Body.Map[refsField]; !ok {
		t.Fatalf("refreshed guest lacks %s: %+v", refsField, rec.Body.Map)
	}
	w := f.writes[0]
	want := url.Values{
		"cores": {"4"}, "memory": {"2048.5"}, "onboot": {"1"}, "protection": {"0"}, "hostname": {"ct2"},
		"delete": {"description,tags"}, "digest": {"d1"},
	}
	if w.method != http.MethodPut || w.path != "/nodes/pve1/lxc/200/config" || w.form.Encode() != want.Encode() {
		t.Fatalf("unexpected update %s %s %v", w.method, w.path, w.form)
	}

	_, err = mutate(cts, client, "update", "lxc/200", map[string]any{"net0": map[string]any{"x": 1}})
	if opErr := opError(t, cts, err); opErr.Code != "INVALID_INPUT" {
		t.Fatalf("nested value: %+v", opErr)
	}
}

func TestWriteErrorsAreClassified(t *testing.T) {
	f, client := newFakePVE(t)

	f.writeCode, f.writeBody = http.StatusForbidden, `{"data":null}`
	_, err := act(vms, client, "reboot", "qemu/100")
	opErr := opError(t, vms, err)
	if opErr.Code != "FORBIDDEN" || !strings.Contains(opErr.Message, "VM.PowerMgmt") ||
		len(opErr.Suggestions) != 1 || !strings.Contains(opErr.Suggestions[0], "PVEVMAdmin") {
		t.Fatalf("reboot 403: %+v", opErr)
	}
	_, err = mutate(vms, client, "delete", "qemu/100", map[string]any{})
	if opErr := opError(t, vms, err); opErr.Code != "FORBIDDEN" || !strings.Contains(opErr.Message, "VM.Allocate") {
		t.Fatalf("delete 403: %+v", opErr)
	}
	_, err = mutate(vms, client, "update", "qemu/100", map[string]any{"cores": 2})
	if opErr := opError(t, vms, err); opErr.Code != "FORBIDDEN" || !strings.Contains(opErr.Message, "VM.Config.") {
		t.Fatalf("update 403: %+v", opErr)
	}

	f.writeCode = http.StatusBadRequest
	f.writeBody = `{"data":null,"errors":{"cores":"value must have a minimum value of 1","memory":"invalid format"}}`
	_, err = mutate(vms, client, "update", "qemu/100", map[string]any{"cores": 0})
	opErr = opError(t, vms, err)
	want := "Proxmox rejected the request (Bad Request)\ncores: value must have a minimum value of 1\nmemory: invalid format"
	if opErr.Code != "INVALID_INPUT" || opErr.Message != want {
		t.Fatalf("update 400: %+v", opErr)
	}

	// A read 403 keeps the read message.
	if opErr := opError(t, vms, &proxmox.APIError{StatusCode: http.StatusForbidden}); !strings.Contains(opErr.Message, "read") {
		t.Fatalf("read 403: %+v", opErr)
	}
}

func TestActionParamHelpers(t *testing.T) {
	in, err := resourcev2.ValueFromNative(map[string]any{
		"s": "x", "b": true, "i": int64(7), "u": uint64(8), "f": 9.0, "frac": 1.5, "l": []any{"a", "b"}, "bad": []any{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	params, err := actionParams(in)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := stringParam(params, "s", true); err != nil || s != "x" {
		t.Fatalf("string: %q %v", s, err)
	}
	if _, err := stringParam(params, "missing", true); err == nil {
		t.Fatal("missing required string accepted")
	}
	if _, err := stringParam(params, "b", false); err == nil {
		t.Fatal("boolean accepted as string")
	}
	if b, err := boolParam(params, "b"); err != nil || !b {
		t.Fatalf("bool: %v %v", b, err)
	}
	for key, want := range map[string]int64{"i": 7, "u": 8, "f": 9} {
		if n, ok, err := intParam(params, key); err != nil || !ok || n != want {
			t.Fatalf("int %s: %d %v %v", key, n, ok, err)
		}
	}
	if _, _, err := intParam(params, "frac"); err == nil {
		t.Fatal("fraction accepted as integer")
	}
	if _, ok, err := intParam(params, "missing"); ok || err != nil {
		t.Fatal("absent integer reported present")
	}
	if l, err := stringListParam(params, "l"); err != nil || strings.Join(l, ",") != "a,b" {
		t.Fatalf("list: %v %v", l, err)
	}
	if _, err := stringListParam(params, "bad"); err == nil {
		t.Fatal("non-string list accepted")
	}

	if p, err := actionParams(resourcev2.Value{}); err != nil || len(p) != 0 {
		t.Fatalf("absent input: %v %v", p, err)
	}
	str, _ := resourcev2.ValueFromNative("x")
	if _, err := actionParams(str); err == nil {
		t.Fatal("non-object input accepted")
	}
}
