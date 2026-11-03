package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ZStack sets each data disk's serial to the volume UUID. virtio-blk exposes at
// most 20 bytes of it; virtio-scsi exposes the whole value.
const virtioBlkSerialLen = 20

var errDeviceNotFound = errors.New("device not found")

type deviceFinder struct {
	devDir   string
	sysDir   string
	timeout  time.Duration
	interval time.Duration
}

func newDeviceFinder() *deviceFinder {
	return &deviceFinder{devDir: "/dev", sysDir: "/sys", timeout: 60 * time.Second, interval: time.Second}
}

// Find waits until the disk of the given volume shows up and returns its
// device path. A hot-plugged disk can appear a moment after the attach call.
func (f *deviceFinder) Find(ctx context.Context, volumeID string) (string, error) {
	deadline := time.Now().Add(f.timeout)
	for {
		path, err := f.lookup(volumeID)
		if err == nil || !errors.Is(err, errDeviceNotFound) || time.Now().After(deadline) {
			return path, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.interval):
		}
	}
}

func (f *deviceFinder) lookup(volumeID string) (string, error) {
	short := volumeID
	if len(short) > virtioBlkSerialLen {
		short = short[:virtioBlkSerialLen]
	}
	for _, link := range []string{"virtio-" + short, "scsi-0QEMU_QEMU_HARDDISK_" + volumeID} {
		path, err := filepath.EvalSymlinks(filepath.Join(f.devDir, "disk", "by-id", link))
		if err == nil {
			return path, nil
		}
	}

	// Fall back to sysfs when udev has not created the by-id links.
	blockDir := filepath.Join(f.sysDir, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", blockDir, err)
	}
	for _, e := range entries {
		if serial, err := os.ReadFile(filepath.Join(blockDir, e.Name(), "serial")); err == nil &&
			strings.TrimSpace(string(serial)) == short {
			return filepath.Join(f.devDir, e.Name()), nil
		}
		if vpd, err := os.ReadFile(filepath.Join(blockDir, e.Name(), "device", "vpd_pg80")); err == nil &&
			bytes.Contains(vpd, []byte(volumeID)) {
			return filepath.Join(f.devDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("volume %s: %w", volumeID, errDeviceNotFound)
}

// rescan makes a SCSI disk pick up a new capacity after the volume is resized;
// virtio-blk disks are updated by the hypervisor and have no rescan entry.
func (f *deviceFinder) rescan(devicePath string) error {
	entry := filepath.Join(f.sysDir, "class", "block", filepath.Base(devicePath), "device", "rescan")
	if _, err := os.Stat(entry); os.IsNotExist(err) {
		return nil
	}
	return os.WriteFile(entry, []byte("1"), 0o200)
}

// vmUUIDFromDMI reads the ZStack VM UUID, which libvirt exposes as the SMBIOS
// system UUID, in the 32-character form used by the ZStack API.
func vmUUIDFromDMI(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read VM UUID from %s: %w", path, err)
	}
	id := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(string(data)), "-", ""))
	if !isUUID(id) {
		return "", fmt.Errorf("unexpected VM UUID %q in %s", id, path)
	}
	return id, nil
}
