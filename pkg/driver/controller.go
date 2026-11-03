package driver

import (
	"context"
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

var provisioningStrategies = map[string]string{
	"thin":  "ThinProvisioning",
	"thick": "ThickProvisioning",
}

var controllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
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
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
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
	var systemTags []string
	if p := params[ParamProvisioning]; p != "" {
		strategy, ok := provisioningStrategies[strings.ToLower(p)]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "%s must be thin or thick, got %q", ParamProvisioning, p)
		}
		systemTags = append(systemTags, zstack.ProvisioningTag(strategy))
	}
	source := req.GetVolumeContentSource()
	if source.GetVolume() != nil {
		return nil, status.Error(codes.InvalidArgument, "volume cloning is not supported")
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
		if vol.Size < size || (limit > 0 && vol.Size > limit) {
			return nil, status.Errorf(codes.AlreadyExists, "volume %s already exists with size %d", name, vol.Size)
		}
		return createVolumeResponse(vol, source), nil
	case !zstack.IsNotFound(err):
		return nil, status.Errorf(codes.Internal, "get volume %s: %v", uuid, err)
	}

	if snap := source.GetSnapshot(); snap != nil {
		vol, err = cs.createVolumeFromSnapshot(uuid, name, snap.GetSnapshotId(), psUUID, size)
		if err != nil {
			return nil, err
		}
	} else {
		vol, err = cs.cloud.CreateVolume(uuid, name, size, psUUID, systemTags)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "create volume %s: %v", name, err)
		}
	}
	return createVolumeResponse(vol, source), nil
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
	if vol.VmInstanceUuid != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to VM %s", id, vol.VmInstanceUuid)
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
	switch vol.VmInstanceUuid {
	case nodeID:
		return &csi.ControllerPublishVolumeResponse{}, nil
	case "":
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to VM %s", volID, vol.VmInstanceUuid)
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
	attachedTo := vol.VmInstanceUuid
	if attachedTo == "" || (nodeID != "" && attachedTo != nodeID) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if err := cs.cloud.DetachVolume(volID, attachedTo); err != nil {
		return nil, status.Errorf(codes.Internal, "detach volume %s from VM %s: %v", volID, attachedTo, err)
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
	if _, err := cs.getLiveVolume(volID); err != nil {
		return nil, err
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
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
