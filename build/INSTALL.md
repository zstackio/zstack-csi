# Installing zstack-csi @VERSION@ offline

The package holds:

| Path | Content |
|---|---|
| `zstack-csi-images-@VERSION@.tar` | Driver, CSI sidecar and snapshot-controller images, listed in `images.txt` |
| `zstack-csi/` | Helm chart |
| `snapshot/` | VolumeSnapshot CRDs and snapshot-controller matching the bundled csi-snapshotter |
| `examples/` | Sample PVCs and pods |
| `README.md` | Driver documentation, including the ZStack permissions it needs |
| `ARCHITECTURE.md` | How the driver connects to ZStack and how it differs from storage-native CSI drivers (in Chinese) |

Kubernetes nodes must be ZStack VMs; see the requirements in `README.md`.

## 1. Make the images available

Push them to your registry under their original names, then set `imgurl` to
the registry prefix:

```sh
docker load -i zstack-csi-images-@VERSION@.tar      # or: nerdctl load -i ...
REGISTRY=harbor.example.com/k8s/
while read -r img; do
  docker tag "$img" "$REGISTRY$img" && docker push "$REGISTRY$img"
done < images.txt
```

Without a registry, import the archive on every node instead and leave
`imgurl` empty:

```sh
ctr -n k8s.io images import zstack-csi-images-@VERSION@.tar
```

## 2. Install the snapshot components

Skip this step if the cluster already has them:

```sh
kubectl get crd volumesnapshots.snapshot.storage.k8s.io
```

Otherwise, after adding your registry prefix to the image in
`snapshot/setup-snapshot-controller.yaml` if you pushed the images:

```sh
kubectl apply -f snapshot/
```

## 3. Install the driver

Create an AccessKey as described under "ZStack permissions" in `README.md`,
then:

```sh
kubectl -n kube-system create secret generic zstack-csi-accesskey \
  --from-literal=accessKeyId=<id> --from-literal=accessKeySecret=<secret>
helm install zstack-csi ./zstack-csi -n kube-system \
  --set imgurl=<registry prefix, or empty> \
  --set zstack.host=<management node IP> \
  --set zstack.existingSecret=zstack-csi-accesskey \
  --set storageClass.primaryStorageUuid=<primary storage UUID> \
  --set volumeSnapshotClass.create=true
```

Check that every pod runs and every node registered the driver:

```sh
kubectl -n kube-system get pods -l 'app in (zstack-csi-controller,zstack-csi-node)'
kubectl get csinode -o custom-columns=NODE:.metadata.name,DRIVERS:.spec.drivers[*].name
```

## Upgrade and uninstall

Upgrade with `helm upgrade zstack-csi ./zstack-csi -n kube-system --reuse-values`
from the new package. Before `helm uninstall zstack-csi -n kube-system`,
delete the PVCs of this driver; volumes left behind can no longer be detached
or deleted from Kubernetes.
