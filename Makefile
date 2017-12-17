.PHONY: all build containers release test lint

ifdef TRAVIS_COMMIT
VERSION := $(shell git rev-parse --short HEAD)
else
VERSION := $(shell git rev-parse --short HEAD)$(shell if ! git diff-index --quiet HEAD --; then echo "-dirty"; fi)
endif
BUILD_TIME := $(shell date +%Y-%m-%d-%H:%M)

GOBUILD := CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -i -ldflags "-s -X github.com/lstoll/k8s-vpcnet/version.Version=$(VERSION) -X github.com/lstoll/k8s-vpcnet/version.BuildTime=$(BUILD_TIME)"

TEMPDIR := $(shell mktemp -d)

all: manifest-latest.yaml build

build:
	mkdir -p build/bin
	$(GOBUILD) -o build/bin/eni-controller ./cmd/eni-controller
	$(GOBUILD) -o build/bin/vpcnet-daemon ./cmd/vpcnet-daemon
	$(GOBUILD) -o build/bin/loopback ./vendor/github.com/containernetworking/plugins/plugins/main/loopback
	$(GOBUILD) -o build/bin/ptp ./vendor/github.com/containernetworking/plugins/plugins/main/ptp
	$(GOBUILD) -o build/bin/vpcnet ./cmd/cni-vpcnet

test:
	go test -v ./...

lint:
	GOOS=linux GOARCH=amd64 gometalinter --config=.gometalinter.cfg.json --deadline=1200s ./...

vethtest:
	sudo go test -v ./cmd/cni-vpcnet/ -vethtests

containers: build
	docker build -f Dockerfile.eni-controller -t eni-controller:$(VERSION) .
	docker build -f Dockerfile.vpcnet-daemon -t vpcnet-daemon:$(VERSION) .

manifest-latest.yaml: manifest.yaml
	cat manifest.yaml | sed -e "s/{{\\.VersionTag}}/latest/g" | sed -e "s/{{\\.Timestamp}}//g" > manifest-latest.yaml

release: build containers manifest-latest.yaml
	docker tag eni-controller:$(VERSION) lstoll/eni-controller:$(VERSION)
	docker push lstoll/eni-controller:$(VERSION)
	docker tag vpcnet-daemon:$(VERSION) lstoll/vpcnet-daemon:$(VERSION)
	docker push lstoll/vpcnet-daemon:$(VERSION)
# For now YOLO as latest, later become branch specific
	@if [ "$$TRAVIS_BRANCH" = "master" ]; then \
		docker tag eni-controller:$(VERSION) lstoll/eni-controller:latest && \
		docker push lstoll/eni-controller:latest && \
		docker tag vpcnet-daemon:$(VERSION) lstoll/vpcnet-daemon:latest && \
		docker push lstoll/vpcnet-daemon:latest; \
	fi
