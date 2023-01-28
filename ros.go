package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros"
)

var rosCommentKey = "set by fletsv6-companion"

type ROSConnectConfig struct {
	host     string
	port     int
	username string
	password string
	useTLS   bool
}

type ROSIPOptions struct {
	Eui64     bool
	Advertise bool
}

type ROSClient struct {
	cfg  ROSConnectConfig
	pool *ROSConnectionPool
}

// connection pool
type ROSConnectionPool struct {
	cfg   ROSConnectConfig
	cs    []*ROSConnection
	mutex sync.Mutex
}
type ROSConnection struct {
	*routeros.Client
}

func NewROSConnectionPool(cfg ROSConnectConfig) *ROSConnectionPool {
	pool := &ROSConnectionPool{
		cfg: cfg,
	}

	// start keep alive
	go func() {
		for {
			var err error
			c, count := func() (*ROSConnection, int) {
				pool.mutex.Lock()
				defer pool.mutex.Unlock()
				// pick a connection
				count := len(pool.cs)
				if count == 0 {
					return nil, 0
				}
				c := pool.cs[count-1]
				pool.cs = pool.cs[:count-1]
				return c, count
			}()
			llog.Trace("Current pooled connections: %d", count)
			if c != nil {
				// do keepalive action
				ch := make(chan *routeros.Reply)
				go func() {
					defer close(ch)
					rep, err := c.RunArgs([]string{
						"/system/resource/print",
						"=.proplist=uptime",
					})
					if err != nil {
						rep = nil
					}
					select {
					case ch <- rep:
					default:
					}
				}()
				select {
				case rep := <-ch:
					if rep != nil && len(rep.Re) >= 1 {
						// keepalive success, push back to pool
						func() {
							pool.mutex.Lock()
							defer pool.mutex.Unlock()
							pool.cs = append([]*ROSConnection{c}, pool.cs...)
						}()
						goto next
					}
					llog.Warning("ROS keepalive failed. discarding connection")
				case <-time.After(time.Second * 5):
					llog.Warning("ROS keepalive timed out. discarding connection")
				}
				// discard action
				c.Close()
				c = nil
				count--
			}
			if count == 0 {
				count = 1
				c, err = pool.makeConnection()
				if err != nil {
					llog.Warning("failed to establish connection to RouterOS API: %s", err)
				}
			}

		next:
			<-time.After(time.Second * 10 / time.Duration(count))
		}
	}()

	return pool
}

func (p *ROSConnectionPool) makeConnection() (*ROSConnection, error) {
	var cl *routeros.Client
	var err error

	cfg := &p.cfg
	address := fmt.Sprintf("%s:%d", cfg.host, cfg.port)

	if cfg.useTLS {
		cl, err = routeros.DialTLS(address, cfg.username, cfg.password, &tls.Config{InsecureSkipVerify: true})
	} else {
		cl, err = routeros.Dial(address, cfg.username, cfg.password)
	}

	if err != nil {
		return nil, err
	}

	return &ROSConnection{
		Client: cl,
	}, nil
}

func (p *ROSConnectionPool) Get() (*ROSConnection, error) {
	// try established
	p.mutex.Lock()
	if len(p.cs) > 0 {
		llog.Trace("ROSConnectionPool.Get(): Using established connection")
		c := p.cs[0]
		p.cs = p.cs[1:]
		p.mutex.Unlock()
		return c, nil
	}
	p.mutex.Unlock()
	// create client
	llog.Trace("ROSConnectionPool.Get(): Creating a new connection")
	return p.makeConnection()
}

func (p *ROSConnectionPool) Put(c *ROSConnection) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.cs = append(p.cs, c)
}

func NewROSClient(cfg ROSConnectConfig) (*ROSClient, error) {
	c := &ROSClient{
		cfg:  cfg,
		pool: NewROSConnectionPool(cfg),
	}

	return c, nil
}

func (c *ROSClient) RunArgs(args []string) (*routeros.Reply, error) {
	conn, err := c.pool.Get()
	if err != nil {
		return nil, err
	}
	// with timeout
	ch := make(chan struct{})
	var rep *routeros.Reply
	go func() {
		rep, err = conn.RunArgs(args)
		select {
		case ch <- struct{}{}:
		default:
		}
	}()

	select {
	case <-ch:
		c.pool.Put(conn)
		return rep, err
	case <-time.After(time.Second * 5):
		conn.Close()
		return nil, fmt.Errorf("RouterOS API timed out after 5s")
	}
}

