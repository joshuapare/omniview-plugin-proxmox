package resource

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	logging "github.com/omniviewdev/plugin-sdk/log"
	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// resLog is the process-wide plugin logger; the SDK installs it in NewPlugin.
func resLog() logging.Logger { return logging.Default() }

// resClassFields are the log fields of a classified error. They never carry
// the error text: Proxmox error messages come from the remote API.
func resClassFields(err error) []any {
	outcome, class, status := proxmox.ClassifyError(err)
	fields := []any{"outcome", outcome}
	if class != "" {
		fields = append(fields, "error_class", class)
	}
	if status > 0 {
		fields = append(fields, "status_code", status)
	}
	if code := resourcev2.ErrorCode(err); code != "" {
		fields = append(fields, "error_code", code)
	}
	return fields
}

// resEndSpan ends span with the Proxmox classification plus the SDK
// resource error code and, for guest writes, the privilege they needed.
func (k *kind) resEndSpan(span trace.Span, err error) {
	if err != nil {
		code := resourcev2.ErrorCode(err)
		if code == "" {
			code = resourcev2.ErrorCode(k.ClassifyError(err))
		}
		if code != "" {
			span.SetAttributes(attribute.String("error_code", code))
		}
		var write *writeError
		if errors.As(err, &write) {
			span.SetAttributes(attribute.String("privilege", write.privilege))
		}
	}
	proxmox.EndSpan(span, err)
}

// resVMID reads the VMID out of a guest, snapshot or create ID
// ("qemu/100", "lxc/101/snap").
func resVMID(id string) (int, bool) {
	parts := strings.Split(id, "/")
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// resUPIDType is the task type segment of a UPID, or "other".
func resUPIDType(upid string) string {
	parts := strings.Split(upid, ":")
	if len(parts) < 6 || parts[5] == "" || len(parts[5]) > 32 {
		return "other"
	}
	for _, r := range parts[5] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "other"
		}
	}
	return parts[5]
}

type (
	resActionFunc func(context.Context, *proxmox.Client, resourcev2.ActionRequest) (resourcev2.Value, error)
	resMutateFunc func(context.Context, *proxmox.Client, resourcev2.MutationRequest) (resourcev2.Record, error)
)

// resActionOp names an action in span names from a closed set; ids a caller
// invents map to "unknown" so span names stay bounded.
func (k *kind) resActionOp(action string) string {
	switch action {
	case actionConfig, actionMigrate, actionClone, "pending", actionRollback:
		return action
	case actionGuestExec:
		return "guest_exec"
	}
	for _, a := range k.actions() {
		if a.descriptor.ID == action {
			return action
		}
	}
	return "unknown"
}

// resMutationOp names a mutation operation in span names.
func resMutationOp(operation string) string {
	switch operation {
	case "create", "update", "delete":
		return operation
	}
	return "unknown"
}

// startWrite opens the span of one guest write (action or mutation).
func (k *kind) startWrite(ctx context.Context, op, id string) (context.Context, trace.Span) {
	ctx, span := proxmox.Tracer().Start(ctx, "proxmox.action."+op)
	span.SetAttributes(attribute.String("proxmox.kind", k.typ))
	if vmid, ok := resVMID(id); ok {
		span.SetAttributes(attribute.Int("proxmox.vmid", vmid))
	}
	return ctx, span
}

// finishWrite ends the write span and logs its outcome: Info on success,
// Error for a classified failure, Debug for a cancellation.
func (k *kind) finishWrite(ctx context.Context, span trace.Span, op string, start time.Time, err error) {
	k.resEndSpan(span, err)
	fields := append([]any{"kind", k.typ, "operation", op, "duration_ms", time.Since(start).Milliseconds()}, resClassFields(err)...)
	switch outcome, _, _ := proxmox.ClassifyError(err); outcome {
	case "success":
		resLog().Infow(ctx, "proxmox write finished", fields...)
	case "canceled":
		resLog().Debugw(ctx, "proxmox write canceled", fields...)
	default:
		resLog().Errorw(ctx, "proxmox write failed", fields...)
	}
}

func (k *kind) observeAction(fn resActionFunc) resActionFunc {
	return func(ctx context.Context, client *proxmox.Client, request resourcev2.ActionRequest) (resourcev2.Value, error) {
		op := k.resActionOp(request.Action)
		start := time.Now()
		ctx, span := k.startWrite(ctx, op, request.Reference.Address.ID)
		value, err := fn(ctx, client, request)
		k.finishWrite(ctx, span, op, start, err)
		return value, err
	}
}

func (k *kind) observeMutate(fn resMutateFunc) resMutateFunc {
	return func(ctx context.Context, client *proxmox.Client, request resourcev2.MutationRequest) (resourcev2.Record, error) {
		op := resMutationOp(request.Operation)
		start := time.Now()
		ctx, span := k.startWrite(ctx, op, request.Reference.Address.ID)
		record, err := fn(ctx, client, request)
		k.finishWrite(ctx, span, op, start, err)
		return record, err
	}
}

// fetch is list with a span: one per list of a kind, whether for a Get, an
// Enumerate or a source poll.
func (k *kind) fetch(ctx context.Context, client *proxmox.Client) ([]proxmox.Entry, error) {
	ctx, span := proxmox.Tracer().Start(ctx, "proxmox.list")
	span.SetAttributes(attribute.String("proxmox.kind", k.typ))
	start := time.Now()
	entries, err := k.list(ctx, client)
	span.SetAttributes(attribute.Int("entries", len(entries)))
	k.resEndSpan(span, err)
	if err != nil {
		resLog().Debugw(ctx, "proxmox list failed", append([]any{"kind", k.typ}, resClassFields(err)...)...)
		return entries, err
	}
	resLog().Debugw(ctx, "proxmox list", "kind", k.typ, "entries", len(entries), "duration_ms", time.Since(start).Milliseconds())
	return entries, nil
}
