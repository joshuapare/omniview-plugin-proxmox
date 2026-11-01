package main

import (
	"github.com/omniviewdev/plugin-sdk/pkg/sdk"
	"github.com/omniviewdev/plugin-sdk/settings"

	pveexec "github.com/mononendev/omniview-plugin-proxmox/pkg/plugin/exec"
	pvelogs "github.com/mononendev/omniview-plugin-proxmox/pkg/plugin/logs"
	pvemetric "github.com/mononendev/omniview-plugin-proxmox/pkg/plugin/metric"
	"github.com/mononendev/omniview-plugin-proxmox/pkg/plugin/resource"
)

func main() {
	plugin := sdk.NewPlugin(sdk.PluginOpts{
		ID: "proxmox",
		Settings: []settings.Setting{
			{
				ID:    resource.SettingConfigFiles,
				Label: "Cluster config files",
				Description: "YAML files listing Proxmox VE API endpoints and tokens. " +
					"Each entry under `clusters:` becomes a connection.",
				Type:    settings.Text,
				Default: []string{resource.DefaultConfigFile},
				FileSelection: &settings.SettingFileSelection{
					Enabled:     true,
					Multiple:    true,
					DefaultPath: "~/.config/omniview-proxmox",
				},
				// Connections are loaded from these files when the plugin starts.
				RequiresRestart: true,
			},
		},
	})

	resource.Register(plugin)
	pveexec.Register(plugin)
	pvemetric.Register(plugin)
	pvelogs.Register(plugin)
	plugin.Serve()
}
