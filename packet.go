package main

import (
	"log"
	"net"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

var allRouterMAC = net.HardwareAddr{0x33, 0x33, 0x00, 0x00, 0x00, 0x02}
var allRouterIP = net.IP{0xFF, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02} // FD02::2
var unspecifiedIP = net.IP{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00} // ::

type ICMPv6Data[T gopacket.SerializableLayer] struct {
	SrcMAC net.HardwareAddr
	DstMAC net.HardwareAddr
	SrcIP  net.IP
	DstIP  net.IP
	Type   uint16
	Layer  T
}

func makeRouterSolicitation(addr net.IP, lladdr net.HardwareAddr) []byte {
	return makeICMPv6(ICMPv6Data[*layers.ICMPv6RouterSolicitation]{
		SrcMAC: lladdr,
		DstMAC: allRouterMAC,
		SrcIP:  addr,
		DstIP:  allRouterIP,
		Type:   133, // 133 Router Solicitation
		Layer: &layers.ICMPv6RouterSolicitation{
			Options: []layers.ICMPv6Option{
				{Type: 1, Data: lladdr}, // Source link-layer address (1)
			},
		},
	})
}

func makeICMPv6[T gopacket.SerializableLayer](data ICMPv6Data[T]) []byte {
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	eth := &layers.Ethernet{
		SrcMAC:       data.SrcMAC,
		DstMAC:       data.DstMAC,
		EthernetType: 0x86dd,
	}
	ip6 := &layers.IPv6{
		Version:      6,
		TrafficClass: 0xb8,
		FlowLabel:    0,
		Length:       0, // auto compute
		NextHeader:   0x3a,
		HopLimit:     255,
		SrcIP:        data.SrcIP,
		DstIP:        data.DstIP,
	}
	icmpv6 := &layers.ICMPv6{
		TypeCode: layers.ICMPv6TypeCode(data.Type << 8),
		Checksum: 0, // auto compute
	}
	icmpv6.SetNetworkLayerForChecksum(ip6)
	err := gopacket.SerializeLayers(buf, opts,
		eth, ip6, icmpv6, data.Layer,
	)
	if err != nil {
		log.Printf("failed to create packet: data=%+v, err=%s", data, err)
	}
	return buf.Bytes()
}

func parseICMPv6[T any, PT interface {
	gopacket.SerializableLayer
	gopacket.DecodingLayer
	*T
}](packet []byte, data *ICMPv6Data[PT]) error {
	var eth layers.Ethernet
	var ip6 layers.IPv6
	var icmp6 layers.ICMPv6
	var icmp6nd T
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip6, &icmp6, PT(&icmp6nd))
	decoded := []gopacket.LayerType{}

	if err := parser.DecodeLayers(packet, &decoded); err != nil {
		return err
	}
	data.SrcMAC = eth.SrcMAC
	data.DstMAC = eth.DstMAC
	data.SrcIP = ip6.SrcIP
	data.DstIP = ip6.DstIP
	data.Type = uint16(icmp6.TypeCode >> 8)
	data.Layer = &icmp6nd

	return nil
}
