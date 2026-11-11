package resource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

func TestLoadConnectionsKeepsSecretOutOfConnectionData(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "clusters.yaml")
	body := `
clusters:
  - id: lab
    url: https://pve.lan:8006
    token_id: root@pam!omni
    token_secret: s3cret
    console_user: omniview@pve
    console_password: c0nsole-pw
`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	conns, err := loadConnections(context.Background(), []string{filepath.Join(dir, "missing.yaml"), file})
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 || conns[0].ID != "lab" || conns[0].Name != "lab" {
		t.Fatalf("unexpected connections: %+v", conns)
	}
	for k, v := range conns[0].Data {
		if s, _ := v.(string); strings.Contains(s, "s3cret") || strings.Contains(s, "c0nsole-pw") {
			t.Fatalf("secret exposed in Connection.Data[%q]", k)
		}
	}

	if _, err := loadConnections(context.Background(), []string{file, file}); err == nil {
		t.Fatal("expected duplicate cluster id across files to be rejected")
	}
}

func TestGuestKindsAreExactPerType(t *testing.T) {
	_, client := newFakePVE(t)

	page, err := vms.enumerate(ctx, client, resourcev2.EnumerateRequest{
		Target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: typeVM, Representation: representationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Reference.Address.ID != "qemu/100" {
		t.Fatalf("VM page should contain only qemu guests, got %+v", page.Records)
	}

	get := func(id string) error {
		_, err := vms.get(ctx, client, resourcev2.GetRequest{Reference: ref(typeVM, id), Representation: representationID})
		return err
	}
	if err := get("qemu/100"); err != nil {
		t.Fatalf("get qemu/100: %v", err)
	}
	var opErr *resourcev2.ResourceOperationError
	if err := get("lxc/200"); !errors.As(err, &opErr) || opErr.Code != "NOT_FOUND" {
		t.Fatalf("VM Get must not resolve a container ID, got %v", err)
	}
}

func TestCephKindOnClusterWithoutCephIsCephUnavailable(t *testing.T) {
	srv := httptest.NewServer(noCephPVE(nil))
	defer srv.Close()
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	pools := cephKind(typeCephPool, "Pools", "Ceph pool", (*proxmox.Client).CephPools)

	_, err = pools.list(context.Background(), client)
	var opErr *resourcev2.ResourceOperationError
	if !errors.As(pools.ClassifyError(err), &opErr) || opErr.Code != "UNAVAILABLE" || opErr.Title != "Ceph unavailable" {
		t.Fatalf("want Ceph unavailable, got %v", pools.ClassifyError(err))
	}
}

// noCephPVE serves one online node, its guests and no Ceph; extra, when
// set, answers first.
func noCephPVE(extra http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if extra != nil {
			rec := httptest.NewRecorder()
			extra(rec, r)
			if rec.Code != http.StatusNotFound {
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
				return
			}
		}
		switch {
		case r.URL.Path == "/api2/json/version":
			_, _ = w.Write([]byte(`{"data":{"version":"8.2","release":"8.2","repoid":"x"}}`))
		case r.URL.Path == "/api2/json/cluster/resources" && r.URL.Query().Get("type") == "vm":
			_, _ = w.Write([]byte(`{"data":[` +
				`{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve1"},` +
				`{"id":"qemu/101","type":"qemu","vmid":101,"node":"pve1"},` +
				`{"id":"qemu/102","type":"qemu","vmid":102,"node":"pve1","maxmem":18446744073709551615}]}`))
		case r.URL.Path == "/api2/json/cluster/resources":
			_, _ = w.Write([]byte(`{"data":[{"id":"node/pve1","type":"node","node":"pve1","status":"online"}]}`))
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"data":{}}`))
		default:
			http.Error(w, "binary not installed: /usr/bin/ceph-mon", http.StatusInternalServerError)
		}
	}
}

// startedProvider runs the provider against a fake cluster: one connection,
// "lab", loaded and started.
func startedProvider(t *testing.T, handler http.HandlerFunc) (*provider, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "clusters.yaml")
	config := "clusters:\n  - id: lab\n    url: " + srv.URL + "\n    token_id: root@pam!t\n    token_secret: s\n"
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := newProvider(func() ([]string, error) { return []string{file}, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.LoadConnections(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = p.StartConnection(ctx, "lab")
	return p, err
}

var lab = resourcev2.AccessRoute{Connection: "lab"}

func catalogTypes(t *testing.T, p *provider) (resourcev2.Catalog, map[string]bool) {
	t.Helper()
	catalog, err := p.Catalog(ctx, resourcev2.CatalogRequest{Route: lab})
	if err != nil {
		t.Fatal(err)
	}
	if err := resourcev2.ValidateCatalog(catalog); err != nil {
		t.Fatalf("invalid catalog: %v", err)
	}
	types := map[string]bool{}
	for _, typ := range catalog.Types {
		types[typ.Type] = true
	}
	return catalog, types
}

// The catalog offers the Ceph types only on a cluster that answered `ceph
// status` when its connection started.
func TestCatalogAddsCephTypesOnlyWhereCephAnswers(t *testing.T) {
	for _, hasCeph := range []bool{false, true} {
		cephStatus := func(w http.ResponseWriter, r *http.Request) {
			if hasCeph && r.URL.Path == "/api2/json/nodes/pve1/ceph/status" {
				_, _ = w.Write([]byte(`{"data":{"fsid":"f1","health":{"status":"HEALTH_OK"}}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
		p, err := startedProvider(t, noCephPVE(cephStatus))
		if err != nil {
			t.Fatal(err)
		}
		catalog, types := catalogTypes(t, p)
		for _, k := range staticKinds {
			if !types[k.typ] {
				t.Errorf("ceph=%v: static type %s missing", hasCeph, k.typ)
			}
		}
		for _, k := range cephKinds {
			if types[k.typ] != hasCeph {
				t.Errorf("ceph=%v: Ceph type %s offered=%v", hasCeph, k.typ, types[k.typ])
			}
		}
		for _, descriptor := range catalog.Types {
			representation := resourcev2.Representation{ID: "native", Revision: "1"}
			if len(descriptor.Representations) != 1 || descriptor.Representations[0] != representation {
				t.Fatalf("%s representations: %+v", descriptor.Type, descriptor.Representations)
			}
			binding, err := p.ResolveSchema(ctx, resourcev2.SchemaRequest{
				Route: lab, Type: descriptor.Type,
				Target: resourcev2.SchemaTarget{Representation: &representation},
			})
			if err != nil || binding.State != "absent" {
				t.Fatalf("ceph=%v %s native schema: %+v, %v", hasCeph, descriptor.Type, binding, err)
			}
		}
	}
}

func TestNativeSchemaResolutionRequiresSelectedBinding(t *testing.T) {
	p, err := startedProvider(t, noCephPVE(nil))
	if err != nil {
		t.Fatal(err)
	}
	features, err := p.Features(ctx)
	if err != nil || !features.Has(resourcev2.FeatureSchemaResolver) || features.Has(resourcev2.FeatureSchemaDocuments) {
		t.Fatalf("native schema features: %+v, %v", features, err)
	}
	target := func(id, revision string) resourcev2.SchemaTarget {
		return resourcev2.SchemaTarget{Representation: &resourcev2.Representation{ID: id, Revision: revision}}
	}
	for _, test := range []struct {
		name   string
		route  resourcev2.AccessRoute
		typ    string
		target resourcev2.SchemaTarget
		code   string
	}{
		{name: "selected", route: lab, typ: typeVM, target: target("native", "1")},
		{name: "different format", route: lab, typ: typeVM, target: target("other", "1"), code: "RESET_REQUIRED"},
		{name: "stale revision", route: lab, typ: typeVM, target: target("native", "old"), code: "RESET_REQUIRED"},
		{name: "missing revision", route: lab, typ: typeVM, target: target("native", ""), code: "INVALID_REQUEST"},
		{name: "missing target", route: lab, typ: typeVM, code: "INVALID_REQUEST"},
		{name: "unknown type", route: lab, typ: "unknown", target: target("native", "1"), code: "NOT_FOUND"},
		{name: "unavailable Ceph type", route: lab, typ: typeCephPool, target: target("native", "1"), code: "NOT_FOUND"},
		{name: "unstarted route", route: resourcev2.AccessRoute{Connection: "other"}, typ: typeVM, target: target("native", "1"), code: "INVALID_REQUEST"},
		{name: "missing route", typ: typeVM, target: target("native", "1"), code: "INVALID_REQUEST"},
		{name: "unsupported auxiliary", route: lab, typ: typeVM, code: "UNSUPPORTED", target: resourcev2.SchemaTarget{
			Auxiliary: &resourcev2.AuxiliarySchemaTarget{Key: "config", Reference: reference(providerID, "test-authority", typeVM, "qemu/100")},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding, err := p.ResolveSchema(ctx, resourcev2.SchemaRequest{Route: test.route, Type: test.typ, Target: test.target})
			if test.code == "" {
				if err != nil || binding.State != "absent" {
					t.Fatalf("native schema: %+v, %v", binding, err)
				}
			} else if resourcev2.ErrorCode(err) != test.code || binding.State != "" {
				t.Fatalf("schema: %+v, %v; want %s without a binding", binding, err, test.code)
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	binding, err := p.ResolveSchema(canceled, resourcev2.SchemaRequest{Route: lab, Type: typeVM, Target: target("native", "1")})
	if !errors.Is(err, context.Canceled) || binding.State != "" {
		t.Fatalf("canceled schema: %+v, %v", binding, err)
	}
}

func TestStartRejectedTokenIsUnauthorized(t *testing.T) {
	_, err := startedProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "authentication failure", http.StatusUnauthorized)
	})
	var opErr *resourcev2.ResourceOperationError
	if !errors.As(err, &opErr) || opErr.Code != "UNAUTHORIZED" {
		t.Fatalf("start with a rejected token: %v", err)
	}
}

// Enumeration through the controller pages by cursor and passes the SDK's
// page validation, with integers beyond float64 precision kept exact.
func TestEnumeratePagesThroughController(t *testing.T) {
	p, err := startedProvider(t, noCephPVE(nil))
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := catalogTypes(t, p)
	pinned := resourcev2.WithCatalogRevisionForRoute(ctx, providerID, lab, catalog.Revision)
	target := resourcev2.ReadTarget{
		Route: lab, Provider: providerID, Authority: "test-authority", Type: typeVM,
		Scope: resourcev2.QueryScope{}, Representation: representationID,
	}

	var ids []string
	var cursor *string
	for range 5 {
		page, err := p.Enumerate(pinned, resourcev2.EnumerateRequest{Target: target, PageSize: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if page.Count.Value == nil || *page.Count.Value != "3" {
			t.Fatalf("count %+v", page.Count)
		}
		for _, rec := range page.Records {
			ids = append(ids, rec.Reference.Address.ID)
			if rec.Reference.Address.ID == "qemu/102" && rec.Body.Map["maxmem"].Uint != "18446744073709551615" {
				t.Fatalf("maxmem lost precision: %+v", rec.Body.Map["maxmem"])
			}
			if rec.Representation != (resourcev2.Representation{ID: "native", Revision: "1"}) {
				t.Fatalf("record representation: %+v", rec.Representation)
			}
		}
		if cursor = page.NextCursor; cursor == nil {
			break
		}
	}
	if strings.Join(ids, ",") != "qemu/100,qemu/101,qemu/102" {
		t.Fatalf("paged IDs %v", ids)
	}

	rec, err := p.Get(pinned, resourcev2.GetRequest{Route: lab, Reference: reference(providerID, "test-authority", typeVM, "qemu/101"), Representation: representationID})
	if err != nil || rec.Body.Map["vmid"].Int != "101" {
		t.Fatalf("get qemu/101: %+v, %v", rec, err)
	}
	if rec.Representation != (resourcev2.Representation{ID: "native", Revision: "1"}) {
		t.Fatalf("get representation: %+v", rec.Representation)
	}
}

// A task Get reads the task's status on its node, so tasks too old for the
// cluster list are found too.
func TestTaskGetFallsBackToStatus(t *testing.T) {
	const upid = "UPID:pve1:0001:0002:0003:vzdump::root@pam:"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/tasks/" + upid + "/status":
			_, _ = w.Write([]byte(`{"data":{"upid":"` + upid + `","node":"pve1","status":"stopped","exitstatus":"OK"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"data":null}`))
		}
	}))
	defer srv.Close()
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	tasks := taskKind()
	get := func(id string) (resourcev2.Record, error) {
		return tasks.get(ctx, client, resourcev2.GetRequest{Reference: ref(typeTask, id), Representation: representationID})
	}

	rec, err := get(upid)
	if err != nil {
		t.Fatal(err)
	}
	refs := rec.Body.Map[refsField]
	if rec.Body.Map["exitstatus"].String != "OK" || refs.Map["node"].String != "node/pve1" {
		t.Fatalf("unexpected record %+v", rec.Body.Map)
	}
	if strings.Join(paths, ",") != "/api2/json/nodes/pve1/tasks/"+upid+"/status" {
		t.Fatalf("read %v", paths)
	}

	for _, id := range []string{"UPID:pve1:9999:x:y:z::root@pam:", "not-a-upid", "UPID:"} {
		var opErr *resourcev2.ResourceOperationError
		if _, err := get(id); !errors.As(err, &opErr) || opErr.Code != "NOT_FOUND" {
			t.Fatalf("%s: %v", id, err)
		}
	}
}
