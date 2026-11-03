// Package driver implements the CSI Identity, Controller and Node services for
// ZStack data volumes.
package driver

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"github.com/zstackio/zstack-csi/pkg/zstack"
)

const DefaultDriverName = "disk.csi.zstack.io"

type Mode string

const (
	ModeController Mode = "controller"
	ModeNode       Mode = "node"
	ModeAll        Mode = "all"
)

type Config struct {
	Name              string
	Version           string
	Mode              Mode
	Endpoint          string
	NodeID            string
	MaxVolumesPerNode int64
}

// Run serves the services enabled by cfg.Mode until SIGTERM or SIGINT. cloud is
// only needed by the controller service.
func Run(cfg Config, cloud *zstack.Cloud) error {
	srv := grpc.NewServer(grpc.UnaryInterceptor(logCalls))
	csi.RegisterIdentityServer(srv, &identityServer{name: cfg.Name, version: cfg.Version})

	if cfg.Mode == ModeController || cfg.Mode == ModeAll {
		csi.RegisterControllerServer(srv, newControllerServer(cloud))
	}
	if cfg.Mode == ModeNode || cfg.Mode == ModeAll {
		ns, err := newNodeServer(cfg.NodeID, cfg.MaxVolumesPerNode)
		if err != nil {
			return err
		}
		klog.Infof("node ID %s", ns.nodeID)
		csi.RegisterNodeServer(srv, ns)
	}

	lis, err := listen(cfg.Endpoint)
	if err != nil {
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		srv.GracefulStop()
	}()
	klog.Infof("%s %s serving %s mode on %s", cfg.Name, cfg.Version, cfg.Mode, cfg.Endpoint)
	return srv.Serve(lis)
}

func listen(endpoint string) (net.Listener, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "unix":
		addr := u.Path
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale socket %s: %w", addr, err)
		}
		return net.Listen("unix", addr)
	case "tcp":
		return net.Listen("tcp", u.Host)
	default:
		return nil, fmt.Errorf("unsupported endpoint scheme %q", u.Scheme)
	}
}

func logCalls(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(4).Infof("%s request: %+v", info.FullMethod, req)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("%s failed: %v", info.FullMethod, err)
	} else {
		klog.V(4).Infof("%s response: %+v", info.FullMethod, resp)
	}
	return resp, err
}
