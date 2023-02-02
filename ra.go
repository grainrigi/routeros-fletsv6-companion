package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket/layers"
)

type FlexibleIP struct {
	raPrefix bool
	ip       net.IP
	cidr     int
}

func (f FlexibleIP) String() string {
	if f.ip == nil {
		return "ra-prefix"
	}
	if f.cidr == -1 {
		return fmt.Sprintf("ra-prefix%s", f.ip)
	}
	return fmt.Sprintf("ra-prefix%s/%d", f.ip, f.cidr)
}

type ROSIPAssign struct {
	ip      FlexibleIP
	ifname  string
	options ROSIPOptions
}

type ROSPoolAssign struct {
	ip           FlexibleIP
	poolname     string
	prefixLength int
}

type RAConfig struct {
	mode      string
	extIfs    []string
	timeout   time.Duration
	rosExtIf  string
	rosExtIPs []ROSIPAssign
	rosIntIPs []ROSIPAssign
	rosPools  []ROSPoolAssign
}

type RAClient struct {
	cfg        *RAConfig
	ros        *ROSClient
	extSock    *Socket
	routerInfo *RouterInfo
	infomu     sync.RWMutex
}

type RouterInfo struct {
	prefix  net.IPNet
	gateway net.IP
}

func dumpRAConfig(cfg *RAConfig) {
	llog.Debug("Router Advertisement Configuration:")
	llog.Debug("  RA_MODE=%s", cfg.mode)
	if len(cfg.extIfs) > 0 {
		llog.Debug("  RA_EXTERNAL_INTERFACES=%+v", cfg.extIfs)
	}
	if cfg.timeout != 0 {
		llog.Debug("  RA_TIMEOUT=%d", cfg.timeout/time.Millisecond)
	}
	if cfg.rosExtIf != "" {
		llog.Debug("  RA_ROS_EXTERNAL_INTERFACE=%s", cfg.rosExtIf)
	}
	if len(cfg.rosExtIPs) > 0 {
		llog.Debug("  RA_ROS_EXTERNAL_IPS")
		for i, ass := range cfg.rosExtIPs {
			llog.Debug("  %3d: %+v", i, ass)
		}
	}
	if len(cfg.rosIntIPs) > 0 {
		llog.Debug("  RA_ROS_INTERNAL_IPS")
		for i, ass := range cfg.rosIntIPs {
			llog.Debug("  %3d: %+v", i, ass)
		}
	}
	if len(cfg.rosIntIPs) > 0 {
		llog.Debug("  RA_ROS_INTERNAL_IPS")
		for i, ass := range cfg.rosIntIPs {
			llog.Debug("  %3d: %+v", i, ass)
		}
	}
}

func NewRAClient(cfg *RAConfig, ros *ROSClient) *RAClient {
	return &RAClient{
		cfg: cfg,
		ros: ros,
	}
}

func (c *RAClient) initSock() error {
	if c.extSock != nil && c.extSock.isValid {
		return nil
	}

	extif, err := findFirstInterface(c.cfg.extIfs)
	if err != nil {
		return err
	}
	extifidx, err := extif.Index()
	if err != nil {
		return err
	}
	extsock, err := NewSocket(extifidx)
	if err != nil {
		return err
	}
	if err := extsock.ApplyBPF(bpfRA()); err != nil {
		return err
	}

	c.extSock = extsock

	return nil
}

func (c *RAClient) receive(ctx context.Context, timeout bool) (*RouterInfo, error) {
	var info RouterInfo
	var rapacket []byte
	var to *time.Duration

	if timeout {
		to = &c.cfg.timeout
	}
	timeoutStr := "nil"
	if to != nil {
		timeoutStr = fmt.Sprintf("%dms", *to/time.Millisecond)
	}

	llog.Debug("Waiting for router advertisement on %s (timeout=%s)", c.extSock.netif.Name, timeoutStr)
	select {
	case result := <-c.extSock.ReadOnceChan(to):
		if result.err != nil {
			return nil, result.err
		}
		rapacket = result.data
	case <-ctx.Done():
		return nil, fmt.Errorf("canceled by context")
	}

	ra := &ICMPv6Data[*layers.ICMPv6RouterAdvertisement]{}
	if err := parseICMPv6(rapacket, ra); err != nil {
		return nil, err
	}
	info.gateway = ra.SrcIP
	for _, opt := range ra.Layer.Options {
		if opt.Type == 3 {
			length := int(opt.Data[0])
			ip := net.IP(opt.Data[14:30])
			info.prefix = net.IPNet{IP: ip, Mask: net.CIDRMask(length, 128)}
		}
	}
	llog.Debug("Received a router advertisement: gateway=%s prefix=%s", info.gateway.String(), info.prefix.String())

	return &info, nil
}