func (c *ROSClient) makeClient() (*routeros.Client, error) {
	cfg := &c.cfg
	address := fmt.Sprintf("%s:%d", cfg.host, cfg.port)

	if cfg.useTLS {
		return routeros.DialTLS(address, cfg.username, cfg.password, &tls.Config{InsecureSkipVerify: true})
	} else {
		return routeros.Dial(address, cfg.username, cfg.password)
	}

}

func (*ROSClient) dumpResponse(rep *routeros.Reply) {
	llog.Trace("  raw response:")
	if len(rep.Re) == 0 {
		llog.Trace("    <empty>")
		return
	}
	for i, re := range rep.Re {
		llog.Trace("  %3d: %+v", i, re.Map)
	}
}

func (c *ROSClient) GetInterfaceMAC(name string) (net.HardwareAddr, error) {
	llog.Trace("GetInterfaceMAC(%s)", name)
	rep, err := c.RunArgs([]string{
		"/interface/print",
		"=.proplist=mac-address",
		fmt.Sprintf("?name=%s", name),
	})
	if err != nil {
		return nil, err
	}
	c.dumpResponse(rep)
	if len(rep.Re) == 0 {
		return nil, fmt.Errorf("device %s not found", name)
	}
	return net.ParseMAC(rep.Re[0].Map["mac-address"])
}

func (c *ROSClient) SetIPv6Gateway(ifname string, gateway net.IP) error {
	llog.Trace("SetIPv6Gateway(%s, %s)", ifname, gateway.String())

	// check if route exists
	llog.Trace("  fetching all IPv6 default routes")
	rep, err := c.RunArgs([]string{
		"/ipv6/route/print",
		"=.proplist=.id,gateway,comment",
		"?dst-address=::/0",
	})
	if err != nil {
		return err
	}
	c.dumpResponse(rep)
	var modTarget string
	for _, re := range rep.Re {
		if re.Map["comment"] == rosCommentKey {
			llog.Trace("  found a commented route(.id=%s) mark as modification target", re.Map[".id"])
			modTarget = re.Map[".id"]
		}
		gwparts := strings.Split(re.Map["gateway"], "%")
		if len(gwparts) != 2 {
			llog.Trace("  gateway (%s) is unknown format. skipping", re.Map["gateway"])
			continue
		}
		gwip := net.ParseIP(gwparts[0])
		if gwip == nil {
			llog.Trace("  ip of gateway (%s) is not parsable. skipping", re.Map["gateway"])
			continue
		}
		if gateway.Equal(gwip) && gwparts[1] == ifname {
			llog.Trace("  found a desired default route(.id=%s)", re.Map[".id"])
			return nil // no need to set route
		}
		llog.Trace("%s is not a desired gateway. continuing", re.Map["gateway"])
	}

	gw := fmt.Sprintf("%s%%%s", gateway, ifname)
	if modTarget != "" {
		llog.Info("Updating ROS default gateway: dst-address=::/0 gateway=%s", gw)
		rep, err = c.RunArgs([]string{
			"/ipv6/route/set",
			fmt.Sprintf("=.id=%s", modTarget),
			fmt.Sprintf("=gateway=%s", gw),
		})
		if err != nil {
			return err
		}
	} else {
		llog.Info("Adding ROS default gateway: dst-address=::/0 gateway=%s", gw)
		rep, err = c.RunArgs([]string{
			"/ipv6/route/add",
			"=dst-address=::/0",
			fmt.Sprintf("=gateway=%s", gw),
			fmt.Sprintf("=comment=%s", rosCommentKey),
		})
		if err != nil {
			return err
		}
	}

	return err
}

func (c *ROSClient) ExportIPv6Pool(name string, cidr net.IPNet, prefixlen int) error {
	llog.Trace("ExportIPv6Pool(name=%s, cidr=%s, prefixlen=%d)", name, cidr, prefixlen)
	// check if exists
	rep, err := c.RunArgs([]string{
		"/ipv6/pool/print",
		fmt.Sprintf("?name=%s", name),
	})
	if err != nil {
		return err
	}
	exists := false
	var id string
	if len(rep.Re) > 0 {
		exists = true
		props := rep.Re[0].Map
		id = props[".id"]
		if cidrEqual(props["prefix"], cidr) &&
			props["prefix-length"] == fmt.Sprintf("%d", prefixlen) {
			llog.Trace("  %s is in desired state", name)
			return nil
		}
	}

	if exists {
		_, err = c.RunArgs([]string{
			"/ipv6/pool/set",
			fmt.Sprintf("=.id=%s", id),
			fmt.Sprintf("=prefix=%s", cidr.String()),
			fmt.Sprintf("=prefix-length=%d", prefixlen),
		})
	} else {
		_, err = c.RunArgs([]string{
			"/ipv6/pool/add",
			fmt.Sprintf("=name=%s", name),
			fmt.Sprintf("=prefix=%s", cidr.String()),
			fmt.Sprintf("=prefix-length=%d", prefixlen),
		})
	}

	return err
}

