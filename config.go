package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	prefixes  []*net.IPNet
	ifnames   []string
	routerIPs []net.IP
	routerMAC net.HardwareAddr

	raConfig *RAConfig
}

func loadPrefixes() ([]*net.IPNet, error) {
	prefixesStr := os.Getenv("ND_PREFIXES")
	prefixes := strings.Split(prefixesStr, ",")
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("ND_PREFIXES must have at least 1 IPv6 Prefix")
	}

	var ps []*net.IPNet

	for _, p := range prefixes {
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("Invalid IPv6 Prefix in ND_PREFIXES: %s", err)
		}
		ps = append(ps, n)
	}

	return ps, nil
}

func loadInterfaces() []string {
	ifsStr := os.Getenv("ND_INTERFACES")
	return strings.Split(ifsStr, ",")
}

func loadRouterInfo(cfg *Config) error {
	rmac, err := net.ParseMAC(os.Getenv("ND_ROUTERMAC"))
	if err != nil {
		return fmt.Errorf("Failed to parse ND_ROUTERMAC: %s", err)
	}
	cfg.routerMAC = rmac

	ripstrs := strings.Split(os.Getenv("ND_ROUTERIPS"), ",")
	if len(ripstrs) == 0 {
		return fmt.Errorf("ND_ROUTERIPS must have at least 1 IPv6 Address")
	}
	for _, i := range ripstrs {
		rip := net.ParseIP(i)
		if rip == nil || rip.To16() == nil {
			return fmt.Errorf("Invalid IPv6 Address in ND_ROUTERIPS: %s", i)
		}
		cfg.routerIPs = append(cfg.routerIPs, rip)
	}

	return nil
}

func loadROSConfig() (ROSConnectConfig, error) {
	var cfg ROSConnectConfig

	cfg.host = os.Getenv("ROS_HOST")
	if cfg.host == "" {
		return cfg, fmt.Errorf("you must specify the routerboard api endpoint as ROS_HOST")
	}
	port := os.Getenv("ROS_PORT")
	if port != "" {
		portnum, err := strconv.Atoi(port)
		if err != nil {
			cfg.port = portnum
		}
	}
	cfg.username = os.Getenv("ROS_USER")
	if cfg.host == "" {
		cfg.username = "admin"
	}
	cfg.password = os.Getenv("ROS_PASSWORD")
	useTLS := os.Getenv("ROS_USETLS")
	if useTLS == "1" {
		cfg.useTLS = true
	}

	if cfg.port == 0 {
		if cfg.useTLS {
			cfg.port = 8729
		} else {
			cfg.port = 8728
		}
	}

	return cfg, nil
}

func loadRAConfig() (*RAConfig, error) {
	var cfg RAConfig

	mode := os.Getenv("RA_MODE")
	if mode == "" {
		mode = "ros"
	}
	if mode != "ros" && mode != "off" {
		return nil, fmt.Errorf("invalid RA_MODE '%s'", mode)
	}

	cfg.mode = mode
	if mode == "off" {
		return &cfg, nil
	}

	cfg.extIfs = strings.Split(os.Getenv("RA_EXTERNAL_INTERFACES"), ",")
	timeoutStr := os.Getenv("RA_TIMEOUT")
	if timeoutStr == "" {
		timeoutStr = "5000"
	}
	timeout, err := strconv.Atoi(timeoutStr)
	if err != nil {
		return nil, fmt.Errorf("invalid RA_TIMEOUT: %s", err)
	}
	cfg.timeout = time.Millisecond * time.Duration(timeout)

	if mode == "ros" {
		cfg.rosExtIf = os.Getenv("RA_ROS_EXTERNAL_INTERFACE")
		extIpStrs := strings.Split(os.Getenv("RA_ROS_EXTERNAL_IPS"), ",")
		for _, eipstr := range extIpStrs {
			if eipstr == "" {
				continue
			}
			eip, err := ParseROSIPAssign(eipstr, cfg.rosExtIf)
			if err != nil {
				return nil, fmt.Errorf("Invalid ROS external ip: %s", err)
			}
			cfg.rosExtIPs = append(cfg.rosExtIPs, eip)
		}
		intIpStrs := strings.Split(os.Getenv("RA_ROS_INTERNAL_IPS"), ",")
		for _, iipstr := range intIpStrs {
			if iipstr == "" {
				continue
			}
			iip, err := ParseROSIPAssign(iipstr, cfg.rosExtIf)
			if err != nil {
				return nil, fmt.Errorf("Invalid ROS internal ip: %s", err)
			}
			cfg.rosIntIPs = append(cfg.rosIntIPs, iip)
		}
		poolStr := os.Getenv("RA_ROS_POOLS")
		if poolStr == "" {
			poolStr = "ra-prefix@fletsv6-pool/64"
		}
		if poolStr != "none" {
			poolStrs := strings.Split(poolStr, ",")
			for _, poolstr := range poolStrs {
				if poolstr == "" {
					continue
				}
				pool, err := ParseROSPoolAssign(poolstr)
				if err != nil {
					return nil, fmt.Errorf("Invalid ROS pool: %s", err)
				}
				cfg.rosPools = append(cfg.rosPools, pool)
			}
		}
	}

	return &cfg, nil
}

