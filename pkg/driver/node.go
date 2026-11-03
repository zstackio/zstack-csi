package driver

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

const (
	defaultFsType = "ext4"
	dmiUUIDPath   = "/sys/class/dmi/id/product_uuid"
)

// formatOptions keeps new filesystems mountable by older node kernels: XFS
// bigtime and inobtcount, enabled by default in recent xfsprogs, need Linux 5.10.
var formatOptions = map[string][]string{
	"xfs": {"-m", "bigtime=0,inobtcount=0"},
}

var nodeCapabilities = []csi.NodeServiceCapability_RPC_Type{
	csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
	csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
	csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
}

type nodeServer struct {
	csi.UnimplementedNodeServer
	nodeID     string
	maxVolumes int64
	mounter    *mount.SafeFormatAndMount
	resizer    *mount.ResizeFs
	devices    *deviceFinder
	locks      *keyLocks
}

func newNodeServer(nodeID string, maxVolumes int64) (*nodeServer, error) {
	if nodeID == "" {
		id, err := vmUUIDFromDMI(dmiUUIDPath)
		if err != nil {
			return nil, err
		}
		nodeID = id
	}
	exec := utilexec.New()
	return &nodeServer{
		nodeID:     nodeID,
		maxVolumes: maxVolumes,
		mounter:    &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: exec},
		resizer:    mount.NewResizeFs(exec),
		devices:    newDeviceFinder(),
		locks:      newKeyLocks(),
	}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volID, staging, vc := req.GetVolumeId(), req.GetStagingTargetPath(), req.GetVolumeCapability()
	if volID == "" || staging == "" || vc == nil {
		return nil, status.Error(codes.InvalidArgument, "volume ID, staging target path and volume capability are required")
	}
	if err := validateCapabilities([]*csi.VolumeCapability{vc}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !ns.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer ns.locks.Release(volID)

	device, err := ns.devices.Find(ctx, volID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find disk of volume %s: %v", volID, err)
	}
	// Raw block volumes are bind mounted straight from the device at publish.
	if vc.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	if err := os.MkdirAll(staging, 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mounted, err := ns.mounter.IsMountPoint(staging)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check mount point %s: %v", staging, err)
	}
	if mounted {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	fsType := vc.GetMount().GetFsType()
	if fsType == "" {
		fsType = defaultFsType
	}
	options := vc.GetMount().GetMountFlags()
	if err := ns.mounter.FormatAndMountSensitiveWithFormatOptions(device, staging, fsType, options, nil, formatOptions[fsType]); err != nil {
		return nil, status.Errorf(codes.Internal, "format and mount %s at %s: %v", device, staging, err)
	}
	// A volume restored from a smaller snapshot is larger than its filesystem.
	if need, err := ns.resizer.NeedResize(device, staging); err != nil {
		klog.Warningf("check whether %s needs resize: %v", device, err)
	} else if need {
		if _, err := ns.resizer.Resize(device, staging); err != nil {
			return nil, status.Errorf(codes.Internal, "resize filesystem on %s: %v", device, err)
		}
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volID, staging := req.GetVolumeId(), req.GetStagingTargetPath()
	if volID == "" || staging == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging target path are required")
	}
	if !ns.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer ns.locks.Release(volID)

	if err := mount.CleanupMountPoint(staging, ns.mounter, true); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount %s: %v", staging, err)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volID, staging, target, vc := req.GetVolumeId(), req.GetStagingTargetPath(), req.GetTargetPath(), req.GetVolumeCapability()
	if volID == "" || staging == "" || target == "" || vc == nil {
		return nil, status.Error(codes.InvalidArgument, "volume ID, staging target path, target path and volume capability are required")
	}
	if err := validateCapabilities([]*csi.VolumeCapability{vc}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !ns.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer ns.locks.Release(volID)

	options := []string{"bind"}
	if req.GetReadonly() || isReadOnly(vc) {
		options = append(options, "ro")
	}

	source := staging
	if vc.GetBlock() != nil {
		device, err := ns.devices.Find(ctx, volID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "find disk of volume %s: %v", volID, err)
		}
		source = device
		if err := createFile(target); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		if err := os.MkdirAll(target, 0o750); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		options = append(options, vc.GetMount().GetMountFlags()...)
	}

	mounted, err := ns.mounter.IsMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check mount point %s: %v", target, err)
	}
	if mounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if err := ns.mounter.Mount(source, target, "", options); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount %s at %s: %v", source, target, err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func createFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volID, target := req.GetVolumeId(), req.GetTargetPath()
	if volID == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}
	if !ns.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer ns.locks.Release(volID)

	if err := mount.CleanupMountPoint(target, ns.mounter, true); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volID, volumePath := req.GetVolumeId(), req.GetVolumePath()
	if volID == "" || volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and volume path are required")
	}
	if _, err := os.Stat(volumePath); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume path %s not found", volumePath)
	}
	if !ns.locks.TryAcquire(volID) {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volID)
	}
	defer ns.locks.Release(volID)

	device, err := ns.devices.Find(ctx, volID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find disk of volume %s: %v", volID, err)
	}
	if err := ns.devices.rescan(device); err != nil {
		return nil, status.Errorf(codes.Internal, "rescan %s: %v", device, err)
	}
	if req.GetVolumeCapability().GetBlock() != nil {
		return &csi.NodeExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
	}
	mountPath := req.GetStagingTargetPath()
	if mountPath == "" {
		mountPath = volumePath
	}
	if _, err := ns.resizer.Resize(device, mountPath); err != nil {
		return nil, status.Errorf(codes.Internal, "resize filesystem on %s: %v", device, err)
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
}

func (ns *nodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volID, volumePath := req.GetVolumeId(), req.GetVolumePath()
	if volID == "" || volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and volume path are required")
	}
	info, err := os.Stat(volumePath)
	if os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume path %s not found", volumePath)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if !info.IsDir() {
		size, err := blockDeviceSize(volumePath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get size of %s: %v", volumePath, err)
		}
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: size},
		}}, nil
	}

	var st unix.Statfs_t
	if err := unix.Statfs(volumePath, &st); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs %s: %v", volumePath, err)
	}
	bsize := int64(st.Bsize)
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{
			Unit:      csi.VolumeUsage_BYTES,
			Total:     int64(st.Blocks) * bsize,
			Available: int64(st.Bavail) * bsize,
			Used:      int64(st.Blocks-st.Bfree) * bsize,
		},
		{
			Unit:      csi.VolumeUsage_INODES,
			Total:     int64(st.Files),
			Available: int64(st.Ffree),
			Used:      int64(st.Files - st.Ffree),
		},
	}}, nil
}

func blockDeviceSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}

func (ns *nodeServer) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := make([]*csi.NodeServiceCapability, 0, len(nodeCapabilities))
	for _, c := range nodeCapabilities {
		caps = append(caps, &csi.NodeServiceCapability{Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{Type: c},
		}})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (ns *nodeServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: ns.nodeID, MaxVolumesPerNode: ns.maxVolumes}, nil
}
