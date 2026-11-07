package driver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/zstackio/zstack-csi/pkg/zstack"
)

// StorageClass parameters.
const (
	ParamPrimaryStorageUUID = "primaryStorageUuid"
	ParamProvisioning       = "provisioning"
)

const (
	thinProvisioning  = "ThinProvisioning"
	thickProvisioning = "ThickProvisioning"
)

var provisioningStrategies = map[string]string{
	"thin":  thinProvisioning,
	"thick": thickProvisioning,
}

var controllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
	csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
}

type controllerServer struct {
	csi.UnimplementedControllerServer
	cloud *zstack.Cloud
	locks *keyLocks
}

func newControllerServer(cloud *zstack.Cloud) *controllerServer {
	return &controllerServer{cloud: cloud, locks: newKeyLocks()}
}

func (cs *controllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	caps := req.GetVolumeCapabilities()
	if err := validateCapabilities(caps); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	size, err := volumeSize(req.GetCapacityRange())
	if err != nil {
		return nil, status.Error(codes.OutOfRange, err.Error())
	}
	params := req.GetParameters()
	psUUID := params[ParamPrimaryStorageUUID]
	if psUUID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "StorageClass parameter %s is required", ParamPrimaryStorageUUID)
	}
	shared := isMultiNode(caps)
	sharedBlock := false
	if shared {
		psType, err := cs.cloud.PrimaryStorageType(psUUID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get primary storage %s: %v", psUUID, err)
		}
		sharedBlock = psType == zstack.SharedBlockType
	}
	systemTags, err := volumeTags(params[ParamProvisioning], shared, sharedBlock)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	source := req.GetVolumeContentSource()
	if shared && source != nil {
		return nil, status.Error(codes.InvalidArgument, "multi-node volumes cannot be created from a snapshot or another volume")
	}

	uuid := resourceUUID("volume", name)
	if !cs.locks.TryAcquire(uuid) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", name)
	}
	defer cs.locks.Release(uuid)

	vol, err := cs.cloud.GetVolume(uuid)
	switch {
	case err == nil:
		if vol.Status == zstack.VolumeStatusDeleted {
			return nil, status.Errorf(codes.Aborted, "volume %s is being deleted", uuid)
		}
		limit := req.GetCapacityRange().GetLimitBytes()
		if vol.Size < size || (limit > 0 && vol.Size > limit) || vol.IsShareable != shared {
			return nil, status.Errorf(codes.AlreadyExists, "volume %s already exists with size %d, shared %t", name, vol.Size, vol.IsShareable)
		}
		if source.GetVolume() != nil {
			if err := cs.deleteCloneSnapshot(name); err != nil {
				return nil, err
			}
		}
		return createVolumeResponse(vol, source), nil
	case !zstack.IsNotFound(err):
		return nil, status.Errorf(codes.Internal, "get volume %s: %v", uuid, err)
	}

	switch {
	case source.GetSnapshot() != nil:
		vol, err = cs.createVolumeFromSnapshot(uuid, name, source.GetSnapshot().GetSnapshotId(), psUUID, size)
	case source.GetVolume() != nil:
		vol, err = cs.cloneVolume(uuid, name, source.GetVolume().GetVolumeId(), psUUID, size)
	default:
		if vol, err = cs.cloud.CreateVolume(uuid, name, size, psUUID, systemTags); err != nil {
			err = status.Errorf(codes.Internal, "create volume %s: %v", name, err)
		}
	}
	if err != nil {
		return nil, err
	}
	return createVolumeResponse(vol, source), nil
}

