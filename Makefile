VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/CarriedWorldUniverse/nexus/internal/version.Version=$(VERSION)

# Binaries shipped from this repo. Mapped from cmd-dir → bin name. All
# share the same version package and ldflags.
BINS := nexus agentfunnel aspect nexus-comms-mcp nexus-imap-mcp nexus-jira-mcp nexus-skills-mcp nexus-watch outpost

CMD_nexus           := ./nexus/cmd/nexus
CMD_agentfunnel     := ./runtime/cmd/agentfunnel
CMD_aspect          := ./runtime/cmd/aspect
CMD_nexus-comms-mcp := ./runtime/cmd/nexus-comms-mcp
CMD_nexus-imap-mcp  := ./runtime/cmd/nexus-imap-mcp
CMD_nexus-jira-mcp  := ./runtime/cmd/nexus-jira-mcp
CMD_nexus-skills-mcp := ./runtime/cmd/nexus-skills-mcp
CMD_nexus-watch     := ./runtime/cmd/nexus-watch
CMD_outpost         := ./nexus/cmd/outpost

.PHONY: build $(BINS) test vet version clean all

all: $(BINS)

build: $(BINS)

$(BINS):
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/$@ $(CMD_$@)

bin/%:
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o $@ $(CMD_$*)

test:
	go test -race ./...

vet:
	go vet ./...

version:
	@echo $(VERSION)

clean:
	rm -rf bin/
