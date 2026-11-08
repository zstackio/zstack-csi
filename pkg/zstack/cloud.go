// Package zstack wraps the ZStack Cloud API calls used by the CSI controller.
package zstack

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/zstackio/zstack-sdk-go-v2/pkg/client"
	zserrors "github.com/zstackio/zstack-sdk-go-v2/pkg/errors"
	"github.com/zstackio/zstack-sdk-go-v2/pkg/param"
	"github.com/zstackio/zstack-sdk-go-v2/pkg/util/httputils"
	"github.com/zstackio/zstack-sdk-go-v2/pkg/view"
	"k8s.io/klog/v2"
)

const (
	VolumeStatusDeleted = "Deleted"
	SnapshotStatusReady = "Ready"
	SharedBlockType     = "SharedBlock"
	LocalStorageType    = "LocalStorage"
	AddonType           = "Addon"
	defaultPort         = 8080
	apiContextPath      = "zstack"
)

// VolumeStatusNotInstantiated is the status of a volume that ZStack failed to
// create on its primary storage; attaching it would create it on storage of
// ZStack's choice.
const VolumeStatusNotInstantiated = "NotInstantiated"

// System tags the ZStack UI sets on a shared data volume; SharedBlock only
// creates shared volumes thick provisioned.
var SharedVolumeTags = []string{"ephemeral::shareable", "capability::virtio-scsi"}

// Primary storage types ZStack creates shared volumes on.
var SharedVolumeStorageTypes = []string{"Ceph", SharedBlockType, AddonType}

type (
	Volume   = view.VolumeInventoryView
	Snapshot = view.VolumeSnapshotInventoryView
)

type Cloud struct {
	cli *client.ZSClient
}

// NewFromEnv builds a client authenticated with a ZStack AccessKey taken from
// ZSTACK_HOST, ZSTACK_PORT, ZSTACK_ACCESS_KEY_ID and ZSTACK_ACCESS_KEY_SECRET.
func NewFromEnv() (*Cloud, error) {
	host := os.Getenv("ZSTACK_HOST")
	id := os.Getenv("ZSTACK_ACCESS_KEY_ID")
	secret := os.Getenv("ZSTACK_ACCESS_KEY_SECRET")
	if host == "" || id == "" || secret == "" {
		return nil, fmt.Errorf("ZSTACK_HOST, ZSTACK_ACCESS_KEY_ID and ZSTACK_ACCESS_KEY_SECRET must be set")
	}
	port := defaultPort
	if p := os.Getenv("ZSTACK_PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid ZSTACK_PORT %q: %w", p, err)
		}
		port = v
	}
	cfg := client.NewZSConfig(host, port, apiContextPath).AccessKey(id, secret)
	return &Cloud{cli: client.NewZSClient(cfg)}, nil
}

func IsNotFound(err error) bool {
	return err != nil && zserrors.Cause(err) == zserrors.ErrNotFound
}

func isForbidden(err error) bool {
	var ce *httputils.JSONClientError
	return errors.As(err, &ce) && ce.Code == http.StatusForbidden
}

// check exits when ZStack starts rejecting the AccessKey signature, typically
// because the management node's UTC offset changed, so that the restarted
// controller detects the time zone again.
func check(err error) error {
	if isForbidden(err) {
		klog.Fatalf("ZStack rejected the AccessKey request, restarting to detect the management node time zone again: %v", err)
	}
	return err
}

func ProvisioningTag(strategy string) string {
	return "volumeProvisioningStrategy::" + strategy
}

func (c *Cloud) Version() (string, error) {
	v, err := c.cli.GetVersion()
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

func (c *Cloud) GetVolume(uuid string) (*Volume, error) {
	v, err := c.cli.GetVolume(uuid)
	return v, check(err)
}

func (c *Cloud) CreateVolume(uuid, name string, size int64, primaryStorageUUID string, systemTags []string) (*Volume, error) {
	v, err := c.cli.CreateDataVolume(param.CreateDataVolumeParam{
		BaseParam: param.BaseParam{SystemTags: systemTags},
		Params: param.CreateDataVolumeParamDetail{
			Name:               name,
			DiskSize:           &size,
			PrimaryStorageUuid: &primaryStorageUUID,
			ResourceUuid:       &uuid,
		},
	})
	return v, check(err)
}

func (c *Cloud) CreateVolumeFromSnapshot(uuid, name, snapshotUUID, primaryStorageUUID string) (*Volume, error) {
	v, err := c.cli.CreateDataVolumeFromVolumeSnapshot(snapshotUUID, param.CreateDataVolumeFromVolumeSnapshotParam{
		Params: param.CreateDataVolumeFromVolumeSnapshotParamDetail{
			Name:               name,
			PrimaryStorageUuid: &primaryStorageUUID,
			ResourceUuid:       &uuid,
		},
	})
	return v, check(err)
}

// DeleteVolume deletes and expunges a data volume so its space is released
// whatever the volume deletion policy is.
func (c *Cloud) DeleteVolume(uuid string) error {
	vol, err := c.cli.GetVolume(uuid)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return check(err)
	}
	if vol.Status != VolumeStatusDeleted {
		if err := c.cli.DeleteDataVolume(uuid, param.DeleteModePermissive); err != nil {
			return check(err)
		}
		if _, err := c.cli.GetVolume(uuid); IsNotFound(err) {
			return nil
		}
	}
	return check(c.cli.ExpungeDataVolume(uuid))
}

