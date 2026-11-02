package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const pollInterval = 10 * time.Second

// source provides a best-effort snapshot followed by polling differences.
func (k *kind) source(ctx context.Context, client *proxmox.Client, request resourcev2.SourceRequest, emit resourcev2.SourceEmitter) error {
	p := pollSource{kind: k, client: client, target: request.Target, emit: emit, maxBatchItems: int(request.Limits.MaxBatchItems)}
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
	raw    json.RawMessage
	record resourcev2.Record
}

type pollSource struct {
	kind          *kind
	client        *proxmox.Client
	target        resourcev2.ReadTarget
	emit          resourcev2.SourceEmitter
	seen          map[string]seenEntry
	maxBatchItems int
	terminal      bool
}

func (p *pollSource) poll(ctx context.Context, initial bool) error {
	entries, err := p.kind.list(ctx, p.client)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if proxmox.IsStatus(err, http.StatusUnauthorized) || proxmox.IsStatus(err, http.StatusForbidden) {
			return p.kind.ClassifyError(err)
		}
		if err := p.emit.Reset(ctx, resourcev2.SourceResetSourceInvalidated, "Unable to list Proxmox resources."); err != nil {
			return err
		}
		p.terminal = true
		return nil
	}
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
		current[e.ID] = seenEntry{raw: e.Raw, record: rec}
		observation := resourcev2.SourceObservation{Record: rec}
		if initial {
			observations = append(observations, observation)
		} else {
			changes = append(changes, resourcev2.SourceChange{Kind: resourcev2.SourceChangeUpsert, Observation: &observation})
		}
	}
	if initial {
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
				ref := prev.record.Reference
				changes = append(changes, resourcev2.SourceChange{Kind: resourcev2.SourceChangeRemove, Reference: &ref})
			}
		}
		for len(changes) > 0 {
			n := min(len(changes), p.maxBatchItems)
			if err := p.emit.Changes(ctx, changes[:n], nil); err != nil {
				return err
			}
			changes = changes[n:]
		}
	}
	p.seen = current
	return nil
}
