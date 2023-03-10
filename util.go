package main

import (
	"net"

	"golang.org/x/net/bpf"
)

func htonl(num int) int {
	un := uint32(num)
	return int((un >> 24) | (((un >> 16) & 0xFF) << 8) | (((un >> 8) & 0xFF) << 16) | ((un & 0xFF) << 24))
}

func htons(num int) int {
	un := uint16(num)
	return int((un >> 8) | ((un & 0xFF) << 8))
}

func bytes2short(bytes []byte) uint32 {
	return (uint32)(((uint32)(bytes[0]) << 8) | (uint32)(bytes[1]))
}
func bytes2int(bytes []byte) uint32 {
	return (uint32)(((uint32)(bytes[0]) << 24) | ((uint32)(bytes[1]) << 16) | ((uint32)(bytes[2]) << 8) | (uint32)(bytes[3]))
}

// Give id=0 to filter out all tagged vlan packets
func bpfWithDot1Q(org []bpf.Instruction, id int) []bpf.Instruction {
	// generate preamble
	if id == 0 {
		is := []bpf.Instruction{
			bpf.LoadAbsolute{Off: 12, Size: 2},
			bpf.JumpIf{Val: 0x8100, SkipTrue: 2},
			bpf.JumpIf{Val: 0x88a8, SkipTrue: 1},
			bpf.JumpIf{Val: 0x9100, SkipFalse: 1},
			bpf.RetConstant{Val: 0},
		}
		return append(is, org...)
	} else {
		is := []bpf.Instruction{
			bpf.LoadAbsolute{Off: 12, Size: 2},
			bpf.JumpIf{Val: 0x8100, SkipFalse: 3}, // tag stacking is not supported
			bpf.LoadAbsolute{Off: 14, Size: 2},
			bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 0xfff},
			bpf.JumpIf{Val: uint32(id), SkipTrue: 1},
			bpf.RetConstant{Val: 0},
		}
		palen := len(is)

		is = append(is, org...)

		// shift load offset
		for i := palen; i < len(is); i++ {
			load, ok := is[i].(bpf.LoadAbsolute)
			if ok && load.Off >= 12 {
				load.Off += 4
				is[i] = load // 4 bytes for 802.1q header
			}
		}

		return is
	}
}

type Interface struct {
	fd    int
	index int
	name  string
}

func cidrEqual(cidrstr string, cidr net.IPNet) bool {
	ip2, cidr2, err := net.ParseCIDR(cidrstr)
	cidr2.IP = ip2
	if err == nil && cidr.String() == cidr2.String() {
		return true
	}
	return false
}

func invertMask(mask net.IPMask) net.IPMask {
	if len(mask) == 0 {
		return nil
	}

	imask := make(net.IPMask, len(mask))
	for i, m := range mask {
		imask[i] = ^m
	}

	return imask
}

func maskedIPAssign(dst net.IP, src net.IP, mask net.IPMask) {
	if len(dst) != len(src) || len(dst) != len(mask) {
		return
	}

	for i := 0; i < len(dst); i++ {
		dst[i] = (src[i] & mask[i]) | (dst[i] & ^mask[i])
	}
}

func multicastAddr(ip net.IP) (net.IP, net.HardwareAddr) {
	mcip := append(net.IP{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0x00, 0x00, 0x00, 0x01, 0xff}, ip[13:16]...)
	mcmac := append(net.HardwareAddr{0x33, 0x33}, mcip[12:16]...)
	return mcip, mcmac
}
