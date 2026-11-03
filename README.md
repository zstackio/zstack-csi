# zstack-csi

A Kubernetes CSI driver that gives pods ZStack data volumes, for example
volumes on SharedBlock (sblk) primary storage. The Kubernetes nodes are ZStack
VMs; the driver works the way cloud-provider block CSI drivers do:

- The **controller** calls the ZStack API with an AccessKey to create, delete,
  resize and snapshot data volumes and to attach them to the node VMs.
- The **node** plugin finds the attached disk by its serial number (ZStack sets
  it to the volume UUID), formats and mounts it. It needs no ZStack credentials.

ZStack keeps doing everything storage-specific (LV activation and locking,
qcow2 chains, provisioning), so the driver never touches the shared VG.

| CSI call | ZStack API |
|---|---|
| CreateVolume | CreateDataVolume (or CreateDataVolumeFromVolumeSnapshot) |
| DeleteVolume | DeleteDataVolume + ExpungeDataVolume |
| ControllerPublish/Unpublish | AttachDataVolumeToVm / DetachDataVolumeFromVm |
| ControllerExpandVolume | ResizeDataVolume |
| CreateSnapshot/DeleteSnapshot | CreateVolumeSnapshot / DeleteVolumeSnapshot |

## Requirements

- Every Kubernetes node is a ZStack VM. The node ID is the VM UUID, read from
  `/sys/class/dmi/id/product_uuid`.
- All node VMs run in clusters attached to the primary storage named in the
  StorageClass; the driver does not report topology.
- The controller pods can reach the ZStack management node API (port 8080).
- Node and management node clocks are synchronized (NTP). ZStack accepts an
  AccessKey request only within 15 minutes of its own clock; the controller
  detects the management node's time zone at start-up, because with the default
  global config `rest/checkTimeZone=false` ZStack compares the wall-clock time
  and ignores the zone. If ZStack later rejects the signature, for example after
  a daylight-saving change, the controller exits and detects it again on restart.
- Kubernetes 1.25 or later, the minimum of the bundled sidecars. Snapshots
  need the VolumeSnapshot CRDs and snapshot-controller from
  [external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter).
- The driver namespace must allow privileged pods.

## Install

1. In ZStack, create an AccessKey for an account that may manage volumes and
   attach them to the node VMs, and look up the primary storage UUID.
2. Build and push the image, or load the archive on every node:

   ```sh
   make image VERSION=0.1.0              # or: make image-save
   ```

3. Install the chart:

   ```sh
   helm install zstack-csi charts/zstack-csi -n kube-system \
     --set image.repository=<registry>/zstack-csi \
     --set zstack.host=<management node IP> \
     --set zstack.accessKeyId=<id> --set zstack.accessKeySecret=<secret> \
     --set storageClass.primaryStorageUuid=<primary storage UUID> \
     --set volumeSnapshotClass.create=true
   ```

   Use `zstack.existingSecret` instead of the two key values to keep the
   AccessKey out of Helm values. `make manifests` renders plain YAML.

`examples/` has a filesystem volume, a raw block volume, a multi-node shared
block volume, a snapshot restored into a larger volume, and a clone.

## StorageClass parameters

| Parameter | Description |
|---|---|
| `primaryStorageUuid` | Required. Primary storage that holds the volumes. |
| `provisioning` | `thin` or `thick`. Empty keeps the primary storage default. |
| `csi.storage.k8s.io/fstype` | `ext4` (default) or `xfs`. |

## Behavior and limits

- Access modes: ReadWriteOnce as filesystem or raw block.
  ReadWriteMany and ReadOnlyMany need `volumeMode: Block` and are served by
  ZStack shared volumes (`ephemeral::shareable` with virtio-scsi), which
  SharedBlock creates raw and thick provisioned and cannot snapshot. The
  applications sharing the device must coordinate their writes, as with any
  shared disk.
- Cloning a PVC goes through a temporary ZStack snapshot that is deleted once
  the new volume exists. Shared volumes cannot be cloned.
- Sizes are rounded up to whole GiB.
- Online expansion: the controller resizes the volume, the node grows the
  filesystem.
- A ZStack snapshot belongs to its volume and is removed with it, so
  DeleteVolume fails with FailedPrecondition while the volume has snapshots.
  Delete the VolumeSnapshots first.
- XFS is created without `bigtime` and `inobtcount` so that node kernels older
  than 5.10 can mount it.
- `node.maxVolumesPerNode` (default 24) should match the ZStack global config
  `kvm/dataVolume.maxNum`.

## Tested with

| Component | Version |
|---|---|
| ZStack | Cloud 5.5.38.869, SharedBlock primary storage over iSCSI |
| Kubernetes | v1.35.10 (kubeadm), containerd 2.2.1 |
| Node VMs | Ubuntu 24.04, kernel 6.8, virtio-blk disks |
| Sidecars | as pinned in `charts/zstack-csi/values.yaml` |

[csi-sanity](https://github.com/kubernetes-csi/csi-test) v5.6.0 against the live
ZStack API passes; skipped specs cover capabilities the driver does not
declare. End-to-end checks covered dynamic provisioning, moving a pod to
another node, online expansion (ext4 and XFS), raw block volumes, a shared
block volume written and read from two nodes, snapshot restore and cloning into
larger volumes, thick provisioning, time zone detection with
`rest/checkTimeZone` both false and true (including the restart after it
changes), and full cleanup.

## Development

```sh
make test
make build
```
