.PHONY: all build containers release

VERSION := $(shell git rev-parse --short HEAD)$(shell if ! git diff-index --quiet HEAD --; then echo "-dirty"; fi)
BUILD_TIME := $(shell date +%Y-%m-%d-%H:%M)

GOBUILD := GOOS=linux GOARCH=amd64 go build -i -ldflags "-s -X github.com/lstoll/k8s-vpcnet/version.Version=$(VERSION) -X github.com/lstoll/k8s-vpcnet/version.BuildTime=$(BUILD_TIME)"

all: build

build:
	mkdir -p build/bin
	$(GOBUILD) -o build/bin/eni-controller ./cmd/eni-controller
	$(GOBUILD) -o build/bin/vpcnet-configure ./cmd/vpcnet-configure

containers: build
	docker build --build-arg BINARY=eni-controller -t eni-controller:$(VERSION) .
	docker build --build-arg BINARY=vpcnet-configure -t vpcnet-configure:$(VERSION) .

release: containers
	docker tag eni-controller:$(VERSION) lstoll/eni-controller:$(VERSION)
	docker push lstoll/eni-controller:$(VERSION)
	docker tag vpcnet-configure:$(VERSION) lstoll/vpcnet-configure:$(VERSION)
	docker push lstoll/vpcnet-configure:$(VERSION)
# For now YOLO as latest, later become branch specific
	docker tag eni-controller:$(VERSION) lstoll/eni-controller:latest
	docker push lstoll/eni-controller:latest
	docker tag vpcnet-configure:$(VERSION) lstoll/vpcnet-configure:latest
	docker push lstoll/vpcnet-configure:latest
