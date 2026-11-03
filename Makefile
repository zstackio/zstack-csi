VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCH      ?= amd64
IMAGE     ?= zstack-csi
ENGINE    ?= podman
NAMESPACE ?= kube-system
BUILD_FLAGS ?=

.PHONY: build test image image-save manifests

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o _output/linux-$(ARCH)/zstack-csi ./cmd/zstack-csi

test:
	go test ./...

image: build
	$(ENGINE) build $(BUILD_FLAGS) --platform linux/$(ARCH) -f build/Dockerfile -t $(IMAGE):$(VERSION) .

# Image archive for clusters without registry access; load it on each node with
# `ctr -n k8s.io images import`.
image-save: image
	$(ENGINE) save -o _output/$(IMAGE)-$(VERSION)-$(ARCH).tar $(IMAGE):$(VERSION)

# Plain manifests for installs without Helm.
manifests:
	helm template zstack-csi charts/zstack-csi --namespace $(NAMESPACE) \
		--set image.tag=$(VERSION) > _output/zstack-csi-$(VERSION).yaml
