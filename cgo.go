package main

// #include <net/if.h>
// #include <sys/types.h>
// #include <sys/socket.h>
// #include <netpacket/packet.h>
// #include <string.h>
import "C"
import (
	"syscall"
	"unsafe"
)

func ifNameToIndex(name string) (int, error) {
	index, err := C.if_nametoindex(C.CString(name))
	if index == 0 {
		return 0, err
	}
	return int(index), nil
}

func bindToDeviceRaw(fd int, ifindex int) error {
	lladdr := C.struct_sockaddr_ll{}
	C.memset(unsafe.Pointer(&lladdr), 0xff, C.sizeof_struct_sockaddr_ll)
	lladdr.sll_family = C.ushort(syscall.AF_PACKET)
	lladdr.sll_protocol = C.ushort(htons(syscall.ETH_P_IPV6))
	lladdr.sll_ifindex = C.int(ifindex)
	if _, err := C.bind(C.int(fd), (*C.struct_sockaddr)(unsafe.Pointer(&lladdr)), C.sizeof_struct_sockaddr_ll); err != nil {
		return err
	}
	return nil
}
