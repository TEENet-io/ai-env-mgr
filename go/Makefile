GOOS    ?= windows
GOARCH  ?= amd64
DIST    ?= ../dist
VERSION ?= 1.0.0

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all agent admin local dist test vet fmt check clean

all: agent admin

# The OSS bucket and keys are plain strings in cmd/agent/credentials.go and
# cmd/admin/credentials.go -- fill them in there before building. They are in
# separate packages so the administrator's read-write key never ends up in
# agent.exe; a test in cmd/agent enforces that.
agent:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/agent.exe ./cmd/agent

admin:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/admin.exe ./cmd/admin

# local builds admin for whatever machine you are on, so the administrator
# can drive the bucket from a Linux box. Not a deliverable: the customer gets
# the Windows binaries in dist/.
local:
	@mkdir -p ../bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o ../bin/admin ./cmd/admin
	@echo "built ../bin/admin for $$(go env GOOS)/$$(go env GOARCH)"

# dist assembles everything the customer receives. Both binaries carry their
# own OSS settings, written into cmd/*/credentials.go before building, so no
# configuration file is delivered alongside them.
dist: all
	@echo "dist ready:"
	@ls -1 $(DIST)

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...

fmt:
	gofmt -w .

# check is what CI would run: formatting, vet on both platforms, tests, and a
# Windows build, since that is the only target that actually ships.
check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	go test ./...
	GOOS=windows GOARCH=amd64 go build ./...
	@echo "all checks passed"

clean:
	rm -f $(DIST)/agent.exe $(DIST)/admin.exe ../bin/admin
