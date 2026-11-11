package logs

import (
	"fmt"

	logging "github.com/omniviewdev/plugin-sdk/log"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// logger returns the plugin logger the SDK installs in NewPlugin.
func logger() logging.Logger { return logging.Default().Named("proxmox.logs") }

// describeClass names a failure for text shown to the user without the error
// text, which comes from the Proxmox API.
func describeClass(err error) string {
	_, class, status := proxmox.ClassifyError(err)
	if status > 0 {
		return fmt.Sprintf("%s, HTTP %d", class, status)
	}
	return class
}

// errFields classifies err for a log line; it never carries the error text,
// which may come from the Proxmox API.
func errFields(err error) []any {
	outcome, class, status := proxmox.ClassifyError(err)
	kv := []any{"outcome", outcome, "error_class", class}
	if status > 0 {
		kv = append(kv, "http_response_status_code", status)
	}
	return kv
}
