package resource

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
)

type recordingSource struct {
	observations []resourcev2.SourceObservation
	coverage     resourcev2.SourceCoverage
	continuity   string
	changes      [][]resourcev2.SourceChange
	reset        string
}

func (s *recordingSource) Snapshot(_ context.Context, observations []resourcev2.SourceObservation) error {
	s.observations = observations
	return nil
}
func (s *recordingSource) EndSnapshot(_ context.Context, coverage resourcev2.SourceCoverage, continuity string) error {
	s.coverage = coverage
	s.continuity = continuity
	return nil
}
func (s *recordingSource) Changes(_ context.Context, changes []resourcev2.SourceChange, _ *resourcev2.SourceCoverage) error {
	s.changes = append(s.changes, changes)
	return nil
}
func (s *recordingSource) Reset(_ context.Context, reason, _ string) error {
	s.reset = reason
	return nil
}

func TestPollSourceSnapshotAndDiff(t *testing.T) {
	entry := func(id, body string) proxmox.Entry { return proxmox.Entry{ID: id, Raw: []byte(body)} }
	responses := [][]proxmox.Entry{
		{entry("a", `{"status":"running"}`), entry("b", `{"status":"stopped"}`)},
		{entry("a", `{"status":"running"}`), entry("b", `{"status":"stopped"}`)},
		{entry("a", `{"status":"stopped"}`), entry("c", `{"status":"running"}`)},
	}
	calls := 0
	k := &kind{list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) {
		result := responses[calls]
		calls++
		return result, nil
	}}
	sink := &recordingSource{}
	p := pollSource{kind: k, target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: typeVM}, emit: sink, maxBatchItems: 2}
	ctx := context.Background()
	if err := p.poll(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(sink.observations) != 2 || sink.observations[0].Record.Reference.Address.ID != "a" || sink.observations[1].Record.Reference.Address.ID != "b" {
		t.Fatalf("snapshot: %+v", sink.observations)
	}
	wantCoverage := resourcev2.SourceCoverage{Enumeration: resourcev2.EnumerationExhausted, Consistency: resourcev2.ConsistencyBestEffort, Limitations: []string{"10s polling diff; intermediate states between polls are not observed"}}
	if !reflect.DeepEqual(sink.coverage, wantCoverage) || sink.continuity != resourcev2.ContinuityBestEffort {
		t.Fatalf("boundary: %+v %s", sink.coverage, sink.continuity)
	}
	if err := p.poll(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 0 {
		t.Fatalf("unchanged poll emitted changes: %+v", sink.changes)
	}
	if err := p.poll(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 2 || len(sink.changes[0]) != 2 || len(sink.changes[1]) != 1 {
		t.Fatalf("changes: %+v", sink.changes)
	}
	got := map[string]string{}
	for _, change := range append(sink.changes[0], sink.changes[1]...) {
		if change.Kind == resourcev2.SourceChangeUpsert {
			got[change.Observation.Record.Reference.Address.ID] = change.Kind
		} else {
			got[change.Reference.Address.ID] = change.Kind
		}
	}
	want := map[string]string{"a": resourcev2.SourceChangeUpsert, "c": resourcev2.SourceChangeUpsert, "b": resourcev2.SourceChangeRemove}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes: %v, want %v", got, want)
	}
}

func TestPollSourceErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		code  string
		reset bool
	}{
		{"unauthorized", &proxmox.APIError{StatusCode: http.StatusUnauthorized}, "UNAUTHORIZED", false},
		{"forbidden", &proxmox.APIError{StatusCode: http.StatusForbidden}, "FORBIDDEN", false},
		{"transient", errors.New("secret upstream detail"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &kind{list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) { return nil, tc.err }}
			sink := &recordingSource{}
			p := pollSource{kind: k, emit: sink}
			err := p.poll(context.Background(), false)
			if tc.code != "" {
				var opErr *resourcev2.ResourceOperationError
				if !errors.As(err, &opErr) || opErr.Code != tc.code {
					t.Fatalf("error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if (sink.reset == resourcev2.SourceResetSourceInvalidated) != tc.reset || p.terminal != tc.reset {
				t.Fatalf("reset: %q terminal: %v", sink.reset, p.terminal)
			}
		})
	}
}
