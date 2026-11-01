package resource

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// agentPollInterval is how often guest-exec reads the command's status.
var agentPollInterval = time.Second

// Bounds of the guest-exec timeout, in seconds.
const (
	guestExecDefaultTimeout = 30
	guestExecMaxTimeout     = 100
)

func ptrFloat(f float64) *float64 { return &f }

var cloneAction = resourcev2.ActionDescriptor{
	ID: actionClone, Label: "Clone", Icon: "LuCopy",
	Description: "Clone the guest to a new VMID, optionally onto another node or storage, and wait for the copy.",
	ParamsSchema: &resourcev2.Schema{
		Properties: map[string]resourcev2.SchemaProperty{
			"newid":   {Type: resourcev2.SchemaInteger, Minimum: ptrFloat(100), Description: "VMID of the clone."},
			"name":    {Type: resourcev2.SchemaString, Description: "Name (hostname for containers) of the clone."},
			"target":  {Type: resourcev2.SchemaString, Description: "Node to create the clone on; default the guest's node."},
			"full":    {Type: resourcev2.SchemaBoolean, Description: "Full copy instead of a linked clone (templates only)."},
			"storage": {Type: resourcev2.SchemaString, Description: "Target storage for a full clone."},
		},
		Required: []string{"newid"},
	},
}

var guestExecAction = resourcev2.ActionDescriptor{
	ID: actionGuestExec, Label: "Run Command", Icon: "LuTerminal", Dangerous: true,
	Description: "Run a command through the QEMU guest agent and return its exit code and output. Needs the agent running in the VM.",
	ParamsSchema: &resourcev2.Schema{
		Properties: map[string]resourcev2.SchemaProperty{
			"command":   {Type: resourcev2.SchemaArray, Description: "Program and arguments; no shell is implied."},
			"inputData": {Type: resourcev2.SchemaString, Description: "Data written to the command's standard input."},
			"timeoutSeconds": {
				Type: resourcev2.SchemaInteger, Minimum: ptrFloat(1), Maximum: ptrFloat(guestExecMaxTimeout),
				Description: "How long to wait for the command to exit; default 30.",
			},
		},
		Required: []string{"command"},
	},
}

// migrateAction declares migrate with this guest type's options.
func (k *kind) migrateAction() resourcev2.ActionDescriptor {
	props := map[string]resourcev2.SchemaProperty{
		"target": {Type: resourcev2.SchemaString, Description: "Node to migrate to."},
	}
	description := "Migrate the virtual machine to another node of the cluster and wait for the migration."
	if k.guestType == "qemu" {
		props["online"] = resourcev2.SchemaProperty{Type: resourcev2.SchemaBoolean, Description: "Live-migrate a running VM."}
		props["withLocalDisks"] = resourcev2.SchemaProperty{Type: resourcev2.SchemaBoolean, Description: "Copy local disks to the target."}
	} else {
		props["restart"] = resourcev2.SchemaProperty{Type: resourcev2.SchemaBoolean, Description: "Restart-migrate a running container."}
		description = "Migrate the container to another node of the cluster and wait for the migration."
	}
	return resourcev2.ActionDescriptor{
		ID: actionMigrate, Label: "Migrate", Icon: "LuArrowRightLeft", Description: description,
		ParamsSchema: &resourcev2.Schema{Properties: props, Required: []string{"target"}},
	}
}

// migrate moves the guest to the target node.
func (k *kind) migrate(ctx context.Context, client *proxmox.Client, id string, input resourcev2.Value) (resourcev2.Value, error) {
	params, err := actionParams(input)
	if err != nil {
		return resourcev2.Value{}, err
	}
	target, err := stringParam(params, "target", true)
	if err != nil {
		return resourcev2.Value{}, err
	}
	form := url.Values{"target": {target}}
	flags := map[string]string{"restart": "restart"}
	if k.guestType == "qemu" {
		flags = map[string]string{"online": "online", "withLocalDisks": "with-local-disks"}
	}
	for param, field := range flags {
		set, err := boolParam(params, param)
		if err != nil {
			return resourcev2.Value{}, err
		}
		if set {
			form.Set(field, "1")
		}
	}
	g, err := k.find(ctx, client, id)
	if err != nil {
		return resourcev2.Value{}, err
	}
	what := "migrate " + k.noun + " " + id + " to " + target
	upid, err := client.MigrateGuest(ctx, g, form)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privMigrate, what: what, err: err}
	}
	done, err := waitTask(ctx, client, g.Node, upid)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privMigrate, what: what, err: err}
	}
	message := "Migration of " + g.Label() + " to " + target + " is still running."
	if done {
		message = "Migrated " + g.Label() + " to " + target + "."
	}
	return resourcev2.ValueFromNative(map[string]any{"message": message, "upid": upid})
}

