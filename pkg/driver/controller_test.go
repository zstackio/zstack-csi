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
		sharedBlock  bool
		want         []string
		wantFail     bool
	}{
		{"single-node default", "", false, true, nil, false},
		{"single-node thin", "thin", false, true, []string{thin}, false},
		{"single-node thick", "Thick", false, false, []string{thick}, false},
		{"unknown strategy", "eager", false, false, nil, true},
		{"SharedBlock shared default", "", true, true, []string{shareable, scsi, thick}, false},
		{"SharedBlock shared thick", "thick", true, true, []string{shareable, scsi, thick}, false},
		{"SharedBlock shared thin", "thin", true, true, nil, true},
		{"other storage shared default", "", true, false, []string{shareable, scsi}, false},
		{"other storage shared thin", "thin", true, false, []string{shareable, scsi, thin}, false},
	}
	for _, c := range cases {
		got, err := volumeTags(c.provisioning, c.shared, c.sharedBlock)
		if (err != nil) != c.wantFail || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v, fail=%v", c.name, got, err, c.want, c.wantFail)
		}
	}
}
