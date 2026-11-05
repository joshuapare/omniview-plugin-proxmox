package resource

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
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

type boundedChangesSource struct {
	recordingSource
	maxBytes int
}

func (s *boundedChangesSource) Changes(ctx context.Context, changes []resourcev2.SourceChange, coverage *resourcev2.SourceCoverage) error {
	size, err := resourcev2.SourceChangesWireSize(resourcev2.SourceChanges{Changes: changes, Coverage: coverage})
	if err != nil {
		return err
	}
	if size > s.maxBytes {
		return resourcev2.NewError("RESOURCE_LIMIT", "source changes exceed frame limit")
	}
	return s.recordingSource.Changes(ctx, changes, coverage)
}

func TestPollSourceSplitsOversizedChangeFrames(t *testing.T) {
	payload := strings.Repeat("x", 40000)
	calls := 0
	k := &kind{list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return []proxmox.Entry{
			{ID: "a", Raw: []byte(`{"description":"` + payload + `"}`)},
			{ID: "b", Raw: []byte(`{"description":"` + payload + `"}`)},
		}, nil
	}}
	sink := &boundedChangesSource{maxBytes: 65500}
	p := pollSource{kind: k, target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: typeVM}, binding: nativeRepresentation(), emit: sink, maxBatchItems: 2}
	if err := p.poll(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := p.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 2 || len(sink.changes[0]) != 1 || len(sink.changes[1]) != 1 {
		t.Fatalf("changes: %+v", sink.changes)
	}
}

func TestEmitChangesPreservesSingleRecordLimit(t *testing.T) {
	sink := &boundedChangesSource{maxBytes: 1}
	p := pollSource{emit: sink}
	ref := reference(providerID, "a", typeVM, "vm")
	change := resourcev2.SourceChange{Kind: resourcev2.SourceChangeRemove, Reference: &ref}
	err := p.emitChanges(context.Background(), []resourcev2.SourceChange{change})
	if resourcev2.ErrorCode(err) != "RESOURCE_LIMIT" || len(sink.changes) != 0 {
		t.Fatalf("error=%v, changes=%v", err, sink.changes)
	}
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
	p := pollSource{kind: k, target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: typeVM}, binding: nativeRepresentation(), emit: sink, maxBatchItems: 2}
	ctx := context.Background()
	if err := p.poll(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(sink.observations) != 2 || sink.observations[0].Record.Reference.Address.ID != "a" || sink.observations[1].Record.Reference.Address.ID != "b" {
		t.Fatalf("snapshot: %+v", sink.observations)
	}
	if got, want := sink.observations[0].Record.Representation, resourcev2.CompactSourceRecord(resourcev2.Record{Representation: nativeRepresentation()}).Representation; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot representation: %+v, want %+v", got, want)
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

func TestPollSourceBindingChanged(t *testing.T) {
	k := &kind{list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) {
		return []proxmox.Entry{{ID: "a", Raw: []byte(`{"status":"running"}`)}}, nil
	}}
	binding := nativeRepresentation()
	binding.Revision = "different"
	sink := &recordingSource{}
	p := pollSource{kind: k, target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: typeVM}, binding: binding, emit: sink, maxBatchItems: 2}
	if err := p.poll(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if sink.reset != resourcev2.SourceResetBindingChanged || !p.terminal || len(sink.observations) != 0 {
		t.Fatalf("reset=%q terminal=%v observations=%+v", sink.reset, p.terminal, sink.observations)
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

func TestSourceSnapshotAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k := &kind{list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) {
		cancel()
		return []proxmox.Entry{{ID: "vm-1", Raw: []byte(`{"status":"running"}`)}}, nil
	}}
	sink := &recordingSource{}
	request := resourcev2.SourceRequest{Target: resourcev2.ReadTarget{Provider: providerID, Authority: "a", Type: typeVM}, Representation: nativeRepresentation()}
	err := k.source(ctx, nil, request, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("source error = %v, want context canceled", err)
	}
	if len(sink.observations) != 1 || sink.observations[0].Record.Reference.Address.ID != "vm-1" {
		t.Fatalf("snapshot = %+v", sink.observations)
	}
	if sink.coverage.Enumeration != resourcev2.EnumerationExhausted || sink.continuity != resourcev2.ContinuityBestEffort {
		t.Fatalf("snapshot boundary = %+v, %q", sink.coverage, sink.continuity)
	}
}

func TestSourceInitialFailureResetsAndStops(t *testing.T) {
	calls := 0
	k := &kind{list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) {
		calls++
		return nil, errors.New("upstream unavailable")
	}}
	sink := &recordingSource{}
	if err := k.source(context.Background(), nil, resourcev2.SourceRequest{}, sink); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || sink.reset != resourcev2.SourceResetSourceInvalidated {
		t.Fatalf("calls = %d, reset = %q", calls, sink.reset)
	}
}
