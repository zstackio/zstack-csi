package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/klog/v2"

	"github.com/zstackio/zstack-csi/pkg/driver"
	"github.com/zstackio/zstack-csi/pkg/zstack"
)

var version = "dev"

func main() {
	klog.InitFlags(nil)
	cfg := driver.Config{Version: version}
	var mode string
	flag.StringVar(&cfg.Endpoint, "endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
	flag.StringVar(&cfg.Name, "driver-name", driver.DefaultDriverName, "CSI driver name")
	flag.StringVar(&mode, "mode", string(driver.ModeAll), "services to run: controller, node or all")
	flag.StringVar(&cfg.NodeID, "node-id", "", "node ID; defaults to the ZStack VM UUID read from DMI")
	flag.Int64Var(&cfg.MaxVolumesPerNode, "max-volumes-per-node", 24, "data volumes a node VM can attach; ZStack allows 24 by default")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg.Mode = driver.Mode(mode)
	if err := run(cfg); err != nil {
		klog.Error(err)
		klog.Flush()
		os.Exit(1)
	}
}

func run(cfg driver.Config) error {
	var cloud *zstack.Cloud
	switch cfg.Mode {
	case driver.ModeController, driver.ModeAll:
		c, err := zstack.NewFromEnv()
		if err != nil {
			return err
		}
		v, err := c.Version()
		if err != nil {
			return fmt.Errorf("connect to ZStack API: %w", err)
		}
		tz, err := c.SyncTimeZone()
		if err != nil {
			return err
		}
		klog.Infof("connected to ZStack %s, management node time zone %s", v, tz)
		cloud = c
	case driver.ModeNode:
	default:
		return fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	return driver.Run(cfg, cloud)
}
