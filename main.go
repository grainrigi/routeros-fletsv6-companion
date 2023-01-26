package main

import (
	"context"
	"fmt"
	"log"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

var daemonErrs = make(chan error)

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
		log.Fatal(err)
	}

	ctx, _ := context.WithCancel(context.Background())

	// init epoll
	if err := initEpoll(); err != nil {
		log.Fatalf("Failed to init epoll: %s", err)
	}
	go runEpollLoop(ctx)

	// init ros (if necessary)
	var ros *ROSClient
	if racfg.mode == "ros" {
		roscfg, err := loadROSConfig()
		if err != nil {
			log.Fatal(err)
		}
		ros, err = NewROSClient(roscfg)
		if err != nil {
			log.Fatalf("Failed to initialize RouterOS API: %s", err)
		}
	}

	// startRA
	if racfg.mode != "off" {
		log.Print("Start RA Server")
		rac := NewRAClient(racfg, ros)
		startDaemon(ctx, func(ctx context.Context) error { return rac.Work(ctx) })
	}

	<-daemonErrs

	return

	var cfg Config

	// load config
	if err := loadConfig(&cfg); err != nil {
		log.Fatal(err)
	}

	// Show config
	log.Printf("PREFIXES: %v\n", cfg.prefixes)
	log.Printf("INTERFACES: %v\n", cfg.ifnames)
	log.Printf("ROUTERMAC: %s\n", cfg.routerMAC)
	log.Printf("ROUTERIP: %v\n", cfg.routerIPs)

	f, err := openNdInterface(cfg)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Start watching")

	var eth layers.Ethernet
	var ip6 layers.IPv6
	var icmp6 layers.ICMPv6
	var icmp6nd layers.ICMPv6NeighborSolicitation
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip6, &icmp6, &icmp6nd)
	decoded := []gopacket.LayerType{}

	for {
		buf := make([]byte, 2000)
		numRead, err := f.Read(buf)
		if err != nil {
			fmt.Printf("%s", err)
			continue
		}
		if err := parser.DecodeLayers(buf[:numRead], &decoded); err != nil {
			fmt.Printf("%s", err)
			continue
		}
		packet := processNd(NdSolicitation{
			srcMAC:   eth.SrcMAC,
			dstMAC:   eth.DstMAC,
			srcIP:    ip6.SrcIP,
			dstIP:    ip6.DstIP,
			targetIP: icmp6nd.TargetAddress,
		}, cfg)
		if packet != nil {
			n, err := f.Write(packet)
			if err != nil {
				fmt.Println(err.Error())
			}
			if n != len(packet) {
				fmt.Printf("packet truncated: written=%d, sent=%d\n", len(packet), n)
			}
		}
	}
}
