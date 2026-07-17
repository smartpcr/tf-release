VERSION ?= 0.1.0
HOSTNAME = registry.local
NAMESPACE = smartpcr
NAME = labdeploy
BIN = terraform-provider-$(NAME)_v$(VERSION)
OS_ARCH ?= $(shell go env GOOS)_$(shell go env GOARCH)

default: build

# No cgo is a hard build invariant (DESIGN sec 20): the provider must be a
# statically linked binary with no external runtime dependencies.
build:
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BIN) .

tidy:
	go mod tidy

test:
	CGO_ENABLED=0 go test ./... -count=1

lint:
	golangci-lint run

# install into local filesystem mirror so `terraform init` finds it (DESIGN §16.1)
install: build
	mkdir -p ~/.terraform.d/plugins/$(HOSTNAME)/$(NAMESPACE)/$(NAME)/$(VERSION)/$(OS_ARCH)
	cp bin/$(BIN) ~/.terraform.d/plugins/$(HOSTNAME)/$(NAMESPACE)/$(NAME)/$(VERSION)/$(OS_ARCH)/

acc:
	TF_ACC=1 go test ./internal/provider -v -timeout 120m