func (c *ROSClient) LookupNeighbor(ip net.IP, timeoutms int, strict bool) (net.HardwareAddr, error) {
	llog.Trace("LookupNeighbor(ip=%s, timeout=%d, strict=%v)", ip, timeoutms, strict)

	llog.Trace("  pinging the client")
	ping := func() (*routeros.Reply, error) {
		return c.RunArgs([]string{
			"/ping",
			fmt.Sprintf("=address=%s", ip.String()),
			"=count=1",
			fmt.Sprintf("=interval=00:00:0%d.%03d", timeoutms/1000, timeoutms%1000),
		})
	}
	pingrep, err := ping()
	if err != nil {
		return nil, err
	}

	pingSuccess := len(pingrep.Re) > 0 && pingrep.Re[0].Map["packet-loss"] == "0"
	llog.Trace("  ping success: %v", pingSuccess)

	// lookup neighbor entry (cached)
	llog.Trace("  lookup the neighbor entry")
	rep, err := c.RunArgs([]string{
		"/ipv6/neighbor/print",
		"=.proplist=mac-address,status",
		fmt.Sprintf("?address=%s", ip.String()),
	})
	if err != nil {
		return nil, err
	}
	if len(rep.Re) > 0 && (rep.Re[0].Map["status"] == "reachable" || rep.Re[0].Map["status"] == "stale") {
		hwaddr, err := net.ParseMAC(rep.Re[0].Map["mac-address"])
		// trigger (to update cache)
		go ping()
		return hwaddr, err
	}

	// trigger neighbor discovery by pinging
	_, err = c.RunArgs([]string{
		"/ping",
		fmt.Sprintf("=address=%s", ip.String()),
		"=count=1",
		fmt.Sprintf("=interval=00:00:0%d.%03d", timeoutms/1000, timeoutms%1000),
	})
	if err != nil {
		return nil, err
	}

	// lookup neighbor entry
	rep, err = c.RunArgs([]string{
		"/ipv6/neighbor/print",
		"=.proplist=mac-address,status",
		fmt.Sprintf("?address=%s", ip.String()),
	})
	if err != nil {
		return nil, err
	}
	if len(rep.Re) == 0 || rep.Re[0].Map["status"] != "reachable" {
		if !strict && pingSuccess {
			llog.Trace("  neighbor entry not found but report success since pinging succeeded")
			return make(net.HardwareAddr, 6), nil
		}
		llog.Trace("  neighbor entry not found")
		return nil, nil
	}
	hwaddr, err := net.ParseMAC(rep.Re[0].Map["mac-address"])
	return hwaddr, err
}

func (c *ROSClient) AssignIPv6(ifname string, ip *net.IPNet, key string, options ROSIPOptions) error {
	comment := fmt.Sprintf("%s %s", rosCommentKey, key)

	// check ip assignment state
	rep, err := c.RunArgs([]string{
		"/ipv6/address/print",
		"=.proplist=.id,comment,address,advertise,eui-64",
		"?comment",
		"?dynamic=false",
		"?#&",
		fmt.Sprintf("?interface=%s", ifname),
		"?#&",
	})
	if err != nil {
		return err
	}
	id := ""
	for _, s := range rep.Re {
		props := s.Map
		if props["comment"] != comment {
			continue
		}
		id = props[".id"]
		if cidrEqual(props["address"], *ip) &&
			props["eui-64"] == strconv.FormatBool(options.Eui64) &&
			props["advertise"] == strconv.FormatBool(options.Advertise) {
			return nil
		}
	}

	// assign or update ip if necessarry
	if id != "" {
		_, err = c.RunArgs([]string{
			"/ipv6/address/set",
			fmt.Sprintf("=.id=%s", id),
			fmt.Sprintf("=address=%s", ip.String()),
			fmt.Sprintf("=advertise=%t", options.Advertise),
			fmt.Sprintf("=eui-64=%t", options.Eui64),
		})
	} else {
		_, err = c.RunArgs([]string{
			"/ipv6/address/add",
			fmt.Sprintf("=interface=%s", ifname),
			fmt.Sprintf("=address=%s", ip.String()),
			fmt.Sprintf("=advertise=%t", options.Advertise),
			fmt.Sprintf("=eui-64=%t", options.Eui64),
			fmt.Sprintf("=comment=%s", comment),
		})
	}

	return err
}
