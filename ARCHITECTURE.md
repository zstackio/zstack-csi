# zstack-csi 对接方式说明

zstack-csi 是标准的 CSI 驱动，K8s 侧的用法与其他 CSI 驱动相同：StorageClass、PVC、快照、扩容都按 K8s 的标准方式使用。它和 XSKY、Ceph、ZBS 等存储自带的 CSI 不同的地方在于：**建盘时调用的是谁的接口，以及盘怎样到达节点**。

zstack-csi 属于“云平台模式”，与 AWS EBS CSI、OpenStack Cinder CSI、vSphere CSI 是同一类；XSKY、Ceph（ceph-csi）、ZBS CSI 属于“存储原生模式”。

## 数据路径

```
zstack-csi（云平台模式）
  K8s controller ──ZStack API──> ZStack 管理节点：建数据云盘，挂到节点云主机
  节点云主机 <──virtio── 物理机 QEMU <──iSCSI/FC── SharedBlock 存储（Ceph 主存储时 QEMU 经 RBD 访问 Ceph）
  节点只看到多了一块本地盘，不直接连接存储

XSKY / Ceph / ZBS（存储原生模式）
  K8s controller ──存储 API──> 存储系统：建 LUN 或 RBD 镜像
  节点 ──iSCSI / rbd / nbd 经网络──> 存储集群
  节点自己登录存储、映射设备
```

## 一块卷从创建到挂进 Pod

| 阶段 | CSI 调用 | zstack-csi 做的事 |
|---|---|---|
| 建卷 | CreateVolume | 在 StorageClass 指定的主存储上建 ZStack 数据云盘，名字是 PV 名（`pvc-…`） |
| 挂到节点 | ControllerPublishVolume | 确认节点云主机存在，调用 ZStack 把云盘挂到这台云主机 |
| 节点上准备 | NodeStageVolume | 按磁盘序列号找到新盘，文件系统卷格式化（ext4 或 xfs）并挂到暂存目录 |
| 给 Pod | NodePublishVolume | 把暂存目录（或块设备）绑定挂载进 Pod |
| 卸载、删除 | 以上的逆过程 | 从云主机卸下云盘；删除时彻底删除 ZStack 云盘 |

ZStack 把每块云盘的 UUID 设为磁盘序列号，所以节点上可以按 UUID 找到对应的盘：virtio-blk 盘在 `/dev/disk/by-id/virtio-<UUID 前 20 位>`，virtio-scsi 盘（共享卷）在 `/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_<UUID>`。

## K8s 怎么知道节点是哪台云主机

K8s 本身不知道 ZStack，节点和云主机的对应关系是节点插件上报的：

1. ZStack 创建云主机时，用云主机 UUID 作为 libvirt 虚拟机的 UUID，QEMU 再把它作为 SMBIOS 系统 UUID 交给虚拟机。
2. 虚拟机里的 Linux 把它放在 `/sys/class/dmi/id/product_uuid`，只有 root 能读，格式带横杠，例如 `8dd384fe-145f-4d3d-8123-07b7297de754`。
3. 节点插件以特权容器运行在每个节点上，启动时读这个文件，去掉横杠、转成小写，得到 ZStack 格式的云主机 UUID `8dd384fe145f4d3d812307b7297de754`，作为 CSI 的节点 ID。启动参数 `--node-id` 可以覆盖，一般不用。
4. kubelet 通过 node-driver-registrar 拿到节点 ID，写进这个节点的 CSINode 对象（`spec.drivers[].nodeID`）和 Node 注解 `csi.volume.kubernetes.io/nodeid`。可以用 `kubectl get csinode` 查看。
5. Pod 调度到某个节点后，csi-attacher 从 CSINode 取出节点 ID，调用 ControllerPublishVolume。controller 用 AccessKey 向 ZStack 查询这台云主机，查不到就报 “node VM … not found”，查到才挂盘。

因此有两点要求：

- K8s 节点必须是 ZStack 云主机。物理机或其他平台上的虚拟机读到的 UUID 在 ZStack 里不存在，挂盘会失败。
- AccessKey 所属的身份必须能看到并操作这些云主机。使用 IAM2 最小权限时，要把节点云主机划给项目，见 README 的 “ZStack permissions”。

## 与存储原生 CSI 的对比

| | zstack-csi | XSKY / Ceph / ZBS CSI |
|---|---|---|
| controller 调用 | ZStack API（AccessKey） | 存储系统 API |
| 盘怎样到达节点 | ZStack 热插到云主机 | 节点自己登录存储（iSCSI 登录、rbd map、nbd 连接） |
| 对节点的要求 | 必须是 ZStack 云主机；节点上不用装存储客户端，也不用连通存储网络 | 云主机或物理机都可以，但要能访问存储网络，并装好 iscsiadm、multipath、rbd 等客户端 |
| 凭据 | 只有一个 ZStack AccessKey，可用 IAM2 最小权限；存储凭据不进 K8s | 存储账号或 CHAP、Ceph key 放在 K8s Secret 和节点上 |
| 在 ZStack 中的可见性 | 卷就是 ZStack 数据云盘（`pvc-…`），可在 ZStack 里查看、计费、监控 | ZStack 看不到这些卷 |
| 性能 | 多一层虚拟化（virtio），与普通云主机数据盘相同 | 节点直连存储，路径更短 |
| 多节点读写（RWX） | 使用 ZStack 共享云盘，只支持 Block 模式；SharedBlock 上不能扩容，Ceph 上要先删掉使用它的 Pod 才能扩容 | 存储原生多点映射；文件存储（NFS、CephFS）还能提供 RWX 文件系统 |
| 快照、克隆 | ZStack 云盘快照：快照属于源卷，有快照时不能删除源卷 | 存储原生快照，克隆卷通常可以与源卷脱离 |
| 每节点卷数 | 受云主机磁盘槽位限制，默认上报 24 | 受 iSCSI 会话数和设备数限制 |
| 适用范围 | 只能在 ZStack 云主机里用，与 EBS CSI 只能在 EC2 上用一样 | 只要网络能通存储就能用 |

## 为什么 SharedBlock 只能用这种方式

SharedBlock 是只有物理机能看到的 SAN LUN，ZStack 在上面建共享卷组，云主机没有到存储的网络路径，所以无法让节点直连存储，只能通过 ZStack API 由物理机把盘交给云主机。

XSKY、Ceph、ZBS 这类云主机能经网络访问的分布式存储，两种方式都可以选：

- 用存储厂商的 CSI：数据路径更短，但卷不受 ZStack 管理，节点要能访问存储网络并持有存储凭据。
- 用 zstack-csi：卷由 ZStack 统一管理，节点不需要访问存储网络。

zstack-csi 只调用 ZStack 通用的数据云盘接口，目前在 SharedBlock 和 Ceph（ZStone）主存储上测试过，两者在置备方式和共享卷扩容上的差异见 README 的 Known limitations。NFS、本地存储等其他主存储原理上也适用，列在 README 的未测试范围里。
