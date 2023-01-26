package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

type DecodedInterface struct {
	name string
	vlan int
}

func NewDecodedInterface(ifname string) (DecodedInterface, error) {
	var i DecodedInterface
	parts := strings.Split(ifname, "@")
	if len(parts) == 1 {
		i.name = ifname
		return i, nil
	} else if len(parts) == 2 {
		vlan, err := strconv.Atoi(parts[1])
		if err != nil {
			return i, fmt.Errorf("Malformed interface name %s", ifname)
		}
		i.name = parts[0]
		i.vlan = vlan
		return i, nil
	} else {
		return i, fmt.Errorf("Malformed interface name %s", ifname)
	}
}

func (i DecodedInterface) ActualName() string {
	if i.vlan == 0 {
		return i.name
	} else {
		return fmt.Sprintf("%s.vlan%d", i.name, i.vlan)
	}
}

func (i DecodedInterface) Exists() bool {
	if i.vlan == 0 {
		_, err := ifNameToIndex(i.name)
		return err == nil
	} else {
		if err := i.PrepareVLAN(); err != nil {
			log.Printf("[WARNING] Failed to prepare vlan %d on %s: %s", i.vlan, i.name, err)
			return false
		} else {
			return true
		}
	}
}

func (i DecodedInterface) PrepareVLAN() error {
	result := checkVlanDev(i.name, i.ActualName(), i.vlan)
	if result == 1 {
		return fmt.Errorf("%s is not a vlan %d device for %s", i.ActualName(), i.vlan, i.name)
	} else if result == 2 {
		return nil
	}
	return createVlanDev(i.name, i.ActualName(), i.vlan)
}

func (i DecodedInterface) Index() (int, error) {
	if i.vlan != 0 {
		if err := i.PrepareVLAN(); err != nil {
			return 0, err
		}
	}
	return ifNameToIndex(i.ActualName())
}

// interface utility
func findFirstInterface(ifnames []string) (*DecodedInterface, error) {
	for _, name := range ifnames {
		di, err := NewDecodedInterface(name)
		if err != nil {
			log.Printf("[WARNING] Ignoring malformed interface name %s", name)
			continue
		}
		if di.Exists() {
			return &di, nil
		}
	}
	return nil, fmt.Errorf("Could not resolve all of these interfaces: %s", strings.Join(ifnames, ","))
}

func collectInterfaces(ifnames []string) (map[string]DecodedInterface, error) {
	dis := make(map[string]DecodedInterface)

	for _, name := range ifnames {
		di, err := NewDecodedInterface(name)
		if err != nil {
			log.Printf("[WARNING] Ignoring malformed interface name %s", name)
			continue
		}
		if di.Exists() {
			dis[name] = di
		}
	}

	if len(dis) == 0 {
		return nil, fmt.Errorf("Could not resolve all of these interfaces: %s", strings.Join(ifnames, ","))
	} else {
		return dis, nil
	}
}

// VLAN utility
func createVlanDev(linkname string, devname string, id int) error {
	master, err := netlink.LinkByName(linkname)
	if err != nil {
		return fmt.Errorf("netlink.LinkByName(\"%s\") failed: %s", linkname, err)
	}
	la := netlink.NewLinkAttrs()
	la.Name = devname
	la.ParentIndex = master.Attrs().Index
	vlan := &netlink.Vlan{LinkAttrs: la, VlanId: id}
	err = netlink.LinkAdd(vlan)
	if err != nil {
		return fmt.Errorf("netlink.LinkAdd failed: %s", err)
	}
	return netlink.LinkSetUp(vlan)
}

func checkVlanDev(linkname string, devname string, id int) int {
	vlink, err := netlink.LinkByName(devname)
	if err != nil {
		return 0
	}
	vlan, isVlan := vlink.(*netlink.Vlan)
	if !isVlan {
		return 1
	}
	if vlan.VlanId != id {
		return 1
	}
	master, err := netlink.LinkByName(linkname)
	if err != nil {
		return 1
	}
	if master.Attrs().Index != vlan.Attrs().ParentIndex {
		return 1
	}
	return 2
}

func deleteVlanDev(devname string) error {
	vlan, err := netlink.LinkByName(devname)
	if err != nil {
		return nil
	}
	return netlink.LinkDel(vlan)
}
