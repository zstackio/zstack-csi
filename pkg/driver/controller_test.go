package driver

import (
	"slices"
	"testing"
)

func TestVolumeTags(t *testing.T) {
	const shareable, scsi = "ephemeral::shareable", "capability::virtio-scsi"
	const thin, thick = "volumeProvisioningStrategy::ThinProvisioning", "volumeProvisioningStrategy::ThickProvisioning"
	cases := []struct {
		name         string
		provisioning string
		shared       bool
		psType       string
		want         []string
		wantFail     bool
	}{
		{"single-node default", "", false, "SharedBlock", nil, false},
		{"single-node thin", "thin", false, "SharedBlock", []string{thin}, false},
		{"single-node thick", "Thick", false, "NFS", []string{thick}, false},
		{"unknown strategy", "eager", false, "Ceph", nil, true},
		{"SharedBlock shared default", "", true, "SharedBlock", []string{shareable, scsi, thick}, false},
		{"SharedBlock shared thick", "thick", true, "SharedBlock", []string{shareable, scsi, thick}, false},
		{"SharedBlock shared thin", "thin", true, "SharedBlock", nil, true},
		{"Ceph shared default", "", true, "Ceph", []string{shareable, scsi}, false},
		{"Addon shared thin", "thin", true, "Addon", []string{shareable, scsi, thin}, false},
		{"NFS shared", "", true, "NFS", nil, true},
		{"SharedMountPoint shared", "", true, "SharedMountPoint", nil, true},
		{"local storage single-node", "", false, "LocalStorage", nil, true},
		{"local storage shared", "", true, "LocalStorage", nil, true},
	}
	for _, c := range cases {
		got, err := volumeTags(c.provisioning, c.shared, c.psType)
		if (err != nil) != c.wantFail || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v, fail=%v", c.name, got, err, c.want, c.wantFail)
		}
	}
}
