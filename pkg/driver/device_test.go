package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testVolumeID = "4f1c2b3a5d6e47f8a9b0c1d2e3f4a5b6"

func newTestFinder(t *testing.T) (*deviceFinder, string) {
	root := t.TempDir()
	for _, d := range []string{"dev/disk/by-id", "sys/block"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &deviceFinder{
		devDir:   filepath.Join(root, "dev"),
		sysDir:   filepath.Join(root, "sys"),
		timeout:  50 * time.Millisecond,
		interval: 10 * time.Millisecond,
	}, root
}

func addDevice(t *testing.T, root, name string) string {
	path := filepath.Join(root, "dev", name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindVirtioBlkLink(t *testing.T) {
	f, root := newTestFinder(t)
	want := addDevice(t, root, "vdb")
	if err := os.Symlink("../../vdb", filepath.Join(root, "dev/disk/by-id/virtio-"+testVolumeID[:20])); err != nil {
		t.Fatal(err)
	}
	got, err := f.Find(context.Background(), testVolumeID)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestFindVirtioScsiLink(t *testing.T) {
	f, root := newTestFinder(t)
	want := addDevice(t, root, "sdb")
	if err := os.Symlink("../../sdb", filepath.Join(root, "dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_"+testVolumeID)); err != nil {
		t.Fatal(err)
	}
	got, err := f.Find(context.Background(), testVolumeID)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestFindFromSysfs(t *testing.T) {
	f, root := newTestFinder(t)
	writeFile(t, filepath.Join(root, "sys/block/vda/serial"), "rootdiskserial000000\n")
	writeFile(t, filepath.Join(root, "sys/block/vdc/serial"), testVolumeID[:20]+"\n")

	got, err := f.Find(context.Background(), testVolumeID)
	if want := filepath.Join(root, "dev/vdc"); err != nil || got != want {
		t.Fatalf("virtio: got %q, %v; want %q", got, err, want)
	}
	if err := os.Remove(filepath.Join(root, "sys/block/vdc/serial")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "sys/block/sdd/device/vpd_pg80"), "\x00\x80\x00\x20"+testVolumeID)
	got, err = f.Find(context.Background(), testVolumeID)
	if want := filepath.Join(root, "dev/sdd"); err != nil || got != want {
		t.Fatalf("scsi: got %q, %v; want %q", got, err, want)
	}
}

func TestFindTimesOut(t *testing.T) {
	f, _ := newTestFinder(t)
	if _, err := f.Find(context.Background(), testVolumeID); !errors.Is(err, errDeviceNotFound) {
		t.Fatalf("got %v, want errDeviceNotFound", err)
	}
}

func TestVMUUIDFromDMI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "product_uuid")
	writeFile(t, path, "4F1C2B3A-5D6E-47F8-A9B0-C1D2E3F4A5B6\n")
	got, err := vmUUIDFromDMI(path)
	if err != nil || got != testVolumeID {
		t.Fatalf("got %q, %v; want %q", got, err, testVolumeID)
	}
	writeFile(t, path, "Not Settable\n")
	if _, err := vmUUIDFromDMI(path); err == nil {
		t.Fatal("expected an error for a non-UUID value")
	}
}