// second return value is needROS
func loadNDConfig(racfg *RAConfig) (*NDConfig, bool, error) {
	cfg := &NDConfig{}
	needROS := false

	cfg.mode = os.Getenv("NDP_MODE")
	if cfg.mode == "" {
		cfg.mode = "proxy-ros"
	}
	if cfg.mode != "off" &&
		cfg.mode != "static" &&
		cfg.mode != "proxy" &&
		cfg.mode != "proxy-ros" &&
		cfg.mode != "proxy-ros:strict" {
		return nil, false, fmt.Errorf("Unknown NDP_MODE %s", cfg.mode)
	}

	if cfg.mode == "off" {
		return cfg, false, nil
	}
	if cfg.mode == "proxy-ros" {
		needROS = true
	}

	prefixes := os.Getenv("NDP_PREFIXES")
	if prefixes == "" {
		prefixes = "ra-prefix"
	}
	for _, prefixStr := range strings.Split(prefixes, ",") {
		fip, err := ParseFlexibleIP(prefixStr)
		if err != nil {
			return nil, false, fmt.Errorf("Error while reading NDP_PREFIXES: %s", err)
		}
		if fip.raPrefix && racfg.mode == "off" {
			return nil, false, fmt.Errorf("You cannot use ra-prefix in NDP_PREFIXES while you set RA_MODE=off")
		}
		cfg.prefixes = append(cfg.prefixes, fip)
	}

	excludeIps := os.Getenv("NDP_EXCLUDE_IPS")
	if excludeIps == "" {
		excludeIps = "ra-externalips"
	}
	if excludeIps != "none" {
		for _, ipStr := range strings.Split(excludeIps, ",") {
			if ipStr == "ra-externalips" {
				for _, assign := range racfg.rosExtIPs {
					cfg.excludes = append(cfg.excludes, assign.ip)
				}
			} else if ipStr == "ra-internalips" {
				for _, assign := range racfg.rosIntIPs {
					cfg.excludes = append(cfg.excludes, assign.ip)
				}
			} else {
				fip, err := ParseFlexibleIP(ipStr)
				if err != nil {
					return nil, false, fmt.Errorf("Error while reading NDP_EXCLUDE_IPS: %s", err)
				}
				cfg.excludes = append(cfg.excludes, fip)
			}
		}
	}

	extIfs := os.Getenv("NDP_EXTERNAL_INTERFACES")
	if extIfs == "" {
		extIfs = "eth0"
	}
	cfg.extIfs = strings.Split(extIfs, ",")

	if cfg.mode == "proxy" {
		intIfs := os.Getenv("NDP_INTERNAL_INTERFACES")
		if intIfs == "" {
			return nil, false, fmt.Errorf("You must specify at least one interface in NDP_INTERNAL_INTERFACES to use NDP_MODE=proxy")
		}
		cfg.intIfs = strings.Split(intIfs, ",")
	}

	timeoutStr := os.Getenv("NDP_TIMEOUT")
	if timeoutStr == "" {
		timeoutStr = "1000"
	}
	timeoutMs, err := strconv.Atoi(timeoutStr)
	if err != nil {
		return nil, false, fmt.Errorf("NDP_TIMEOUT is not a valid integer")
	}
	if cfg.mode == "proxy-ros" && (timeoutMs < 10 || timeoutMs > 5000) {
		return nil, false, fmt.Errorf("NDP_TIMEOUT is out of range")
	}
	cfg.timeoutMs = timeoutMs

	advMACs := os.Getenv("NDP_ADVERTISE_MACS")
	if advMACs == "" {
		advMACs = "@@external"
	}
	for _, advMAC := range strings.Split(advMACs, ",") {
		if len(advMAC) == 0 {
			continue
		}
		if advMAC[0] == '@' {
			cfg.advMACs = append(cfg.advMACs, MACRef{rosIf: advMAC[1:]})
			needROS = true
		} else {
			hwaddr, err := net.ParseMAC(advMAC)
			if err != nil {
				return nil, false, fmt.Errorf("[WARNING] Invalid MAC Address %s in NDP_ADVERTISE_MACS", advMAC)
			}
			cfg.advMACs = append(cfg.advMACs, MACRef{hwaddr: hwaddr})
		}
	}

	return cfg, needROS, nil
}

