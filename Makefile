VERSION ?= 0.1.0
HOSTNAME = registry.local
NAMESPACE = smartpcr
NAME = labdeploy
BIN = terraform-provider-$(NAME)_v$(VERSION)
OS_ARCH ?= $(shell go env GOOS)_$(shell go env GOARCH)

default: build

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BIN) .

test:
	go test ./... -count=1

lint:
	golangci-lint run

# install into local filesystem mirror so `terraform init` finds it (DESIGN §16.1)
install: build
	mkdir -p ~/.terraform.d/plugins/$(HOSTNAME)/$(NAMESPACE)/$(NAME)/$(VERSION)/$(OS_ARCH)
	cp bin/$(BIN) ~/.terraform.d/plugins/$(HOSTNAME)/$(NAMESPACE)/$(NAME)/$(VERSION)/$(OS_ARCH)/

acc:
	TF_ACC=1 go test ./internal/provider -v -timeout 120m
