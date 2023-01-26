package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

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
	c *routeros.Client
}

func NewROSClient(cfg ROSConnectConfig) (*ROSClient, error) {
	var client *routeros.Client
	var err error
	address := fmt.Sprintf("%s:%d", cfg.host, cfg.port)

	if cfg.useTLS {
		allCiphers := []uint16{}
		for _, s := range tls.CipherSuites() {
			allCiphers = append(allCiphers, s.ID)
		}
		allCiphers = append(allCiphers, 0x006d)
		client, err = routeros.DialTLS(address, cfg.username, cfg.password, &tls.Config{InsecureSkipVerify: true})
	} else {
		client, err = routeros.Dial(address, cfg.username, cfg.password)
	}

	if err != nil {
		return nil, err
	}

	return &ROSClient{c: client}, nil
}

func (c *ROSClient) GetInterfaceExists(name string) (bool, error) {
	rep, err := c.c.RunArgs([]string{
		"/interface/print",
		"=.proplist=",
		fmt.Sprintf("?name=%s", name),
	})
	if err != nil {
		return false, err
	}
	return len(rep.Re) > 0, nil
}

func (c *ROSClient) SetIPv6Gateway(ifname string, gateway net.IP) error {
	// check if route exists
	rep, err := c.c.RunArgs([]string{
		"/ipv6/route/print",
		"=.proplist=.id,gateway,comment",
		"?dst-address=::/0",
	})
	if err != nil {
		return err
	}
	var modTarget string
	for _, re := range rep.Re {
		if re.Map["comment"] == rosCommentKey {
			modTarget = re.Map[".id"]
		}
		gwparts := strings.Split(re.Map["gateway"], "%")
		if len(gwparts) != 2 {
			continue
		}
		gwip := net.ParseIP(gwparts[0])
		if gwip == nil {
			continue
		}
		if gateway.Equal(gwip) && gwparts[1] == ifname {
			return nil // no need to set route
		}
	}

	gw := fmt.Sprintf("%s%%%s", gateway, ifname)
	if modTarget != "" {
		log.Printf("Updating ROS default gateway: dst-address=::/0 gateway=%s", gw)
		rep, err = c.c.RunArgs([]string{
			"/ipv6/route/set",
			fmt.Sprintf("=.id=%s", modTarget),
			fmt.Sprintf("=gateway=%s", gw),
		})
		if err != nil {
			return err
		}
	} else {
		log.Printf("Adding ROS default gateway: dst-address=::/0 gateway=%s", gw)
		rep, err = c.c.RunArgs([]string{
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
	// check if exists
	rep, err := c.c.RunArgs([]string{
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
			log.Printf("%s is in desired state", name)
			return nil
		}
	}

	if exists {
		_, err = c.c.RunArgs([]string{
			"/ipv6/pool/set",
			fmt.Sprintf("=.id=%s", id),
			fmt.Sprintf("=prefix=%s", cidr.String()),
			fmt.Sprintf("=prefix-length=%d", prefixlen),
		})
	} else {
		_, err = c.c.RunArgs([]string{
			"/ipv6/pool/add",
			fmt.Sprintf("=name=%s", name),
			fmt.Sprintf("=prefix=%s", cidr.String()),
			fmt.Sprintf("=prefix-length=%d", prefixlen),
		})
	}

	return err
}

func (c *ROSClient) LookupNeighbor(ip net.IP, timeoutms int) (net.HardwareAddr, error) {
	ping := func() error {
		_, err := c.c.RunArgs([]string{
			"/ping",
			fmt.Sprintf("=address=%s", ip.String()),
			"=count=1",
			fmt.Sprintf("=interval=00:00:0%d.%03d", timeoutms/1000, timeoutms%1000),
		})
		if err != nil {
			return err
		}
		return nil
	}

	// lookup neighbor entry (cached)
	rep, err := c.c.RunArgs([]string{
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
	_, err = c.c.RunArgs([]string{
		"/ping",
		fmt.Sprintf("=address=%s", ip.String()),
		"=count=1",
		fmt.Sprintf("=interval=00:00:0%d.%03d", timeoutms/1000, timeoutms%1000),
	})
	if err != nil {
		return nil, err
	}

	// lookup neighbor entry
	rep, err = c.c.RunArgs([]string{
		"/ipv6/neighbor/print",
		"=.proplist=mac-address,status",
		fmt.Sprintf("?address=%s", ip.String()),
	})
	if err != nil {
		return nil, err
	}
	if len(rep.Re) == 0 || rep.Re[0].Map["status"] != "reachable" {
		return nil, nil
	}
	hwaddr, err := net.ParseMAC(rep.Re[0].Map["mac-address"])
	return hwaddr, err
}

func (c *ROSClient) AssignIPv6(ifname string, ip *net.IPNet, key string, options ROSIPOptions) error {
	comment := fmt.Sprintf("%s %s", rosCommentKey, key)

	// check ip assignment state
	rep, err := c.c.RunArgs([]string{
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
		_, err = c.c.RunArgs([]string{
			"/ipv6/address/set",
			fmt.Sprintf("=.id=%s", id),
			fmt.Sprintf("=address=%s", ip.String()),
			fmt.Sprintf("=advertise=%t", options.Advertise),
			fmt.Sprintf("=eui-64=%t", options.Eui64),
		})
	} else {
		_, err = c.c.RunArgs([]string{
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