// clone copies the guest to a new VMID.
func (k *kind) clone(ctx context.Context, client *proxmox.Client, id string, input resourcev2.Value) (resourcev2.Value, error) {
	params, err := actionParams(input)
	if err != nil {
		return resourcev2.Value{}, err
	}
	newid, ok, err := intParam(params, "newid")
	if err != nil {
		return resourcev2.Value{}, err
	}
	if !ok || newid < 100 {
		return resourcev2.Value{}, invalidParams("newid is required and must be at least 100.")
	}
	form := url.Values{"newid": {strconv.FormatInt(newid, 10)}}
	nameField := "name"
	if k.guestType == "lxc" {
		nameField = "hostname"
	}
	for param, field := range map[string]string{"name": nameField, "target": "target", "storage": "storage"} {
		v, err := stringParam(params, param, false)
		if err != nil {
			return resourcev2.Value{}, err
		}
		if v != "" {
			form.Set(field, v)
		}
	}
	if _, present := params["full"]; present {
		full, err := boolParam(params, "full")
		if err != nil {
			return resourcev2.Value{}, err
		}
		form.Set("full", map[bool]string{true: "1", false: "0"}[full])
	}
	g, err := k.find(ctx, client, id)
	if err != nil {
		return resourcev2.Value{}, err
	}
	newID := k.guestType + "/" + strconv.FormatInt(newid, 10)
	what := "clone " + k.noun + " " + id + " to " + newID
	upid, err := client.CloneGuest(ctx, g, form)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privClone, what: what, err: err}
	}
	done, err := waitTask(ctx, client, g.Node, upid)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privClone, what: what, err: err}
	}
	message := "Clone of " + g.Label() + " to " + newID + " is still running."
	if done {
		message = "Cloned " + g.Label() + " to " + newID + "."
	}
	return resourcev2.ValueFromNative(map[string]any{"message": message, "upid": upid, "newID": newID})
}

// guestExec runs a command through the QEMU guest agent and polls until it
// exits or the timeout ends.
func (k *kind) guestExec(ctx context.Context, client *proxmox.Client, id string, input resourcev2.Value) (resourcev2.Value, error) {
	params, err := actionParams(input)
	if err != nil {
		return resourcev2.Value{}, err
	}
	command, err := stringListParam(params, "command")
	if err != nil {
		return resourcev2.Value{}, err
	}
	if len(command) == 0 || command[0] == "" {
		return resourcev2.Value{}, invalidParams("command is required: the program and its arguments.")
	}
	inputData, err := stringParam(params, "inputData", false)
	if err != nil {
		return resourcev2.Value{}, err
	}
	timeout, ok, err := intParam(params, "timeoutSeconds")
	if err != nil {
		return resourcev2.Value{}, err
	}
	if !ok {
		timeout = guestExecDefaultTimeout
	}
	if timeout < 1 || timeout > guestExecMaxTimeout {
		return resourcev2.Value{}, invalidParams("timeoutSeconds must be between 1 and 100.")
	}
	g, err := k.find(ctx, client, id)
	if err != nil {
		return resourcev2.Value{}, err
	}
	what := "run a command through the guest agent of " + k.noun + " " + id
	pid, err := client.AgentExec(ctx, g, command, inputData)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privGuestAgent, what: what, err: err}
	}

	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	ticker := time.NewTicker(agentPollInterval)
	defer ticker.Stop()
	var status proxmox.AgentExecStatus
	for {
		status, err = client.AgentExecStatus(waitCtx, g, pid)
		if err != nil {
			if ctx.Err() == nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				break
			}
			return resourcev2.Value{}, &writeError{privilege: privGuestAgent, what: what, err: err}
		}
		if status.Exited == 1 {
			break
		}
		select {
		case <-ctx.Done():
			return resourcev2.Value{}, ctx.Err()
		case <-waitCtx.Done():
		case <-ticker.C:
			continue
		}
		break
	}
	out := map[string]any{
		"exited":          status.Exited == 1,
		"stdout":          status.OutData,
		"stderr":          status.ErrData,
		"stdoutTruncated": status.OutTruncated == 1,
		"stderrTruncated": status.ErrTruncated == 1,
		"pid":             pid,
	}
	if status.Exited == 1 && status.ExitCode != nil {
		out["exitCode"] = int64(*status.ExitCode)
	}
	return resourcev2.ValueFromNative(out)
}