// volumeTags returns the system tags for a new volume. Multi-node volumes are
// ZStack shared volumes, which SharedBlock only provisions thick.
func volumeTags(provisioning string, shared, sharedBlock bool) ([]string, error) {
	strategy := ""
	if provisioning != "" {
		s, ok := provisioningStrategies[strings.ToLower(provisioning)]
		if !ok {
			return nil, fmt.Errorf("%s must be thin or thick, got %q", ParamProvisioning, provisioning)
		}
		strategy = s
	}
	var tags []string
	if shared {
		if sharedBlock {
			if strategy == thinProvisioning {
				return nil, fmt.Errorf("multi-node volumes are shared volumes, which SharedBlock provisions thick")
			}
			strategy = thickProvisioning
		}
		tags = slices.Clone(zstack.SharedVolumeTags)
	}
	if strategy != "" {
		tags = append(tags, zstack.ProvisioningTag(strategy))
	}
	return tags, nil
}

func (cs *controllerServer) createVolumeFromSnapshot(uuid, name, snapshotID, psUUID string, size int64) (*zstack.Volume, error) {
	if !isUUID(snapshotID) {
		return nil, status.Errorf(codes.NotFound, "snapshot %s not found", snapshotID)
	}
	if _, err := cs.cloud.GetSnapshot(snapshotID); err != nil {
		if zstack.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "snapshot %s not found", snapshotID)
		}
		return nil, status.Errorf(codes.Internal, "get snapshot %s: %v", snapshotID, err)
	}
	vol, err := cs.cloud.CreateVolumeFromSnapshot(uuid, name, snapshotID, psUUID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create volume %s from snapshot %s: %v", name, snapshotID, err)
	}
	if vol.Size < size {
		if vol, err = cs.cloud.ResizeVolume(uuid, size); err != nil {
			return nil, status.Errorf(codes.Internal, "resize volume %s restored from snapshot: %v", uuid, err)
		}
	}
	return vol, nil
}

// cloneVolume copies a volume through a temporary snapshot because ZStack has
// no direct volume copy. A volume created from a snapshot does not depend on
// it, so the snapshot is removed once the copy exists.
func (cs *controllerServer) cloneVolume(uuid, name, sourceID, psUUID string, size int64) (*zstack.Volume, error) {
	if !isUUID(sourceID) {
		return nil, status.Errorf(codes.NotFound, "source volume %s not found", sourceID)
	}
	src, err := cs.getLiveVolume(sourceID)
	if err != nil {
		return nil, err
	}
	if src.IsShareable {
		return nil, status.Errorf(codes.InvalidArgument, "shared volume %s cannot be cloned", sourceID)
	}
	if size < src.Size {
		return nil, status.Errorf(codes.OutOfRange, "requested size %d is smaller than source volume size %d", size, src.Size)
	}
	snapUUID := resourceUUID("clone", name)
	if _, err := cs.cloud.GetSnapshot(snapUUID); zstack.IsNotFound(err) {
		if _, err := cs.cloud.CreateSnapshot(snapUUID, "csi-clone-"+name, sourceID); err != nil {
			return nil, status.Errorf(codes.Internal, "snapshot source volume %s: %v", sourceID, err)
		}
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "get snapshot %s: %v", snapUUID, err)
	}
	vol, err := cs.createVolumeFromSnapshot(uuid, name, snapUUID, psUUID, size)
	if err != nil {
		return nil, err
	}
	if err := cs.deleteCloneSnapshot(name); err != nil {
		return nil, err
	}
	return vol, nil
}

func (cs *controllerServer) deleteCloneSnapshot(name string) error {
	id := resourceUUID("clone", name)
	if _, err := cs.cloud.GetSnapshot(id); zstack.IsNotFound(err) {
		return nil
	} else if err != nil {
		return status.Errorf(codes.Internal, "get snapshot %s: %v", id, err)
	}
	if err := cs.cloud.DeleteSnapshot(id); err != nil {
		return status.Errorf(codes.Internal, "delete temporary clone snapshot %s: %v", id, err)
	}
	return nil
}

func createVolumeResponse(vol *zstack.Volume, source *csi.VolumeContentSource) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      vol.UUID,
		CapacityBytes: vol.Size,
		ContentSource: source,
	}}
}

