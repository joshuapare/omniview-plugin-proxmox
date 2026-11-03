PLUGIN_ID := proxmox
PLATFORMS ?= darwin_arm64 darwin_amd64 linux_amd64 linux_arm64 windows_amd64
# Releases pass the tag here; the default is whatever plugin.yaml says.
VERSION ?= $(shell sed -n 's/^version: *//p' plugin.yaml)

# Every Go subprocess uses this module, never an enclosing go.work or GOFLAGS.
override export GOWORK := off
override export GOFLAGS :=

.PHONY: init build build-go build-ui test package clean

init:
	cd ui && pnpm install --frozen-lockfile

# build/bin/plugin is where the host expects a locally built backend.
build-go:
	go build -o build/bin/plugin ./pkg

build-ui: init
	cd ui && pnpm build

build: build-go build-ui

test:
	go test ./... -count=1
	cd ui && pnpm typecheck
	cd ui && pnpm test

# Produces dist/<id>-<os>_<arch>.tar.gz, the names publish-plugin-action uploads.
# Layout is what the app installs: plugin.yaml, assets/ and bin/plugin at the root.
package: build-ui
	rm -rf dist
	@set -eu; for platform in $(PLATFORMS); do \
		goos=$${platform%_*}; goarch=$${platform#*_}; \
		bin=plugin; [ "$$goos" = windows ] && bin=plugin.exe; \
		stage=dist/pkg-$$platform; \
		mkdir -p $$stage/bin $$stage/assets; \
		sed "s/^version: .*/version: $(VERSION)/" plugin.yaml > $$stage/plugin.yaml; \
		cp -R ui/dist/assets/. $$stage/assets/; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath -o $$stage/bin/$$bin ./pkg; \
		COPYFILE_DISABLE=1 tar -czf dist/$(PLUGIN_ID)-$$platform.tar.gz -C $$stage plugin.yaml assets bin; \
		rm -rf $$stage; \
	done

clean:
	rm -rf build dist ui/dist
