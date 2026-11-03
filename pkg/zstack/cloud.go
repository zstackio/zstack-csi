// Package zstack wraps the ZStack Cloud API calls used by the CSI controller.
package zstack

import (
	"fmt"
	"os"
	"strconv"

	"github.com/zstackio/zstack-sdk-go-v2/pkg/client"
	zserrors "github.com/zstackio/zstack-sdk-go-v2/pkg/errors"
	"github.com/zstackio/zstack-sdk-go-v2/pkg/param"
	"github.com/zstackio/zstack-sdk-go-v2/pkg/view"
)

const (
	VolumeStatusDeleted  = "Deleted"
	SnapshotStatusReady  = "Ready"
	defaultPort          = 8080
	apiContextPath       = "zstack"
	provisioningTagToken = "volumeProvisioningStrategy::"
)

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

// ProvisioningTag returns the system tag selecting thin or thick provisioning.
func ProvisioningTag(strategy string) string {
	return provisioningTagToken + strategy
}

func (c *Cloud) Version() (string, error) {
	v, err := c.cli.GetVersion()
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

func (c *Cloud) GetVolume(uuid string) (*Volume, error) {
	return c.cli.GetVolume(uuid)
}

func (c *Cloud) CreateVolume(uuid, name string, size int64, primaryStorageUUID string, systemTags []string) (*Volume, error) {
	return c.cli.CreateDataVolume(param.CreateDataVolumeParam{
		BaseParam: param.BaseParam{SystemTags: systemTags},
		Params: param.CreateDataVolumeParamDetail{
			Name:               name,
			DiskSize:           &size,
			PrimaryStorageUuid: &primaryStorageUUID,
			ResourceUuid:       &uuid,
		},
	})
}

func (c *Cloud) CreateVolumeFromSnapshot(uuid, name, snapshotUUID, primaryStorageUUID string) (*Volume, error) {
	return c.cli.CreateDataVolumeFromVolumeSnapshot(snapshotUUID, param.CreateDataVolumeFromVolumeSnapshotParam{
		Params: param.CreateDataVolumeFromVolumeSnapshotParamDetail{
			Name:               name,
			PrimaryStorageUuid: &primaryStorageUUID,
			ResourceUuid:       &uuid,
		},
	})
}

// DeleteVolume deletes and expunges a data volume so its space is released
// whatever the volume deletion policy is.
func (c *Cloud) DeleteVolume(uuid string) error {
	vol, err := c.cli.GetVolume(uuid)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if vol.Status != VolumeStatusDeleted {
		if err := c.cli.DeleteDataVolume(uuid, param.DeleteModePermissive); err != nil {
			return err
		}
		if _, err := c.cli.GetVolume(uuid); IsNotFound(err) {
			return nil
		}
	}
	return c.cli.ExpungeDataVolume(uuid)
}

func (c *Cloud) ResizeVolume(uuid string, size int64) (*Volume, error) {
	return c.cli.ResizeDataVolume(uuid, param.ResizeDataVolumeParam{
		Params: param.ResizeDataVolumeParamDetail{Size: size},
	})
}

func (c *Cloud) GetVM(uuid string) (*view.VmInstanceInventoryView, error) {
	return c.cli.GetVmInstance(uuid)
}

func (c *Cloud) AttachVolume(volumeUUID, vmUUID string) error {
	_, err := c.cli.AttachDataVolumeToVm(volumeUUID, vmUUID, param.AttachDataVolumeToVmParam{})
	return err
}

func (c *Cloud) DetachVolume(volumeUUID, vmUUID string) error {
	_, err := c.cli.DetachDataVolumeFromVm(volumeUUID, param.DetachDataVolumeFromVmParam{
		Params: param.DetachDataVolumeFromVmParamDetail{VmUuid: &vmUUID},
	})
	return err
}

func (c *Cloud) GetSnapshot(uuid string) (*Snapshot, error) {
	return c.cli.GetVolumeSnapshot(uuid)
}

func (c *Cloud) ListSnapshots(volumeUUID string) ([]Snapshot, error) {
	q := param.NewQueryParam()
	q.AddQ("volumeUuid=" + volumeUUID)
	return c.cli.QueryVolumeSnapshot(&q)
}

func (c *Cloud) CreateSnapshot(uuid, name, volumeUUID string) (*Snapshot, error) {
	return c.cli.CreateVolumeSnapshot(volumeUUID, param.CreateVolumeSnapshotParam{
		Params: param.CreateVolumeSnapshotParamDetail{Name: name, ResourceUuid: &uuid},
	})
}

func (c *Cloud) DeleteSnapshot(uuid string) error {
	err := c.cli.DeleteVolumeSnapshot(uuid, param.DeleteModePermissive)
	if IsNotFound(err) {
		return nil
	}
	return err
}
