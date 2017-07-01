.PHONY: all build containers release

VERSION := $(shell git rev-parse --short HEAD)$(shell if ! git diff-index --quiet HEAD --; then echo "-dirty"; fi)
BUILD_TIME := $(shell date +%Y-%m-%d-%H:%M)

GOBUILD := GOOS=linux GOARCH=amd64 go build -i -ldflags "-s -X github.com/lstoll/k8s-vpcnet/version.Version=$(VERSION) -X github.com/lstoll/k8s-vpcnet/version.BuildTime=$(BUILD_TIME)"

all: build

build:
	mkdir -p build/bin
	$(GOBUILD) -o build/bin/eni-controller ./cmd/eni-controller

containers: build
	docker build --build-arg BINARY=eni-controller -t eni-controller:$(VERSION) .

release: containers
	docker tag eni-controller:$(VERSION) lstoll/eni-controller:$(VERSION)
	docker push lstoll/eni-controller:$(VERSION)
# For now YOLO as latest, later become branch specific
	docker tag eni-controller:$(VERSION) lstoll/eni-controller:latest
	docker push lstoll/eni-controller:latest
