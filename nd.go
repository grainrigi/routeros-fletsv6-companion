package main

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket/layers"
	"golang.org/x/net/bpf"
)

type NDConfig struct {
	mode      string
	timeoutMs int
	prefixes  []FlexibleIP
	excludes  []FlexibleIP
	extIfs    []string
	intIfs    []string
	advMACs   []MACRef
}

type MACRef struct {
	hwaddr net.HardwareAddr
	rosIf  string
}

type NDClient struct {
	cfg      *NDConfig
	ra       *RAClient
	ros      *ROSClient
	extSocks map[string]SockRef
	intSocks map[string]SockRef
	mutex    sync.Mutex
}

type SockRef struct {
	name   string
	s      *Socket
	advMAC MACRef
}

func dumpNDConfig(cfg *NDConfig) {
	llog.Debug("NDProxy Configuration:")
	llog.Debug("  NDP_MODE=%s", cfg.mode)
	llog.Debug("  NDP_TIMEOUT=%s", cfg.timeoutMs)
	if len(cfg.prefixes) > 0 {
		llog.Debug("  NDP_PREFIXES")
		for i, p := range cfg.prefixes {
			llog.Debug("  %3d: %+v", i, p)
		}
	}
	if len(cfg.excludes) > 0 {
		llog.Debug("  NDP_EXCLUDE_IPS")
		for i, p := range cfg.excludes {
			llog.Debug("  %3d: %+v", i, p)
		}
	}
	if len(cfg.extIfs) > 0 {
		llog.Debug("  NDP_EXTERNAL_INTERFACES=%+v", cfg.extIfs)
	}
	if len(cfg.intIfs) > 0 {
		llog.Debug("  NDP_INTERNAL_INTERFACES=%+v", cfg.intIfs)
	}
	if len(cfg.excludes) > 0 {
		llog.Debug("  NDP_ADVERTISE_MACS")
		for i, p := range cfg.advMACs {
			llog.Debug("  %3d: %+v", i, p)
		}
	}
}

func NewNDClient(cfg *NDConfig, ra *RAClient, ros *ROSClient) *NDClient {
	c := &NDClient{
		cfg: cfg,
		ra:  ra,
		ros: ros,
	}

	return c
}

func initSockGroup(socks map[string]SockRef, ifnames []string, filter []bpf.RawInstruction, advMACs []MACRef) (map[string]SockRef, error) {
	var iflist []string

	if socks == nil {
		iflist = ifnames
		socks = make(map[string]SockRef)
	} else {
		for k := range socks {
			iflist = append(iflist, k)
		}
	}

	ifs, err := collectInterfaces(iflist)
	if err != nil {
		return socks, err
	}
	llog.Debug("  collectInterfaces(%+v) -> %+v", ifnames, ifs)

	maci := 0
	for k := range ifs {
		// search original index
		for i, name := range iflist {
			if k == name {
				maci = i
				break
			}
		}
		// prepare
		sr := socks[k]
		if sr.s == nil || !sr.s.isValid {
			ii, err := ifs[k].Index()
			if err != nil {
				llog.Warning("  failed to get the index of %s", k)
				continue
			}
			s, err := NewSocket(ii)
			if err != nil {
				llog.Warning("  failed to initialize socket for %s", k)
				continue
			}
			if filter != nil {
				if err := s.ApplyBPF(filter); err != nil {
					llog.Warning("  failed to apply a packet filter on %s", k)
					_ = s.Close()
					continue
				}
			}
			var advMAC MACRef
			if advMACs != nil {
				advMAC = advMACs[maci]
			}
			socks[k] = SockRef{
				name:   k,
				s:      s,
				advMAC: advMAC,
			}
		}
	}

	return socks, nil
}

func (c *NDClient) processNd(targetIP net.IP, srcMAC net.HardwareAddr, srcIP net.IP, ref *SockRef) {
	var hwaddr net.HardwareAddr
	var err error
	switch c.cfg.mode {
	case "static":
		llog.Trace("skipping solicitation since NDP_MODE=static")
		hwaddr = make([]byte, 6)
	case "proxy":
		llog.Trace("soliciting by myself: targetIP=%s", targetIP.String())
		hwaddr, err = c.solicitInternal(targetIP)
		if err != nil {
			llog.Warning("failed to send internal ND solicitation: %s", err)
		}
	case "proxy-ros":
		fallthrough
	case "proxy-ros:strict":
		llog.Trace("soliciting via routerboard: targetIP=%s", targetIP.String())
		hwaddr, err = c.ros.LookupNeighbor(targetIP, c.cfg.timeoutMs, c.cfg.mode == "proxy-ros:strict")
		if err != nil {
			llog.Warning("failed to send ND solicitation via RouterOS: %s", err)
		}
	}

	if hwaddr != nil {
		if c.cfg.mode != "static" {
			llog.Debug("SOLICITATION SUCCUSSFUL! %s is at %s", targetIP, hwaddr)
		}
		if ref.advMAC.rosIf != "" {
			// update advMAC
			mac, err := c.ros.GetInterfaceMAC(ref.advMAC.rosIf)
			if err != nil {
				llog.Warning("failed to fetch the MAC address of %s: %s", ref.advMAC.rosIf, err)
			}
			ref.advMAC.hwaddr = mac
		}
		llog.Debug("Sending out Neighbor Advertisement: targetIP=%s srcMAC=%s, dstMAC=%s", targetIP, ref.advMAC.hwaddr, srcMAC)
		// sending out NA (synchronized)
		na := makeICMPv6(ICMPv6Data[*layers.ICMPv6NeighborAdvertisement]{
			SrcMAC: ref.advMAC.hwaddr,
			DstMAC: srcMAC,
			SrcIP:  targetIP,
			DstIP:  srcIP,
			Type:   136,
			Layer: &layers.ICMPv6NeighborAdvertisement{
				Flags:         0x40,
				TargetAddress: targetIP,
				Options: []layers.ICMPv6Option{
					{Type: 2, Data: ref.advMAC.hwaddr},
				},
			},
		})
		if err := func() error {
			c.mutex.Lock()
			defer c.mutex.Unlock()
			return ref.s.WriteOnce(na)
		}(); err != nil {
			llog.Warning("failed to send NA via %s", ref.name)
		}
	} else {
		llog.Trace("solicitation failed for %s", targetIP)
	}
}