// FlattenVolume copies into a volume the data it shares with the snapshot it
// was created from, so that the snapshot can be deleted.
func (c *Cloud) FlattenVolume(uuid string) error {
	_, err := c.cli.FlattenVolume(uuid, param.FlattenVolumeParam{})
	return check(err)
}

func (c *Cloud) ResizeVolume(uuid string, size int64) (*Volume, error) {
	v, err := c.cli.ResizeDataVolume(uuid, param.ResizeDataVolumeParam{
		Params: param.ResizeDataVolumeParamDetail{Size: size},
	})
	return v, check(err)
}

func (c *Cloud) GetVM(uuid string) (*view.VmInstanceInventoryView, error) {
	v, err := c.cli.GetVmInstance(uuid)
	return v, check(err)
}

func (c *Cloud) AttachVolume(volumeUUID, vmUUID string) error {
	_, err := c.cli.AttachDataVolumeToVm(volumeUUID, vmUUID, param.AttachDataVolumeToVmParam{})
	return check(err)
}

func (c *Cloud) DetachVolume(volumeUUID, vmUUID string) error {
	_, err := c.cli.DetachDataVolumeFromVm(volumeUUID, param.DetachDataVolumeFromVmParam{
		Params: param.DetachDataVolumeFromVmParamDetail{VmUuid: &vmUUID},
	})
	return check(err)
}

// AttachedVMs returns the VMs a volume is attached to. A shared volume records
// its VMs separately from the volume itself.
func (c *Cloud) AttachedVMs(vol *Volume) ([]string, error) {
	if !vol.IsShareable {
		if vol.VmInstanceUuid == "" {
			return nil, nil
		}
		return []string{vol.VmInstanceUuid}, nil
	}
	q := param.NewQueryParam()
	q.AddQ("volumeUuid=" + vol.UUID)
	refs, err := c.cli.QueryShareableVolumeVmInstanceRef(&q)
	if err != nil {
		return nil, check(err)
	}
	vms := make([]string, 0, len(refs))
	for _, r := range refs {
		vms = append(vms, r.VmInstanceUuid)
	}
	return vms, nil
}

func (c *Cloud) PrimaryStorageType(uuid string) (string, error) {
	q := param.NewQueryParam()
	q.AddQ("uuid=" + uuid)
	ps, err := c.cli.QueryPrimaryStorage(&q)
	if err != nil {
		return "", check(err)
	}
	if len(ps) == 0 {
		return "", fmt.Errorf("primary storage %s not found", uuid)
	}
	return ps[0].Type, nil
}

func (c *Cloud) GetSnapshot(uuid string) (*Snapshot, error) {
	s, err := c.cli.GetVolumeSnapshot(uuid)
	return s, check(err)
}

func (c *Cloud) ListSnapshots(volumeUUID string) ([]Snapshot, error) {
	q := param.NewQueryParam()
	q.AddQ("volumeUuid=" + volumeUUID)
	s, err := c.cli.QueryVolumeSnapshot(&q)
	return s, check(err)
}

func (c *Cloud) CreateSnapshot(uuid, name, volumeUUID string) (*Snapshot, error) {
	s, err := c.cli.CreateVolumeSnapshot(volumeUUID, param.CreateVolumeSnapshotParam{
		Params: param.CreateVolumeSnapshotParamDetail{Name: name, ResourceUuid: &uuid},
	})
	return s, check(err)
}

func (c *Cloud) DeleteSnapshot(uuid string) error {
	err := c.cli.DeleteVolumeSnapshot(uuid, param.DeleteModePermissive)
	if IsNotFound(err) {
		return nil
	}
	return check(err)
}
