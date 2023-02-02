package main

import (
	"context"

	"github.com/grainrigi/routeros-fletsv6-companion/logger"
)

var daemonErrs = make(chan error)

var llog = logger.NewBuiltinLogger()

func startDaemon(ctx context.Context, f func(context.Context) error) {
	go func() {
		err := f(ctx)
		if err != nil {
			daemonErrs <- err
		}
	}()
}

func main() {
	racfg, err := loadRAConfig()
	if err != nil {
		llog.Fatal("%s", err)
	}
	dumpRAConfig(racfg)

	ndcfg, ndNeedROS, err := loadNDConfig(racfg)
	if err != nil {
		llog.Fatal("%s", err)
	}
	dumpNDConfig(ndcfg)

	ctx, _ := context.WithCancel(context.Background())

	// init ros (if necessary)
	var ros *ROSClient
	if racfg.mode == "ros" || ndNeedROS {
		roscfg, err := loadROSConfig()
		if err != nil {
			llog.Fatal("%s", err)
		}
		ros, err = NewROSClient(roscfg)
		if err != nil {
			llog.Fatal("Failed to initialize RouterOS API: %s", err)
		}
	}

	// startRA
	rac := NewRAClient(racfg, ros)
	if racfg.mode != "off" {
		llog.Info("Starting RA Server")
		startDaemon(ctx, func(ctx context.Context) error { return rac.Work(ctx) })
	}
	// start ND
	if ndcfg.mode != "off" {
		llog.Info("Starting ND Server")
		ndc := NewNDClient(ndcfg, rac, ros)
		startDaemon(ctx, func(ctx context.Context) error { return ndc.Work(ctx) })
	}

	<-daemonErrs
}