func (cs *controllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if !isUUID(id) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if !cs.locks.TryAcquire(id) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", id)
	}
	defer cs.locks.Release(id)

	vol, err := cs.cloud.GetVolume(id)
	if zstack.IsNotFound(err) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get volume %s: %v", id, err)
	}
	vms, err := cs.cloud.AttachedVMs(vol)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get VMs of volume %s: %v", id, err)
	}
	if len(vms) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to VMs %v", id, vms)
	}
	// ZStack snapshots belong to their volume and are removed with it, so a
	// volume is kept while VolumeSnapshots still depend on it.
	snaps, err := cs.cloud.ListSnapshots(id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list snapshots of volume %s: %v", id, err)
	}
	if len(snaps) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s still has %d snapshots", id, len(snaps))
	}
	if err := cs.cloud.DeleteVolume(id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete volume %s: %v", id, err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerPublishVolume(_ context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volID, nodeID := req.GetVolumeId(), req.GetNodeId()
	if volID == "" || nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and node ID are required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	if err := validateCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !isUUID(volID) {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", volID)
	}
	if !isUUID(nodeID) {
		return nil, status.Errorf(codes.NotFound, "node %s not found", nodeID)
	}
	if !cs.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer cs.locks.Release(volID)

	vol, err := cs.getLiveVolume(volID)
	if err != nil {
		return nil, err
	}
	if _, err := cs.cloud.GetVM(nodeID); err != nil {
		if zstack.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "node VM %s not found", nodeID)
		}
		return nil, status.Errorf(codes.Internal, "get VM %s: %v", nodeID, err)
	}
	vms, err := cs.cloud.AttachedVMs(vol)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get VMs of volume %s: %v", volID, err)
	}
	if slices.Contains(vms, nodeID) {
		return &csi.ControllerPublishVolumeResponse{}, nil
	}
	if len(vms) > 0 && !vol.IsShareable {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to VM %s", volID, vms[0])
	}
	if err := cs.cloud.AttachVolume(volID, nodeID); err != nil {
		return nil, status.Errorf(codes.Internal, "attach volume %s to VM %s: %v", volID, nodeID, err)
	}
	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(_ context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volID, nodeID := req.GetVolumeId(), req.GetNodeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if !isUUID(volID) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if !cs.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer cs.locks.Release(volID)

	vol, err := cs.cloud.GetVolume(volID)
	if zstack.IsNotFound(err) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get volume %s: %v", volID, err)
	}
	vms, err := cs.cloud.AttachedVMs(vol)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get VMs of volume %s: %v", volID, err)
	}
	// An empty node ID asks to unpublish the volume from every node.
	for _, vm := range vms {
		if nodeID != "" && vm != nodeID {
			continue
		}
		if err := cs.cloud.DetachVolume(volID, vm); err != nil {
			return nil, status.Errorf(codes.Internal, "detach volume %s from VM %s: %v", volID, vm, err)
		}
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if !isUUID(volID) {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", volID)
	}
	vol, err := cs.getLiveVolume(volID)
	if err != nil {
		return nil, err
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}
	if isMultiNode(req.GetVolumeCapabilities()) && !vol.IsShareable {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: "multi-node access needs a shared volume"}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeContext:      req.GetVolumeContext(),
		VolumeCapabilities: req.GetVolumeCapabilities(),
		Parameters:         req.GetParameters(),
	}}, nil
}