func loadConfig(cfg *Config) error {
	prefixes, err := loadPrefixes()
	if err != nil {
		return err
	}
	cfg.prefixes = prefixes

	cfg.ifnames = loadInterfaces()

	return loadRouterInfo(cfg)
}

// parsers
func ParseFlexibleIP(ipstr string) (FlexibleIP, error) {
	var i FlexibleIP
	if strings.HasPrefix(ipstr, "ra-prefix") {
		ipstr = strings.TrimPrefix(ipstr, "ra-prefix")
		i.raPrefix = true
		if ipstr == "" {
			i.cidr = -1
			return i, nil
		}
		ipstr = "0:0:0:0" + ipstr
	}
	ip, cidr, err := net.ParseCIDR(ipstr)
	if err != nil {
		i.ip = net.ParseIP(ipstr)
		if i.ip == nil {
			return i, err
		}
		i.cidr = 128
		return i, nil
	}
	i.ip = ip
	i.cidr, _ = cidr.Mask.Size()
	return i, nil
}

var rosipreg = regexp.MustCompile("^([^@]+)@(@external|[^@:]+)((?::[-a-z0-9]+)*)$")

func ParseROSIPAssign(config string, extif string) (ROSIPAssign, error) {
	var a ROSIPAssign

	parsed := rosipreg.FindStringSubmatch(config)
	if parsed == nil {
		return a, fmt.Errorf("ip assignment '%s' has invalid format", config)
	}

	ipstr := parsed[1]
	ifstr := parsed[2]
	options := strings.Split(parsed[3], ":")[1:]

	// ip
	fip, err := ParseFlexibleIP(ipstr)
	if err != nil {
		return a, fmt.Errorf("ip assignment '%s' has invalid ip specifier: %s", config, err)
	}

	// interface
	if ifstr == "@external" {
		if extif == "" {
			return a, fmt.Errorf("ip assignment '%s' has @external but RA_ROS_EXTERNAL_INTERFACE is empty", config)
		} else {
			ifstr = extif
		}
	}

	// options
	for _, opt := range options {
		if opt == "eui-64" {
			a.options.Eui64 = true
		} else if opt == "advertise" {
			a.options.Advertise = true
		}
	}

	a.ip = fip
	a.ifname = ifstr

	return a, nil
}

func ParseROSPoolAssign(config string) (ROSPoolAssign, error) {
	var a ROSPoolAssign

	parts := strings.Split(config, "@")
	if len(parts) != 2 {
		return a, fmt.Errorf("pool assignment '%s' has invalid pool specifier", config)
	}

	fip, err := ParseFlexibleIP(parts[0])
	if err != nil {
		return a, fmt.Errorf("pool assignment '%s' has invalid ip specifier: %s", config, err)
	}

	poolparts := strings.Split(parts[1], "/")
	if len(poolparts) != 2 {
		return a, fmt.Errorf("pool assignment '%s' has invalid pool specifier", config)
	}

	prefixLength, err := strconv.Atoi(poolparts[1])
	if err != nil {
		return a, fmt.Errorf("pool assignment: '%s' has invalid prefix specifier: %s", config, err)
	}

	a.ip = fip
	a.poolname = poolparts[0]
	a.prefixLength = prefixLength

	return a, nil
}
