package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"time"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const pollInterval = 10 * time.Second

// source provides a best-effort snapshot followed by polling differences.
func (k *kind) source(ctx context.Context, client *proxmox.Client, request resourcev2.SourceRequest, emit resourcev2.SourceEmitter) error {
	p := pollSource{kind: k, client: client, target: request.Target, binding: request.Representation, emit: emit, maxBatchItems: int(request.Limits.MaxBatchItems)}
	if err := p.poll(ctx, true); err != nil {
		return err
	}
	if p.terminal {
		return nil
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.poll(ctx, false); err != nil {
				return err
			}
			if p.terminal {
				return nil
			}
		}
	}
}

type seenEntry struct {
	raw       json.RawMessage
	reference resourcev2.ResourceReference
}

type pollSource struct {
	kind          *kind
	client        *proxmox.Client
	target        resourcev2.ReadTarget
	binding       resourcev2.Representation
	emit          resourcev2.SourceEmitter
	seen          map[string]seenEntry
	maxBatchItems int
	terminal      bool
	// Counts of the poll in progress, for its span.
	pollEntries, pollChanges int
}

// terminate ends the source: the host gets reset and the poll loop stops.
func (p *pollSource) terminate(ctx context.Context, reason, reasonText, cause string, causeErr error) error {
	trace.SpanFromContext(ctx).AddEvent("source.terminal", trace.WithAttributes(attribute.String("reason", cause)))
	fields := []any{"kind", p.kind.typ, "reason", cause}
	if causeErr != nil {
		fields = append(fields, resClassFields(causeErr)...)
	}
	resLog().Warnw(ctx, "source terminated", fields...)
	if err := p.emit.Reset(ctx, reason, reasonText); err != nil {
		return err
	}
	p.terminal = true
	return nil
}

// poll is one list-and-diff of the source, as a span.
func (p *pollSource) poll(ctx context.Context, initial bool) error {
	ctx, span := proxmox.Tracer().Start(ctx, "proxmox.source.poll")
	span.SetAttributes(attribute.String("proxmox.kind", p.kind.typ), attribute.Bool("initial", initial))
	p.pollEntries, p.pollChanges = 0, 0
	start := time.Now()
	err := p.pollOnce(ctx, initial)
	span.SetAttributes(attribute.Int("entries", p.pollEntries), attribute.Int("changes", p.pollChanges))
	if p.terminal {
		span.SetAttributes(attribute.Bool("terminal", true))
	}
	p.kind.resEndSpan(span, err)
	outcome, _, _ := proxmox.ClassifyError(err)
	switch {
	case outcome == "success" && p.terminal:
		outcome = "terminated"
	case outcome == "success":
		outcome = "ok"
	case outcome == "failure":
		outcome = "failed"
	}
	proxmox.SourcePoll(outcome)
	resLog().Debugw(ctx, "source poll", "kind", p.kind.typ, "initial", initial, "entries", p.pollEntries, "changes", p.pollChanges,
		"outcome", outcome, "duration_ms", time.Since(start).Milliseconds())
	return err
}

func (p *pollSource) pollOnce(ctx context.Context, initial bool) error {
	entries, err := p.kind.fetch(ctx, p.client)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if proxmox.IsStatus(err, http.StatusUnauthorized) || proxmox.IsStatus(err, http.StatusForbidden) {
			resLog().Warnw(ctx, "source poll rejected", append([]any{"kind", p.kind.typ}, resClassFields(err)...)...)
			return p.kind.ClassifyError(err)
		}
		return p.terminate(ctx, resourcev2.SourceResetSourceInvalidated, "Unable to list Proxmox resources.", "list_failed", err)
	}
	p.pollEntries = len(entries)
	current := make(map[string]seenEntry, len(entries))
	observations := make([]resourcev2.SourceObservation, 0, len(entries))
	var changes []resourcev2.SourceChange
	for _, e := range entries {
		prev, known := p.seen[e.ID]
		if known && bytes.Equal(prev.raw, e.Raw) {
			current[e.ID] = prev
			continue
		}
		rec, err := record(reference(p.target.Provider, p.target.Authority, p.target.Type, e.ID), e)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(rec.Representation, p.binding) {
			return p.terminate(ctx, resourcev2.SourceResetBindingChanged, "Source binding changed.", "binding_changed", nil)
		}
		current[e.ID] = seenEntry{raw: e.Raw, reference: rec.Reference}
		observation := resourcev2.SourceObservation{Record: resourcev2.CompactSourceRecord(rec)}
		if initial {
			observations = append(observations, observation)
		} else {
			changes = append(changes, resourcev2.SourceChange{Kind: resourcev2.SourceChangeUpsert, Observation: &observation})
		}
	}
	if initial {
		p.pollChanges = len(observations)
		if err := p.emit.Snapshot(ctx, observations); err != nil {
			return err
		}
		coverage := resourcev2.SourceCoverage{Enumeration: resourcev2.EnumerationExhausted, Consistency: resourcev2.ConsistencyBestEffort, Limitations: []string{"10s polling diff; intermediate states between polls are not observed"}}
		if err := p.emit.EndSnapshot(ctx, coverage, resourcev2.ContinuityBestEffort); err != nil {
			return err
		}
	} else {
		for id, prev := range p.seen {
			if _, ok := current[id]; !ok {
				ref := prev.reference
				changes = append(changes, resourcev2.SourceChange{Kind: resourcev2.SourceChangeRemove, Reference: &ref})
			}
		}
		p.pollChanges = len(changes)
		for len(changes) > 0 {
			n := min(len(changes), p.maxBatchItems)
			if err := p.emitChanges(ctx, changes[:n]); err != nil {
				return err
			}
			changes = changes[n:]
		}
	}
	p.seen = current
	return nil
}

// emitChanges splits batches rejected by the SDK's resource limits. A single
// change that exceeds a limit remains an error.
func (p *pollSource) emitChanges(ctx context.Context, changes []resourcev2.SourceChange) error {
	err := p.emit.Changes(ctx, changes, nil)
	if err == nil || len(changes) == 1 || resourcev2.ErrorCode(err) != "RESOURCE_LIMIT" {
		return err
	}
	trace.SpanFromContext(ctx).AddEvent("batch.split", trace.WithAttributes(attribute.Int("size", len(changes))))
	resLog().Debugw(ctx, "source change batch split", "kind", p.kind.typ, "size", len(changes))
	mid := len(changes) / 2
	if err := p.emitChanges(ctx, changes[:mid]); err != nil {
		return err
	}
	return p.emitChanges(ctx, changes[mid:])
}
