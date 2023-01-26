package main

import (
	"context"
	"log"
	"net"
	"os"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/net/bpf"
)

type NDConfig struct {
	mode     string
	timeout  *time.Duration
	prefixes []FlexibleIP
	excludes []FlexibleIP
	extIfs   []string
	intIfs   []string
}

type NDClient struct {
	cfg      *NDConfig
	extSocks map[string]*Socket
	intSocks map[string]*Socket
}

func NewNDClient(cfg *NDConfig) *NDClient {
	c := &NDClient{
		cfg: cfg,
	}

	return c
}

func initSockGroup(socks map[string]*Socket, ifnames []string, filter []bpf.RawInstruction) (map[string]*Socket, error) {
	var iflist []string

	if socks == nil {
		iflist = ifnames
	} else {
		for k := range socks {
			iflist = append(iflist, k)
		}
	}

	ifs, err := collectInterfaces(iflist)
	if err != nil {
		return socks, err
	}

	for k := range ifs {
		s := socks[k]
		if s == nil || !s.isValid {
			ii, err := ifs[k].Index()
			if err != nil {
				log.Printf("[WARNING] Failed to get the index of %s", k)
				continue
			}
			s, err := NewSocket(ii)
			if err != nil {
				log.Printf("[WARNING] Failed to initialize socket for %s", k)
				continue
			}
			if filter != nil {
				if err := s.ApplyBPF(filter); err != nil {
					log.Printf("[WARNING] Failed to apply a packet filter on %s", k)
					_ = s.Close()
					continue
				}
			}
		}
	}

	return socks, nil
}

func (c *NDClient) workInternal(ctx context.Context) error {
	var err error

	// initialize external sockets (mandatory)
	c.extSocks, err = initSockGroup(c.extSocks, c.cfg.extIfs, bpfND())
	if err != nil {
		return err
	}
	// initialize internal sockets (if necessarry)
	if c.cfg.mode == "proxy" {
		c.intSocks, err = initSockGroup(c.intSocks, c.cfg.intIfs, bpfICMPv6(136)) // Neighbor Advertisement
		if err != nil {
			return err
		}
	}

	// nd receive loop
	extSocks := make([]*Socket, len(c.extSocks))
	for _, s := range c.extSocks {
		extSocks = append(extSocks, s)
	}
	for {
		data, err := ReadMultiSocksOnce(extSocks)
		if err != nil {
			return err
		}
	}
}

func (c *NDClient) solicitInternal(ip net.IP) (net.HardwareAddr, error) {
	return nil, nil
}

func openNdInterface(cfg Config) (*os.File, error) {
	i, err := openInterface(cfg.ifnames)
	if err != nil {
		return nil, err
	}

	// Filter for ND Packets
	insn := []bpf.Instruction{
		// from tcpdump -d "ether[0:2] == 0x3333 and icmp6[0] == 135"
		bpf.LoadAbsolute{Off: 0, Size: 2},     // Load ether dst[0:2]
		bpf.JumpIf{Val: 0x3333, SkipFalse: 7}, // dst[0:2] == 33:33 (IPv6 MultiCast)
		bpf.LoadAbsolute{Off: 12, Size: 2},    // Load EtherType
		bpf.JumpIf{Val: 0x86dd, SkipFalse: 5}, // EtherType == 0x86dd (IPv6)
		bpf.LoadAbsolute{Off: 20, Size: 1},    // Load IPv6 Next Header
		bpf.JumpIf{Val: 0x3a, SkipFalse: 3},   // Next Header = 0x3a (ICMPv6)
		bpf.LoadAbsolute{Off: 54, Size: 1},    // Load ICMPv6 Type
		bpf.JumpIf{Val: 0x87, SkipFalse: 1},   // Type == 0x87 (Neighbor Solicitation)
		bpf.RetConstant{Val: 262144},
		bpf.RetConstant{Val: 0},
	}
	// insn = bpfWithDot1Q(insn, 101)
	log.Printf("%s", insn)
	is, err := bpf.Assemble(insn)

	if err := applyBPF(i.fd, is); err != nil {
		return nil, err
	}

	return openFdFile(i.fd), nil
}

type NdSolicitation struct {
	srcMAC   net.HardwareAddr
	dstMAC   net.HardwareAddr
	srcIP    net.IP
	dstIP    net.IP
	targetIP net.IP
}

func processNd(s NdSolicitation, cfg Config) []byte {
	for _, i := range cfg.routerIPs {
		if s.targetIP.Equal(i) {
			return nil
		}
	}
	for _, p := range cfg.prefixes {
		if p.Contains(s.targetIP) {
			buf := gopacket.NewSerializeBuffer()
			opts := gopacket.SerializeOptions{
				FixLengths:       true,
				ComputeChecksums: true,
			}
			eth := &layers.Ethernet{
				SrcMAC:       cfg.routerMAC,
				DstMAC:       s.srcMAC,
				EthernetType: 0x86dd,
			}
			ip6 := &layers.IPv6{
				Version:      6,
				TrafficClass: 0xb8,
				FlowLabel:    0,
				Length:       0, // auto compute
				NextHeader:   0x3a,
				HopLimit:     255,
				SrcIP:        s.targetIP,
				DstIP:        s.srcIP,
			}
			icmpv6 := &layers.ICMPv6{
				TypeCode: 0x8800,
				Checksum: 0, // auto compute
			}
			icmpv6.SetNetworkLayerForChecksum(ip6)
			err := gopacket.SerializeLayers(buf, opts,
				eth, ip6, icmpv6,
				&layers.ICMPv6NeighborAdvertisement{
					Flags:         0x40,
					TargetAddress: s.targetIP,
					Options: []layers.ICMPv6Option{
						{Type: 2, Data: cfg.routerMAC},
					},
				},
			)
			if err != nil {
				log.Println(err)
				return nil
			}

			log.Printf("Sending out neighbor advertisement: targetIP: %s, srcMAC: %s, dstMAC: %s\n", s.targetIP, cfg.routerMAC, s.srcMAC)

			return buf.Bytes()
		}
	}

	return nil
}