func (cs *controllerServer) ControllerExpandVolume(_ context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}
	size, err := volumeSize(req.GetCapacityRange())
	if err != nil {
		return nil, status.Error(codes.OutOfRange, err.Error())
	}
	if !isUUID(volID) {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", volID)
	}
	if !cs.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer cs.locks.Release(volID)

	vol, err := cs.getLiveVolume(volID)
	if err != nil {
		return nil, err
	}
	nodeExpansion := req.GetVolumeCapability().GetBlock() == nil
	if vol.Size >= size {
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: vol.Size, NodeExpansionRequired: nodeExpansion}, nil
	}
	if vol.IsShareable {
		psType, err := cs.cloud.PrimaryStorageType(vol.PrimaryStorageUuid)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get primary storage of volume %s: %v", volID, err)
		}
		if psType == zstack.SharedBlockType {
			return nil, status.Errorf(codes.InvalidArgument, "ZStack cannot resize shared volume %s on SharedBlock primary storage", volID)
		}
		// Other primary storage resizes a shared volume only while no running
		// VM has it attached. The resizer takes FailedPrecondition as offline
		// expansion and retries once no Pod uses the volume.
		vms, err := cs.cloud.AttachedVMs(vol)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get VMs of volume %s: %v", volID, err)
		}
		if len(vms) > 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "ZStack resizes shared volume %s only while it is detached, but it is attached to VMs %v; stop the Pods that use it", volID, vms)
		}
	}
	if vol, err = cs.cloud.ResizeVolume(volID, size); err != nil {
		return nil, status.Errorf(codes.Internal, "resize volume %s: %v", volID, err)
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: vol.Size, NodeExpansionRequired: nodeExpansion}, nil
}

func (cs *controllerServer) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := make([]*csi.ControllerServiceCapability, 0, len(controllerCapabilities))
	for _, c := range controllerCapabilities {
		caps = append(caps, &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
		}})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (cs *controllerServer) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name, volID := req.GetName(), req.GetSourceVolumeId()
	if name == "" || volID == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name and source volume ID are required")
	}
	uuid := resourceUUID("snapshot", name)
	if !cs.locks.TryAcquire(uuid) {
		return nil, status.Errorf(codes.Aborted, "an operation on snapshot %s is in progress", name)
	}
	defer cs.locks.Release(uuid)

	snap, err := cs.cloud.GetSnapshot(uuid)
	switch {
	case err == nil:
		if snap.VolumeUuid != volID {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %s already exists for volume %s", name, snap.VolumeUuid)
		}
	case !zstack.IsNotFound(err):
		return nil, status.Errorf(codes.Internal, "get snapshot %s: %v", uuid, err)
	}
	if !isUUID(volID) {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", volID)
	}
	vol, err := cs.getLiveVolume(volID)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		if snap, err = cs.cloud.CreateSnapshot(uuid, name, volID); err != nil {
			return nil, status.Errorf(codes.Internal, "create snapshot %s of volume %s: %v", name, volID, err)
		}
	}
	return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
		SnapshotId:     snap.UUID,
		SourceVolumeId: volID,
		SizeBytes:      vol.Size,
		CreationTime:   timestamppb.New(snap.CreateDate),
		ReadyToUse:     snap.Status == zstack.SnapshotStatusReady,
	}}, nil
}

func (cs *controllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	id := req.GetSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot ID is required")
	}
	if !isUUID(id) {
		return &csi.DeleteSnapshotResponse{}, nil
	}
	if !cs.locks.TryAcquire(id) {
		return nil, status.Errorf(codes.Aborted, "an operation on snapshot %s is in progress", id)
	}
	defer cs.locks.Release(id)

	if _, err := cs.cloud.GetSnapshot(id); err != nil {
		if zstack.IsNotFound(err) {
			return &csi.DeleteSnapshotResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "get snapshot %s: %v", id, err)
	}
	if err := cs.cloud.DeleteSnapshot(id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete snapshot %s: %v", id, err)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// getLiveVolume returns the volume unless it does not exist or is already
// deleted, which both map to NotFound.
func (cs *controllerServer) getLiveVolume(id string) (*zstack.Volume, error) {
	vol, err := cs.cloud.GetVolume(id)
	if zstack.IsNotFound(err) || (err == nil && vol.Status == zstack.VolumeStatusDeleted) {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", id)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get volume %s: %v", id, err)
	}
	return vol, nil
}
