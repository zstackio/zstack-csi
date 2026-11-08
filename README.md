# zstack-csi

A Kubernetes CSI driver that gives pods ZStack data volumes on SharedBlock
(sblk), Ceph, NFS or SharedMountPoint primary storage. The Kubernetes nodes are
ZStack VMs; the driver works the way cloud-provider block CSI drivers do:

- The **controller** calls the ZStack API with an AccessKey to create, delete,
  resize and snapshot data volumes and to attach them to the node VMs.
- The **node** plugin finds the attached disk by its serial number (ZStack sets
  it to the volume UUID), formats and mounts it. It needs no ZStack credentials.

ZStack keeps doing everything storage-specific, so the driver never touches
the storage itself.

[ARCHITECTURE.md](ARCHITECTURE.md) (in Chinese) explains the data path, how a
node learns its VM UUID, and how this model differs from storage-native CSI
drivers.

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
- Node and management node clocks are synchronized (NTP). ZStack rejects
  AccessKey requests more than 15 minutes off its own clock.
- Kubernetes 1.25 or later, the minimum of the bundled sidecars. Snapshots need the VolumeSnapshot CRDs and
  snapshot-controller from
  [external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter).
- The driver namespace must allow privileged pods.

## Install

1. In ZStack, create an AccessKey as described in
   [ZStack permissions](#zstack-permissions) and look up the primary storage
   UUID.
2. Make the images available. `make package` builds the offline package
   `_output/zstack-csi-<version>.tgz` (driver and sidecar images, chart,
   VolumeSnapshot components, docs); its `INSTALL.md` walks through pushing
   the images to a registry or importing them on the nodes.

3. Install the chart:

   ```sh
   helm install zstack-csi charts/zstack-csi -n kube-system \
     --set imgurl=<registry prefix, empty to use the original image names> \
     --set zstack.host=<management node IP> \
     --set zstack.accessKeyId=<id> --set zstack.accessKeySecret=<secret> \
     --set storageClass.primaryStorageUuid=<primary storage UUID> \
     --set volumeSnapshotClass.create=true
   ```

   `imgurl` is prepended to every image. Use
   `zstack.existingSecret` instead of the two key values to keep the AccessKey
   out of Helm values. `make manifests` renders plain YAML.

`examples/` has a filesystem volume, a raw block volume, a multi-node shared
block volume, a snapshot restored into a larger volume, and a clone.

## StorageClass parameters

| Parameter | Description |
|---|---|
| `primaryStorageUuid` | Required. Primary storage that holds the volumes. |
| `provisioning` | `thin` or `thick`. Empty keeps the primary storage default. Ceph, NFS and SharedMountPoint volumes are always thin; ZStack records the setting but does not preallocate. |
| `csi.storage.k8s.io/fstype` | `ext4` (default) or `xfs`. |

## ZStack permissions

The driver does not need an admin account. Attaching a volume requires owning
the VM, so the AccessKey must belong to the identity that owns the node VMs;
the volumes the driver creates belong to that identity too.

- **Account AccessKey.** An AccessKey of the account that owns the node VMs.
  ZStack applies no policy to an account's own key, so the driver gets every
  permission of that account.
- **Least privilege (IAM2).** Put the node VMs in an IAM2 project and create
  the AccessKey for a project member whose only role is the one below. ZStack
  only restricts keys of IAM2 members; it always makes a normal account's key
  account-wide.

```sh
zstack-cli CreateIAM2Project name=k8s
zstack-cli ChangeResourceOwner accountUuid=<project account> resourceUuid=<node VM>   # each node VM
zstack-cli CreateIAM2VirtualID name=csi-driver password=<password>
zstack-cli AddIAM2VirtualIDsToProject projectUuid=<project> virtualIDUuids=<member>
zstack-cli CreateRole name=zstack-csi-driver statements='<statements below>'
zstack-cli AddRolesToIAM2VirtualID virtualIDUuid=<member> roleUuids=<role> projectUuid=<project>
zstack-cli CreateAccessKey accountUuid=<project account> userUuid=<member>
```

```json
[
  {
    "name": "zstack-csi-driver",
    "effect": "Allow",
    "actions": [
      "org.zstack.header.storage.primary.APIQueryPrimaryStorageMsg",
      "org.zstack.header.vm.APIQueryVmInstanceMsg",
      "org.zstack.header.volume.APIQueryVolumeMsg",
      "org.zstack.header.volume.APICreateDataVolumeMsg",
      "org.zstack.header.volume.APICreateDataVolumeFromVolumeSnapshotMsg",
      "org.zstack.header.volume.APIDeleteDataVolumeMsg",
      "org.zstack.header.volume.APIExpungeDataVolumeMsg",
      "org.zstack.header.volume.APIResizeDataVolumeMsg",
      "org.zstack.header.volume.APIAttachDataVolumeToVmMsg",
      "org.zstack.header.volume.APIDetachDataVolumeFromVmMsg",
      "org.zstack.mevoco.APIQueryShareableVolumeVmInstanceRefMsg",
      "org.zstack.header.volume.APICreateVolumeSnapshotMsg",
      "org.zstack.header.storage.snapshot.APIQueryVolumeSnapshotMsg",
      "org.zstack.header.storage.snapshot.APIDeleteVolumeSnapshotMsg"
    ]
  }
]
```

Project members may run query APIs by default; they are listed for
completeness.

## Behavior

- Access modes: ReadWriteOnce as filesystem or raw block; ReadWriteMany and
  ReadOnlyMany as raw block on ZStack shared volumes (`ephemeral::shareable`
  with virtio-scsi), which ZStack provides only on SharedBlock, Ceph and
  external (Addon) primary storage. Applications sharing a device must
  coordinate their writes and bypass the page cache, as with any shared disk.
- ReadOnlyMany volumes are set read-only on each node with `blockdev --setro`.
- Cloning a PVC goes through a temporary ZStack snapshot that is deleted once
  the new volume exists.
- Sizes are rounded up to whole GiB.
- Online expansion: the controller resizes the volume, the node grows the
  filesystem.
- XFS is created without `bigtime` and `inobtcount`, so node kernels older
  than 5.10 can mount it.
- `node.maxVolumesPerNode` (default 24) should match the ZStack global config
  `kvm/dataVolume.maxNum`.

## Known limitations

- No topology: every node VM must run in a cluster attached to the primary
  storage. A pod scheduled on another node stays in ContainerCreating because
  the attach fails.
- Local storage primary storage is not supported: its volumes attach only to
  VMs on the same host, which would need topology. The controller rejects PVCs
  on it.
- Multi-node access is raw block only; ext4 and xfs cannot be mounted on several
  nodes at once, so ReadOnlyMany filesystem volumes are not supported either.
  Shared volumes cannot be cloned, and multi-node volumes cannot be created
  from a snapshot or another volume.
- The controller rejects ReadWriteMany and ReadOnlyMany PVCs on primary
  storage without ZStack shared volumes, such as NFS and SharedMountPoint; they
  stay Pending with an event naming the supported storage types.
- SharedBlock creates shared volumes thick provisioned (a multi-node PVC with
  `provisioning: thin` is rejected) and cannot snapshot or resize them.
  Give multi-node PVCs a StorageClass with `allowVolumeExpansion: false`.
- Ceph resizes a shared volume only while it is detached from every VM: the
  expansion completes after the Pods using it are deleted.
- A ZStack snapshot belongs to its volume and is removed with it, so
  DeleteVolume fails with FailedPrecondition while the volume has snapshots.
  Delete the VolumeSnapshots first.
- The image is built for x86_64 only.

## Tested with

| Component | Version |
|---|---|
| ZStack | Cloud 5.5.38 with SharedBlock, Ceph, NFS and SharedMountPoint (on NFS) primary storage |
| Kubernetes | v1.35 (kubeadm), containerd 2.2 |
| Node VMs | Ubuntu 24.04, kernel 6.8 |

## Development

```sh
make test
make build
```
