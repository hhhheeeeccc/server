.DEFAULT_GOAL := help

ENTERPRISE_ROOT := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
SERVER_DIR ?= $(abspath $(ENTERPRISE_ROOT)/..)
WEBAPP_DIR ?= $(abspath $(SERVER_DIR)/../webapp/channels)
SERVER_BINARY := $(SERVER_DIR)/bin/mattermost
BUILD_TAGS ?= enterprise sourceavailable
LDFLAGS := -X github.com/mattermost/mattermost/server/public/model.BuildEnterpriseReady=true -X github.com/mattermost/mattermost/server/public/model.BuildHashEnterprise=workspace-enterprise
ARGS ?= server --config ./config/config.json

.PHONY: help build-server build-webapp run-server

help:
	@echo 'make run-server   Build Enterprise and start the server in the foreground'
	@echo 'make build-server Build the Enterprise server executable only'
	@echo 'make build-webapp Build the web app using its existing build command'
	@echo 'Editor settings: server/config/workspace-visual-editor.json'

build-server:
	cd "$(SERVER_DIR)" && go build -mod=readonly -buildvcs=false -tags '$(BUILD_TAGS)' -ldflags '$(LDFLAGS)' -o "$(SERVER_BINARY)" ./cmd/mattermost

build-webapp:
	cd "$(WEBAPP_DIR)" && npm run build

# The upstream runtime resolves relative data, plugin, template and config paths
# from the server directory. Changing cwd preserves those paths; no source files
# or runtime configuration need to be rewritten for this entry point.
run-server: build-server
	cd "$(SERVER_DIR)" && exec "$(SERVER_BINARY)" $(ARGS)
