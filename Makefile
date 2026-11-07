VERSION   ?= $(shell awk -F'"' '/^appVersion:/{print $$2}' charts/zstack-csi/Chart.yaml)
ARCH      ?= amd64
IMAGE     ?= zstack.io/zstack-csi/zstack-csi
ENGINE    ?= podman
NAMESPACE ?= kube-system
BUILD_FLAGS ?=

VALUES    := charts/zstack-csi/values.yaml
SIDECARS  := $(shell awk '/^sidecars:/{f=1; next} /^[^ ]/{f=0} f{print $$2}' $(VALUES))
SNAPSHOT_VERSION := $(shell awk -F: '/csi-snapshotter:/{print $$NF}' $(VALUES))
SNAPSHOT_CONTROLLER := registry.k8s.io/sig-storage/snapshot-controller:$(SNAPSHOT_VERSION)
SNAPSHOT_URL := https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$(SNAPSHOT_VERSION)
SNAPSHOT_FILES := \
	client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml \
	client/config/crd/snapshot.storage.k8s.io_volumesnapshotcontents.yaml \
	client/config/crd/snapshot.storage.k8s.io_volumesnapshots.yaml \
	deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml \
	deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml
PKG       := _output/zstack-csi-$(VERSION)
SAVE_FLAGS := $(if $(filter podman,$(ENGINE)),-m)

.PHONY: build test image package manifests

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o _output/linux-$(ARCH)/zstack-csi ./cmd/zstack-csi

test:
	go test ./...

image: build
	$(ENGINE) build $(BUILD_FLAGS) --platform linux/$(ARCH) -f build/Dockerfile -t $(IMAGE):$(VERSION) .

# Offline package in the layout of zbs-csi: every image in one archive, the
# chart, the VolumeSnapshot components matching csi-snapshotter, and the docs.
# Needs access to registry.k8s.io and GitHub.
package: image
	rm -rf $(PKG) && mkdir -p $(PKG)/snapshot
	for i in $(SIDECARS) $(SNAPSHOT_CONTROLLER); do $(ENGINE) pull --platform linux/$(ARCH) $$i || exit 1; done
	printf '%s\n' $(IMAGE):$(VERSION) $(SIDECARS) $(SNAPSHOT_CONTROLLER) > $(PKG)/images.txt
	$(ENGINE) save $(SAVE_FLAGS) -o $(PKG)/zstack-csi-images-$(VERSION).tar $$(cat $(PKG)/images.txt)
	for f in $(SNAPSHOT_FILES); do curl -fsSL $(SNAPSHOT_URL)/$$f -o $(PKG)/snapshot/$$(basename $$f) || exit 1; done
	sed -i 's#image: .*/snapshot-controller:.*#image: $(SNAPSHOT_CONTROLLER)#' $(PKG)/snapshot/setup-snapshot-controller.yaml
	cp -r charts/zstack-csi examples $(PKG)/
	cp README.md ARCHITECTURE.md $(PKG)/
	sed 's/@VERSION@/$(VERSION)/g' build/INSTALL.md > $(PKG)/INSTALL.md
	tar -C _output -czf _output/zstack-csi-$(VERSION).tgz zstack-csi-$(VERSION)

# Plain manifests for installs without Helm.
manifests:
	helm template zstack-csi charts/zstack-csi --namespace $(NAMESPACE) > _output/zstack-csi-$(VERSION).yaml
