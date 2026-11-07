# zstack-csi

A Kubernetes CSI driver that gives pods ZStack data volumes, for example
volumes on SharedBlock (sblk) or Ceph primary storage. The Kubernetes nodes are
ZStack VMs; the driver works the way cloud-provider block CSI drivers do:

- The **controller** calls the ZStack API with an AccessKey to create, delete,
  resize and snapshot data volumes and to attach them to the node VMs.
- The **node** plugin finds the attached disk by its serial number (ZStack sets
  it to the volume UUID), formats and mounts it. It needs no ZStack credentials.

ZStack keeps doing everything storage-specific (LV activation and locking on
SharedBlock, RBD images on Ceph, provisioning), so the driver never touches the
storage itself.

[ARCHITECTURE.md](ARCHITECTURE.md) (in Chinese) explains the data path, how a
node learns its VM UUID, and how this model differs from storage-native CSI
drivers such as XSKY, Ceph and ZBS.

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
- Kubernetes 1.25 or later is the minimum of the bundled sidecars; only 1.35
  has been tested (see below). Snapshots need the VolumeSnapshot CRDs and
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

   `imgurl` is prepended to every image, as in zbs-csi. Use
   `zstack.existingSecret` instead of the two key values to keep the AccessKey
   out of Helm values. `make manifests` renders plain YAML.

`examples/` has a filesystem volume, a raw block volume, a multi-node shared
block volume, a snapshot restored into a larger volume, and a clone.

## StorageClass parameters

| Parameter | Description |
|---|---|
| `primaryStorageUuid` | Required. Primary storage that holds the volumes. |
| `provisioning` | `thin` or `thick`. Empty keeps the primary storage default. Ceph volumes are always thin; ZStack records the setting but does not preallocate. |
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
  account-wide. With this role the driver passes all tests, and write APIs
  outside it are denied.

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

GetVersion needs no credentials. Project members may run query APIs by default;
they are listed for completeness. ZStack treats GetPlatformTimeZone as
admin-only, which is why the driver detects the time zone without it.

## Behavior

- Access modes: ReadWriteOnce as filesystem or raw block; ReadWriteMany and
  ReadOnlyMany as raw block on ZStack shared volumes (`ephemeral::shareable`
  with virtio-scsi). Applications sharing a device must coordinate their writes
  and bypass the page cache, as with any shared disk.
- ReadOnlyMany volumes are set read-only on each node with `blockdev --setro`.
- Cloning a PVC goes through a temporary ZStack snapshot that is deleted once
  the new volume exists.
- Sizes are rounded up to whole GiB.
- Online expansion: the controller resizes the volume, the node grows the
  filesystem.
- XFS is created without `bigtime` and `inobtcount` so that node kernels older
  than 5.10 can mount it.
- `node.maxVolumesPerNode` (default 24) should match the ZStack global config
  `kvm/dataVolume.maxNum`.

## Known limitations

- No topology: every node VM must run in a cluster attached to the primary
  storage. A pod scheduled on another node stays in ContainerCreating because
  the attach fails.
- Multi-node access is raw block only; ext4 and xfs cannot be mounted on several
  nodes at once, so ReadOnlyMany filesystem volumes are not supported either.
- SharedBlock creates shared volumes raw and thick provisioned (a multi-node
  PVC with `provisioning: thin` is rejected) and cannot snapshot, clone or
  resize them. Kubernetes marks such an expansion infeasible and the volume
  keeps its size; give multi-node PVCs a StorageClass with
  `allowVolumeExpansion: false` to reject the request up front.
- Ceph resizes a shared volume only while no running VM has it attached. While
  Pods use the PVC the controller returns FailedPrecondition, which the resizer
  treats as offline expansion (`only supports offline expansion and volume is
  currently in-use`); once those Pods are deleted the expansion completes, and
  Pods started afterwards see the new size.
- A ZStack snapshot belongs to its volume and is removed with it, so
  DeleteVolume fails with FailedPrecondition while the volume has snapshots.
  Delete the VolumeSnapshots first.
- zstack-sdk-go-v2 v0.0.10 drops ZStack's error text when an asynchronous API
  call fails, so Kubernetes events show only `StatusCode: 503`; the reason is
  in the ZStack management node log.
- The image is built for x86_64 only.

## Tested with

| Component | Version |
|---|---|
| ZStack | Cloud 5.5.38.869; SharedBlock primary storage over iSCSI, and Ceph primary storage on ZStone 5.5.12 |
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
changes), least-privilege IAM2 AccessKey, and full cleanup. On Ceph primary
storage, csi-sanity and the same volume checks passed with an admin AccessKey;
the differences are listed under Known limitations.

Not tested yet:

- Node operating systems other than Ubuntu 24.04, such as Kylin V10, openEuler
  or other distributions with 4.19 kernels.
- Kubernetes versions other than 1.35, and arm64 nodes.
- ZStack versions other than 5.5.38, primary storage other than SharedBlock and
  Ceph (NFS, local storage and others), and Ceph clusters other than ZStone.
- Controller failover under load, large numbers of volumes or nodes, and
  upgrading the driver while volumes are in use.

## Development

```sh
make test
make build
```