func (c *RAClient) soilicit(ctx context.Context) error {
	if c.routerInfo != nil {
		return nil
	}

	for {
		// send solicitation
		llog.Debug("Sending out Router Solicitation via %s", c.extSock.netif.Name)
		rs := makeRouterSolicitation(c.extSock.LinkLocal(), c.extSock.netif.HardwareAddr)
		if err := c.extSock.WriteOnce(rs); err != nil {
			return err
		}
		// wait for advertisement
		rinfo, err := c.receive(ctx, true)
		if err != nil {
			return err
		}
		if rinfo.prefix.IP == nil {
			return fmt.Errorf("Router did not return a prefix")
		}
		llog.Info("Router solicited: prefix=%s gateway=%s", rinfo.prefix.String(), rinfo.gateway.String())
		func() {
			c.infomu.Lock()
			defer c.infomu.Unlock()
			c.routerInfo = rinfo
		}()
		break
	}

	return nil
}

func (c *RAClient) reconcile() {
	if c.cfg.mode == "ros" {
		// apply ros config
		if c.cfg.rosExtIf != "" {
			if err := c.ros.SetIPv6Gateway(c.cfg.rosExtIf, c.routerInfo.gateway); err != nil {
				llog.Warning("ros.SetIPv6Gateway failed: %s", err)
			}
		}
		for _, eip := range c.cfg.rosExtIPs {
			ip := c.ResolveFIP(eip.ip)
			if err := c.ros.AssignIPv6(eip.ifname, ip, eip.ip.String(), eip.options); err != nil {
				llog.Warning("ros.AssignIPv6(%s, %s) failed: %s", eip.ifname, ip.String(), err)
			}
		}
		for _, iip := range c.cfg.rosIntIPs {
			ip := c.ResolveFIP(iip.ip)
			if err := c.ros.AssignIPv6(iip.ifname, ip, iip.ip.String(), iip.options); err != nil {
				llog.Warning("ros.AssignIPv6(%s, %s) failed: %s", iip.ifname, ip.String(), err)
			}
		}
		for _, pool := range c.cfg.rosPools {
			prefix := c.ResolveFIP(pool.ip)
			if err := c.ros.ExportIPv6Pool(pool.poolname, *prefix, pool.prefixLength); err != nil {
				llog.Warning("ros.ExportIPv6Pool(%s, %s, %d) failed: %s", pool.poolname, prefix.String(), pool.prefixLength, err)
			}
		}
	}
}

func (c *RAClient) workInternal(ctx context.Context) error {
	// prepare interface
	if err := c.initSock(); err != nil {
		return fmt.Errorf("raInitSock failed: %s", err)
	}
	// resolve ra
	if err := c.soilicit(ctx); err != nil {
		return fmt.Errorf("raSolicit failed: %s", err)
	}
	c.reconcile()

	// listen RA
	for {
		rinfo, err := c.receive(ctx, false)
		if err != nil {
			return err
		}
		if rinfo.prefix.String() != c.routerInfo.prefix.String() ||
			!rinfo.gateway.Equal(c.routerInfo.gateway) {
			llog.Info("RouterInfo changed: prefix=%s gateway=%s", rinfo.prefix.String(), rinfo.gateway.String())
			func() {
				c.infomu.Lock()
				defer c.infomu.Unlock()
				c.routerInfo = rinfo
			}()
			c.reconcile()
		}
	}
}

func (c *RAClient) Work(ctx context.Context) error {
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
			llog.Error("Router Advertisement Worker failed: %s", err)
			llog.Error("Waiting 10s to avoid error bursting")
		}
		<-time.After(time.Second * 10)
	}
}

func (c *RAClient) ResolveFIP(fip FlexibleIP) *net.IPNet {
	ip := make(net.IP, 16)
	copy([]byte(ip), []byte(fip.ip))

	rinfo := func() *RouterInfo {
		c.infomu.RLock()
		defer c.infomu.RUnlock()
		return c.routerInfo
	}()

	if fip.raPrefix && rinfo == nil {
		return nil
	}

	if fip.raPrefix {
		maskedIPAssign(ip, rinfo.prefix.IP, rinfo.prefix.Mask)
	}

	var mask net.IPMask
	if fip.cidr == -1 {
		mask = make(net.IPMask, 16)
		copy(mask, rinfo.prefix.Mask)
	} else {
		mask = net.CIDRMask(fip.cidr, 128)
	}

	return &net.IPNet{
		IP:   ip,
		Mask: mask,
	}
}
