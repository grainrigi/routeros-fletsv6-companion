package main

import (
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

type Socket struct {
	fd      int
	netif   *net.Interface
	isValid bool
}

type SocketReadResult struct {
	data []byte
	err  error
}

func NewSocket(ifindex int) (*Socket, error) {
	s := &Socket{}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_NONBLOCK, htons(syscall.ETH_P_IPV6))
	if err != nil {
		return nil, err
	}
	s.fd = fd

	netif, err := net.InterfaceByIndex(ifindex)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	s.netif = netif

	if err := bindToDeviceRaw(fd, ifindex); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}

	s.isValid = true

	return s, nil
}

func (s *Socket) ApplyBPF(is []bpf.RawInstruction) error {
	return applyBPF(s.fd, is)
}

func (s *Socket) readImmediate() ([]byte, error) {
	var buf [2048]byte
	n, err := syscall.Read(s.fd, buf[:])
	if err == syscall.EAGAIN {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else {
		return buf[:n], nil
	}
}

func (s *Socket) ReadOnce(timeout *time.Duration) ([]byte, error) {
	s2, err := epollOnce([]*Socket{s}, timeout)
	if err != nil {
		return nil, err
	}
	if !s.isValid {
		return nil, fmt.Errorf("the socket has been closed")
	}

	if s2 == nil {
		return nil, fmt.Errorf("Read timed out")
	}
	return s.readImmediate()
}

func (s *Socket) LinkLocal() net.IP {
	ips, err := s.netif.Addrs()
	if err != nil {
		llog.Warning("s.netif.Addrs() failed %s", err)
		return nil
	}
	errs := []string{}
	for _, addr := range ips {
		ip, isip := addr.(*net.IPNet)
		if isip {
			if len(ip.IP) == 16 && ip.IP.IsLinkLocalUnicast() {
				return ip.IP
			} else {
				errs = append(errs, fmt.Sprintf("%s is not link local unicast", ip))
			}
		} else {
			errs = append(errs, fmt.Sprintf("%T is not IPNet", addr))
		}
	}
	llog.Warning("%s has no link local address: %s", s.netif.Name, strings.Join(errs, ","))
	return nil
}

func (s *Socket) ReadOnceChan(timeout *time.Duration) <-chan SocketReadResult {
	ch := make(chan SocketReadResult, 1)

	go func() {
		defer close(ch)
		b, err := s.ReadOnce(timeout)
		ch <- SocketReadResult{data: b, err: err}
	}()

	return ch
}

func (s *Socket) WriteOnce(packet []byte) error {
	_, err := syscall.Write(s.fd, packet)
	return err
}

func (s *Socket) FlushAll() {
	for {
		data, err := s.readImmediate()
		if data == nil || err != nil {
			return
		}
	}
}

func ReadMultiSocksOnce(socks []*Socket, timeout *time.Duration) (int, []byte, error) {
	s, err := epollOnce(socks, timeout)
	if err != nil {
		return -1, nil, err
	} else if s == nil {
		return -1, nil, nil // timeout
	}

	for i, so := range socks {
		if so == s {
			data, err := so.readImmediate()
			return i, data, err
		}
	}

	// would not reach here
	return -1, nil, fmt.Errorf("Unexpected code reached")
}

func (s *Socket) ClearBuf() error {
	var buf [2048]byte
	for {
		_, err := syscall.Read(s.fd, buf[:])
		if err == syscall.EAGAIN {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func (s *Socket) Close() error {
	if !s.isValid {
		return nil
	}
	s.isValid = false
	return syscall.Close(s.fd)
}

func (s *Socket) onClosed() {
	s.isValid = false
}

// epoll
func epollOnce(socks []*Socket, timeout *time.Duration) (*Socket, error) {
	var timeoutMs int
	if timeout == nil {
		timeoutMs = -1
	} else {
		timeoutMs = int(*timeout / time.Millisecond)
	}

	events := make([]syscall.EpollEvent, len(socks))

	epfd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, fmt.Errorf("EpollCreate1 failed: %s", err)
	}

	for _, s := range socks {
		event := syscall.EpollEvent{
			Events: syscall.EPOLLIN,
			Fd:     int32(s.fd),
		}
		if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, s.fd, &event); err != nil {
			return nil, fmt.Errorf("EPOLL_CTL_ADD failed: %s", err)
		}
	}

	var readSock *Socket

	remainsock := len(socks)
retry:
	nevents, err := syscall.EpollWait(epfd, events[:], timeoutMs)
	if err != nil {
		return nil, fmt.Errorf("EpollWait failed: %s", err)
	}

	for _, ev := range events[:nevents] {
		fd := int(ev.Fd)
		var sock *Socket
		for _, s := range socks {
			if s.fd == fd {
				sock = s
			}
		}
		if sock == nil {
			continue
		}
		if (ev.Events&syscall.EPOLLERR) != 0 || (ev.Events&syscall.EPOLLHUP) != 0 {
			sock.onClosed()
			_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, fd, nil)
			remainsock--
			if remainsock > 0 {
				goto retry
			}
		}
		if (ev.Events&syscall.EPOLLIN) != 0 && readSock == nil {
			readSock = sock
		}
	}

	_ = syscall.Close(epfd)

	return readSock, nil
}

// util
func applyBPF(fd int, is []bpf.RawInstruction) error {
	// from: https://riyazali.net/posts/berkeley-packet-filter-in-golang/
	program := unix.SockFprog{
		Len:    uint16(len(is)),
		Filter: (*unix.SockFilter)(unsafe.Pointer(&is[0])),
	}
	b := (*[unix.SizeofSockFprog]byte)(unsafe.Pointer(&program))[:unix.SizeofSockFprog]

	if _, _, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT,
		uintptr(fd), uintptr(syscall.SOL_SOCKET), uintptr(syscall.SO_ATTACH_FILTER),
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0); errno != 0 {
		return errno
	}

	return nil
}

