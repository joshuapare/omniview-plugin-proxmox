package resource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// TestDeclaredEdgesResolveToListedResources reads every declared extractor
// field path from the enumerated bodies, as the host's graph indexer does,
// and checks each extracted ID is the ID of an enumerated resource of the
// target type.
func TestDeclaredEdgesResolveToListedResources(t *testing.T) {
	byType := map[string]string{
		"node": `[{"id":"node/pve1","type":"node","node":"pve1"},{"id":"node/pve2","type":"node","node":"pve2"}]`,
		"storage": `[
			{"id":"storage/pve1/local-lvm","type":"storage","storage":"local-lvm","node":"pve1"},
			{"id":"storage/pve1/nfs","type":"storage","storage":"nfs","node":"pve1","shared":1},
			{"id":"storage/pve2/nfs","type":"storage","storage":"nfs","node":"pve2","shared":1}]`,
		"vm": `[
			{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve1"},
			{"id":"qemu/101","type":"qemu","vmid":101,"node":"pve2"},
			{"id":"lxc/200","type":"lxc","vmid":200,"node":"pve1"}]`,
	}
	configs := map[string]string{
		"/api2/json/nodes/pve1/qemu/100/config": `{"scsi0":"local-lvm:vm-100-disk-0,size=32G","scsi1":"nfs:100/vm-100-disk-1.qcow2"}`,
		"/api2/json/nodes/pve2/qemu/101/config": `{"virtio0":"nfs:101/vm-101-disk-0.qcow2"}`,
		"/api2/json/nodes/pve1/lxc/200/config":  `{"rootfs":"local-lvm:subvol-200-disk-0,size=8G"}`,
		"/api2/json/cluster/tasks":              `[{"upid":"UPID:pve2:0001:0002:0003:qmstart:101:root@pam:","node":"pve2"}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := configs[r.URL.Path]
		if r.URL.Path == "/api2/json/cluster/resources" {
			body, ok = byType[r.URL.Query().Get("type")]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":` + body + `}`))
	}))
	defer srv.Close()
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}

	p := &provider{}
	objects := map[string]map[string]map[string]any{}
	for _, k := range staticKinds {
		page, err := k.enumerate(context.Background(), client, resourcev2.EnumerateRequest{
			Target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: k.typ, Representation: representationID},
		})
		if err != nil {
			t.Fatalf("enumerate %s: %v", k.typ, err)
		}
		objects[k.typ] = map[string]map[string]any{}
		for _, rec := range page.Records {
			native, err := resourcev2.NativeFromValue(rec.Body)
			if err != nil {
				t.Fatal(err)
			}
			objects[k.typ][rec.Reference.Address.ID] = native.(map[string]any)
		}
	}

	got := map[string][]string{}
	for _, k := range staticKinds {
		decls, err := p.GetRelationships(context.Background(), resourcev2.AccessRoute{Connection: "c"}, k.typ)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range decls {
			for id, obj := range objects[k.typ] {
				for _, target := range fieldPathValues(obj, decl.Extractor.FieldPath) {
					if _, ok := objects[decl.TargetType][target]; !ok {
						t.Errorf("%s %s: %s edge to %q, which is not a listed %s", k.typ, id, decl.Type, target, decl.TargetType)
					}
					got[id] = append(got[id], string(decl.Type)+" "+target)
				}
			}
		}
	}
	for _, edges := range got {
		sort.Strings(edges)
	}

	want := map[string][]string{
		"qemu/100": {"runs_on node/pve1", "uses storage/pve1/local-lvm", "uses storage/pve1/nfs"},
		// Shared storage resolves to the entry on the guest's node.
		"qemu/101":               {"runs_on node/pve2", "uses storage/pve2/nfs"},
		"lxc/200":                {"runs_on node/pve1", "uses storage/pve1/local-lvm"},
		"storage/pve1/local-lvm": {"runs_on node/pve1"},
		"storage/pve1/nfs":       {"runs_on node/pve1"},
		"storage/pve2/nfs":       {"runs_on node/pve2"},
		"UPID:pve2:0001:0002:0003:qmstart:101:root@pam:": {"runs_on node/pve2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges:\n got %v\nwant %v", got, want)
	}
}

// fieldPathValues reads a dotted path; an array yields each element.
func fieldPathValues(obj map[string]any, path string) []string {
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	switch v := cur.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			out = append(out, e.(string))
		}
		return out
	}
	return nil
}
