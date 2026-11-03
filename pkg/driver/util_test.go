package driver

import (
	"regexp"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// The pattern ZStack's API mediator applies to resourceUuid.
var zstackUUID = regexp.MustCompile(`^[0-9a-f]{8}[0-9a-f]{4}[1-5][0-9a-f]{3}[89ab][0-9a-f]{3}[0-9a-f]{12}$`)

func TestResourceUUID(t *testing.T) {
	a := resourceUUID("volume", "pvc-1")
	if !zstackUUID.MatchString(a) {
		t.Fatalf("%s is rejected by ZStack", a)
	}
	if a != resourceUUID("volume", "pvc-1") {
		t.Fatal("same name must give the same UUID")
	}
	if a == resourceUUID("snapshot", "pvc-1") || a == resourceUUID("volume", "pvc-2") {
		t.Fatal("different kinds or names must give different UUIDs")
	}
}

func TestVolumeSize(t *testing.T) {
	cases := []struct {
		name     string
		r        *csi.CapacityRange
		want     int64
		wantFail bool
	}{
		{"default", nil, GiB, false},
		{"round up", &csi.CapacityRange{RequiredBytes: GiB + 1}, 2 * GiB, false},
		{"exact", &csi.CapacityRange{RequiredBytes: 10 * GiB, LimitBytes: 10 * GiB}, 10 * GiB, false},
		{"limit only", &csi.CapacityRange{LimitBytes: 5 * GiB}, GiB, false},
		{"no whole GiB in range", &csi.CapacityRange{RequiredBytes: GiB + 1, LimitBytes: GiB + 2}, 0, true},
		{"required above limit", &csi.CapacityRange{RequiredBytes: 2 * GiB, LimitBytes: GiB}, 0, true},
	}
	for _, c := range cases {
		got, err := volumeSize(c.r)
		if (err != nil) != c.wantFail || got != c.want {
			t.Errorf("%s: got %d, %v; want %d, fail=%v", c.name, got, err, c.want, c.wantFail)
		}
	}
}

func TestValidateCapabilities(t *testing.T) {
	mountCap := func(mode csi.VolumeCapability_AccessMode_Mode, fs string) *csi.VolumeCapability {
		return &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fs}},
		}
	}
	blockCap := func(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
		return &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		}
	}
	ok := [][]*csi.VolumeCapability{
		{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "")},
		{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY, "xfs")},
		{blockCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		{blockCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
	}
	bad := [][]*csi.VolumeCapability{
		nil,
		{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, "ext4")},
		{mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, "ext4")},
		{mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, "ext4")},
		{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "ntfs")},
		{{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}},
	}
	for i, caps := range ok {
		if err := validateCapabilities(caps); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	for i, caps := range bad {
		if validateCapabilities(caps) == nil {
			t.Errorf("bad[%d] was accepted", i)
		}
	}
}