func (c *NDClient) workInternal(context.Context) error {
	var err error

	// initialize external sockets (mandatory)
	c.extSocks, err = initSockGroup(c.extSocks, c.cfg.extIfs, bpfND(), c.cfg.advMACs)
	if err != nil {
		return err
	}
	// initialize internal sockets (if necessarry)
	if c.cfg.mode == "proxy" {
		c.intSocks, err = initSockGroup(c.intSocks, c.cfg.intIfs, bpfICMPv6(136), nil) // Neighbor Advertisement
		if err != nil {
			return err
		}
	}

	// nd receive loop
	extSockRefs := make([]SockRef, len(c.extSocks))
	extSocks := make([]*Socket, len(c.extSocks))
	i := 0
	for _, s := range c.extSocks {
		extSockRefs[i] = s
		extSocks[i] = s.s
		i++
	}
main:
	for {
		si, packet, err := ReadMultiSocksOnce(extSocks, nil)
		if err != nil {
			return err
		}
		sr := extSockRefs[si]
		nd := ICMPv6Data[*layers.ICMPv6NeighborSolicitation]{}
		if err := parseICMPv6(packet, &nd); err != nil {
			llog.Warning("failed to parse ND Solicitation: %s", err)
			continue
		}
		targetIP := nd.Layer.TargetAddress
		llog.Debug("Received an nd solicitation: targetIP=%s srcMAC=%s", targetIP.String(), nd.SrcMAC.String())

		// check whether in specified prefixes
		validPrefix := false
		for _, prefix := range c.cfg.prefixes {
			pfip := c.ra.ResolveFIP(prefix)
			if pfip == nil {
				continue
			}
			if pfip.Contains(targetIP) {
				validPrefix = true
				break
			}
		}
		if !validPrefix {
			continue
		}

		//  check exclude ip
		for _, exclude := range c.cfg.excludes {
			efip := c.ra.ResolveFIP(exclude)
			if efip == nil {
				continue
			}
			if efip.Contains(targetIP) {
				llog.Debug("excluding %s", targetIP.String())
				continue main
			}
		}

		go c.processNd(targetIP, nd.SrcMAC, nd.SrcIP, &sr)
	}
}

func (c *NDClient) Work(ctx context.Context) error {
	for {
		err := c.workInternal(ctx)
		if err != nil {
			// check ctx
			select {
			case <-ctx.Done():
				return err
			default:
			}
			// holdoff timer
			llog.Warning("NDProxy Worker failed: %s", err)
			llog.Warning("Waiting 10s to avoid error bursting")
		}
		<-time.After(time.Second * 10)
	}
}

func (c *NDClient) solicitInternal(ip net.IP) (net.HardwareAddr, error) {
	socks := make([]*Socket, len(c.intSocks))
	sockRefs := make([]SockRef, len(c.intSocks))
	// send nd
	dstIP, dstMAC := multicastAddr(ip)
	i := 0
	for _, s := range c.intSocks {
		packet := makeICMPv6(ICMPv6Data[*layers.ICMPv6NeighborSolicitation]{
			Type:   135,
			SrcMAC: s.s.netif.HardwareAddr,
			DstMAC: dstMAC,
			SrcIP:  s.s.LinkLocal(),
			DstIP:  dstIP,
			Layer: &layers.ICMPv6NeighborSolicitation{
				TargetAddress: ip,
				Options: []layers.ICMPv6Option{
					{Type: 1, Data: s.s.netif.HardwareAddr},
				},
			},
		})
		s.s.FlushAll()
		llog.Trace("  sending out nd via %s", s.name)
		if err := s.s.WriteOnce(packet); err != nil {
			return nil, err
		}
		socks[i] = s.s
		sockRefs[i] = s
		i++
	}
	// wait for na
	var remain *time.Duration
	if c.cfg.timeoutMs != 0 {
		remain = new(time.Duration)
		*remain = time.Millisecond * time.Duration(c.cfg.timeoutMs)
	}
	for remain == nil || *remain > 0 {
		var na ICMPv6Data[*layers.ICMPv6NeighborAdvertisement]

		start := time.Now()

		si, packet, err := ReadMultiSocksOnce(socks, remain)
		if err != nil {
			return nil, err
		}
		if si == -1 {
			break // timed out
		}
		err = parseICMPv6(packet, &na)
		if err != nil {
			llog.Warning("  failed to parse na packet from %s: %+v", sockRefs[si].name, packet)
			goto next
		}
		if !na.Layer.TargetAddress.Equal(ip) {
			goto next
		}

		return na.SrcMAC, nil

	next:
		*remain -= time.Now().Sub(start)
	}

	llog.Trace("  nd solicitation timed out after %d ms", c.cfg.timeoutMs)
	return nil, nil
}