// filters
func bpfMulticast(not bool) []bpf.RawInstruction {
	if not {
		is, _ := bpf.Assemble([]bpf.Instruction{
			bpf.LoadAbsolute{Off: 0, Size: 2},     // Load ether dst[0:2]
			bpf.JumpIf{Val: 0x3333, SkipFalse: 3}, // dst[0:2] == 33:33 (IPv6 MultiCast)
			bpf.LoadAbsolute{Off: 12, Size: 2},    // Load EtherType
			bpf.JumpIf{Val: 0x86dd, SkipFalse: 1}, // EtherType == 0x86dd (IPv6)
			bpf.RetConstant{Val: 0},
			bpf.RetConstant{Val: 262144},
		})
		return is
	} else {
		is, _ := bpf.Assemble([]bpf.Instruction{
			bpf.LoadAbsolute{Off: 0, Size: 2},     // Load ether dst[0:2]
			bpf.JumpIf{Val: 0x3333, SkipFalse: 3}, // dst[0:2] == 33:33 (IPv6 MultiCast)
			bpf.LoadAbsolute{Off: 12, Size: 2},    // Load EtherType
			bpf.JumpIf{Val: 0x86dd, SkipFalse: 1}, // EtherType == 0x86dd (IPv6)
			bpf.RetConstant{Val: 262144},
			bpf.RetConstant{Val: 0},
		})
		return is
	}
}

func bpfRA() []bpf.RawInstruction {
	insn, _ := bpf.Assemble([]bpf.Instruction{
		// from tcpdump -d "icmp6[0] == 134"
		bpf.LoadAbsolute{Off: 12, Size: 2},    // Load EtherType
		bpf.JumpIf{Val: 0x86dd, SkipFalse: 5}, // EtherType == 0x86dd (IPv6)
		bpf.LoadAbsolute{Off: 20, Size: 1},    // Load IPv6 Next Header
		bpf.JumpIf{Val: 0x3a, SkipFalse: 3},   // Next Header = 0x3a (ICMPv6)
		bpf.LoadAbsolute{Off: 54, Size: 1},    // Load ICMPv6 Type
		bpf.JumpIf{Val: 134, SkipFalse: 1},    // Type == 0x84 (Router Advertisement)
		bpf.RetConstant{Val: 262144},
		bpf.RetConstant{Val: 0},
	})
	return insn
}

func bpfND() []bpf.RawInstruction {
	insn, _ := bpf.Assemble([]bpf.Instruction{
		// from tcpdump -d "ether[0:2] == 0x3333 and icmp6[0] == 135"
		bpf.LoadAbsolute{Off: 0, Size: 2},     // Load ether dst[0:2]
		bpf.JumpIf{Val: 0x3333, SkipFalse: 7}, // dst[0:2] == 33:33 (IPv6 MultiCast)
		bpf.LoadAbsolute{Off: 12, Size: 2},    // Load EtherType
		bpf.JumpIf{Val: 0x86dd, SkipFalse: 5}, // EtherType == 0x86dd (IPv6)
		bpf.LoadAbsolute{Off: 20, Size: 1},    // Load IPv6 Next Header
		bpf.JumpIf{Val: 0x3a, SkipFalse: 3},   // Next Header = 0x3a (ICMPv6)
		bpf.LoadAbsolute{Off: 54, Size: 1},    // Load ICMPv6 Type
		bpf.JumpIf{Val: 135, SkipFalse: 1},    // Type == 0x87 (Neighbor Solicitation)
		bpf.RetConstant{Val: 262144},
		bpf.RetConstant{Val: 0},
	})
	return insn
}

func bpfICMPv6(id int) []bpf.RawInstruction {
	insn, _ := bpf.Assemble([]bpf.Instruction{
		// from tcpdump -d "ether[0:2] == 0x3333 and icmp6[0] == 135"
		bpf.LoadAbsolute{Off: 12, Size: 2},        // Load EtherType
		bpf.JumpIf{Val: 0x86dd, SkipFalse: 5},     // EtherType == 0x86dd (IPv6)
		bpf.LoadAbsolute{Off: 20, Size: 1},        // Load IPv6 Next Header
		bpf.JumpIf{Val: 0x3a, SkipFalse: 3},       // Next Header = 0x3a (ICMPv6)
		bpf.LoadAbsolute{Off: 54, Size: 1},        // Load ICMPv6 Type
		bpf.JumpIf{Val: uint32(id), SkipFalse: 1}, // Type == 0x87 (Neighbor Solicitation)
		bpf.RetConstant{Val: 262144},
		bpf.RetConstant{Val: 0},
	})
	return insn
}
