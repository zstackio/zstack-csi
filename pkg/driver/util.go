package driver

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	GiB               = int64(1) << 30
	defaultVolumeSize = GiB
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// resourceUUID derives a stable ZStack UUID from a CSI request name so that a
// retried CreateVolume/CreateSnapshot finds the resource created by the first
// attempt. The version and variant bits follow RFC 4122 version 5, which the
// ZStack API requires for resourceUuid.
func resourceUUID(kind, name string) string {
	sum := sha1.Sum([]byte(kind + "/" + name))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// volumeSize returns the requested size rounded up to whole GiB.
func volumeSize(r *csi.CapacityRange) (int64, error) {
	required, limit := r.GetRequiredBytes(), r.GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, fmt.Errorf("capacity range must not be negative")
	}
	if limit > 0 && required > limit {
		return 0, fmt.Errorf("required bytes %d exceed limit bytes %d", required, limit)
	}
	size := required
	if size == 0 {
		size = defaultVolumeSize
		if limit > 0 && limit < size {
			size = limit
		}
	}
	size = (size + GiB - 1) / GiB * GiB
	if limit > 0 && size > limit {
		return 0, fmt.Errorf("no whole-GiB size fits within required %d and limit %d bytes", required, limit)
	}
	return size, nil
}

var singleNodeModes = map[csi.VolumeCapability_AccessMode_Mode]bool{
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:      true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY: true,
}

// Multi-node modes are served by ZStack shared volumes as raw block devices;
// ext4 and xfs cannot be mounted on several nodes at once.
var multiNodeModes = map[csi.VolumeCapability_AccessMode_Mode]bool{
	csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:   true,
	csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER: true,
	csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:  true,
}

var supportedFsTypes = map[string]bool{"": true, "ext4": true, "ext3": true, "xfs": true}

func validateCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return fmt.Errorf("volume capabilities are required")
	}
	for _, c := range caps {
		if c.GetAccessMode() == nil {
			return fmt.Errorf("access mode is required")
		}
		mode := c.GetAccessMode().GetMode()
		if !singleNodeModes[mode] && !multiNodeModes[mode] {
			return fmt.Errorf("access mode %s is not supported", mode)
		}
		switch {
		case c.GetBlock() != nil:
		case c.GetMount() != nil:
			if multiNodeModes[mode] {
				return fmt.Errorf("access mode %s needs volumeMode Block", mode)
			}
			if fs := c.GetMount().GetFsType(); !supportedFsTypes[fs] {
				return fmt.Errorf("filesystem type %q is not supported", fs)
			}
		default:
			return fmt.Errorf("access type block or mount is required")
		}
	}
	return nil
}

func isMultiNode(caps []*csi.VolumeCapability) bool {
	for _, c := range caps {
		if multiNodeModes[c.GetAccessMode().GetMode()] {
			return true
		}
	}
	return false
}

func isReadOnly(c *csi.VolumeCapability) bool {
	mode := c.GetAccessMode().GetMode()
	return mode == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY ||
		mode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
}

// keyLocks rejects concurrent operations on the same volume or snapshot; the
// caller returns Aborted and the sidecar or kubelet retries later.
type keyLocks struct {
	mu   sync.Mutex
	held map[string]struct{}
}

func newKeyLocks() *keyLocks {
	return &keyLocks{held: map[string]struct{}{}}
}

func (l *keyLocks) TryAcquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.held[key]; ok {
		return false
	}
	l.held[key] = struct{}{}
	return true
}

func (l *keyLocks) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, key)
}
