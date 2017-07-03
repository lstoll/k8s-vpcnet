.PHONY: all build containers release test cni-bundle

ifdef TRAVIS_COMMIT
VERSION := $(shell git rev-parse --short HEAD)
else
VERSION := $(shell git rev-parse --short HEAD)$(shell if ! git diff-index --quiet HEAD --; then echo "-dirty"; fi)
endif
BUILD_TIME := $(shell date +%Y-%m-%d-%H:%M)

GOBUILD := CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -i -ldflags "-s -X github.com/lstoll/k8s-vpcnet/version.Version=$(VERSION) -X github.com/lstoll/k8s-vpcnet/version.BuildTime=$(BUILD_TIME)"

TEMPDIR := $(shell mktemp -d)

all: build

build:
	mkdir -p build/bin
	$(GOBUILD) -o build/bin/eni-controller ./cmd/eni-controller
	$(GOBUILD) -o build/bin/vpcnet-configure ./cmd/vpcnet-configure
	$(GOBUILD) -o build/bin/bridge ./vendor/github.com/containernetworking/plugins/plugins/main/bridge
	$(GOBUILD) -o build/bin/loopback ./vendor/github.com/containernetworking/plugins/plugins/main/loopback
	$(GOBUILD) -o build/bin/vpcnet ./cmd/cni-ipam-vpcnet

test:
	go test -v $$(go list ./... | grep -v /vendor/)

containers: build
	docker build --build-arg BINARY=eni-controller -t eni-controller:$(VERSION) .
	docker build --build-arg BINARY=vpcnet-configure -t vpcnet-configure:$(VERSION) .

cni-bundle: build
	cd build/bin && tar -zcvf ../cni-$(VERSION).tgz vpcnet bridge loopback

release: build containers cni-bundle
	git status
	docker tag eni-controller:$(VERSION) lstoll/eni-controller:$(VERSION)
	docker push lstoll/eni-controller:$(VERSION)
	docker tag vpcnet-configure:$(VERSION) lstoll/vpcnet-configure:$(VERSION)
	docker push lstoll/vpcnet-configure:$(VERSION)
	aws s3 cp --acl public-read build/cni-$(VERSION).tgz s3://s3.lstoll.net/artifacts/k8s-vpcnet/cni/
	cat manifest.yaml | sed -e "s/{{VERSION_TAG}}/$(VERSION)/g" | sed -e "s/{{TIMESTAMP}}/$(TIMESTAMP)/g" > $(TEMPDIR)/manifest-$(VERSION).yaml
	aws s3 cp --acl public-read $(TEMPDIR)/manifest-$(VERSION).yaml s3://s3.lstoll.net/artifacts/k8s-vpcnet/manifest/
# For now YOLO as latest, later become branch specific
	@if [ "$$TRAVIS_BRANCH" = "master" ]; then \
		docker tag eni-controller:$(VERSION) lstoll/eni-controller:latest && \
		docker push lstoll/eni-controller:latest && \
		docker tag vpcnet-configure:$(VERSION) lstoll/vpcnet-configure:latest && \
		docker push lstoll/vpcnet-configure:latest && \
		aws s3 cp --acl public-read build/cni-$(VERSION).tgz s3://s3.lstoll.net/artifacts/k8s-vpcnet/cni/cni-latest.tgz && \
		cat manifest.yaml | sed -e "s/{{VERSION_TAG}}/latest/g" | sed -e "s/{{TIMESTAMP}}/$(TIMESTAMP)/g" > $(TEMPDIR)/manifest-latest.yaml && \
		aws s3 cp --acl public-read $(TEMPDIR)/manifest-latest.yaml s3://s3.lstoll.net/artifacts/k8s-vpcnet/manifest/ ; \
	fi
