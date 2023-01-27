package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

type Socket struct {
	fd        int
	netif     *net.Interface
	isValid   bool
	readChans []chan *Socket
	readable  bool
	mutex     sync.Mutex
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

	if err := startListenSock(fd, s); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}

	s.isValid = true

	// launch peeker
	go func() {
		buf := make([]byte, 2000)
		for {
			if !s.isValid {
				break
			}
			n, _, err := syscall.Recvfrom(s.fd, buf, syscall.MSG_PEEK)
			if err == nil {
				log.Printf("from peeker(fd=%d): %+v", s.fd, buf[:n])
			} else if err != syscall.EAGAIN {
				log.Printf("[WARN] peek message failed on fd=%d: %s", s.fd, err)
			}
			<-time.After(time.Second * 10)
		}
	}()

	return s, nil
}

func (s *Socket) ApplyBPF(is []bpf.RawInstruction) error {
	return applyBPF(s.fd, is)
}

func (s *Socket) readImmediate() ([]byte, error) {
	var buf [2048]byte
	n, err := syscall.Read(s.fd, buf[:])
	if err == syscall.EAGAIN {
		log.Printf("Got EAGAIN fd=%d", s.fd)
		s.unsetReadable()
		return nil, nil
	} else if err != nil {
		return nil, err
	} else {
		// peek message
		_, _, err := syscall.Recvfrom(s.fd, []byte{}, syscall.MSG_PEEK)
		if err == nil {
			// still have message
			s.notifyReadable()
		} else if err != syscall.EAGAIN {
			log.Printf("[WARN] peek message failed on fd=%d: %s", s.fd, err)
		}
		return buf[:n], nil
	}
}

func (s *Socket) ReadOnce(timeout *time.Duration) ([]byte, error) {
	data, err := s.readImmediate()
	if err != nil {
		return nil, err
	} else if data != nil {
		return data, err
	} else {
		ch := make(chan *Socket)
		s.addReadChan(ch)
		if timeout == nil {
			<-ch
		} else {
			select {
			case <-ch:
				if !s.isValid {
					return nil, fmt.Errorf("the socket has been closed")
				}
			case <-time.After(*timeout):
				return nil, fmt.Errorf("Socket.ReadOnce timed out after %s", *timeout)
			}
		}
		return s.readImmediate()
	}
}

func (s *Socket) LinkLocal() net.IP {
	ips, err := s.netif.Addrs()
	if err != nil {
		log.Printf("s.netif.Addrs() failed %s", err)
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
	log.Printf("%s has no link local address: %s", s.netif.Name, strings.Join(errs, ","))
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

func ReadMultiSocksOnce(socks []*Socket) (int, []byte, error) {
	// try immediate
	for i, s := range socks {
		data, err := s.readImmediate()
		if err != nil {
			return 0, nil, err
		} else if data != nil {
			return i, data, nil
		}
	}
	// async read
	ch := make(chan *Socket)
	for _, s := range socks {
		s.addReadChan(ch)
	}
	for {
		asyncs := <-ch
		// close all channels
		for _, s := range socks {
			s.removeReadChan(ch)
		}
		close(ch)
		if asyncs.isValid {
			data, err := asyncs.readImmediate()
			for i, s := range socks {
				if asyncs == s {
					return i, data, err
				}
			}
			return -1, data, err
		}
	}
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
	for _, ch := range s.readChans {
		select {
		case ch <- s:
		default:
		}
	}
	return syscall.Close(s.fd)
}

func (s *Socket) notifyReadable() {
	log.Printf("notifyReadable fd=%d", s.fd)
	s.mutex.Lock()
	defer s.mutex.Unlock()
retry:
	if len(s.readChans) > 0 {
		select {
		case s.readChans[0] <- s:
			log.Printf("sent to first listening sock fd=%d", s.fd)
		default:
			goto retry
		}
		s.readChans = s.readChans[1:]
	} else {
		log.Printf("queueing to boolean fd=%d", s.fd)
		s.readable = true
	}
}

func (s *Socket) unsetReadable() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.readable = false
}

func (s *Socket) addReadChan(ch chan *Socket) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.readable {
		s.readable = false
		select {
		case ch <- s:
		default:
		}
	} else {
		s.readChans = append(s.readChans, ch)
	}
}

func (s *Socket) removeReadChan(ch chan *Socket) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	var newChans []chan *Socket
	for _, c := range s.readChans {
		if c != ch {
			newChans = append(newChans, c)
		}
	}
	s.readChans = newChans
}

func (s *Socket) onClosed() {
	s.isValid = false
	stopListenSock(s.fd)
	s.notifyReadable()
}

func (s *Socket) onReadable() {
	s.notifyReadable()
}

func epollOnce(socks []*Socket) (*Socket, error) {
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

	nevents, err := syscall.EpollWait(epctx.epfd, events[:], -1)
	if err != nil {
		return nil, fmt.Errorf("EpollWait failed: %s", err)
	}

	var readSock *Socket
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
		}
		if (ev.Events&syscall.EPOLLIN) != 0 && readSock != nil {
			readSock = sock
		}
	}

	_ = syscall.Close(epfd)

	return readSock, nil
}

// epoll
type EpollContext struct {
	epfd  int
	mu    sync.RWMutex
	socks map[int]*Socket
}

type EpollReadCallback chan<- SocketReadResult

var epctx EpollContext

func initEpoll() error {
	epfd, err := syscall.EpollCreate1(0)
	if err != nil {
		return err
	}

	epctx.epfd = epfd
	epctx.socks = make(map[int]*Socket)

	return nil
}

func startListenSock(fd int, sock *Socket) error {
	epctx.mu.Lock()
	_, exists := epctx.socks[fd]
	epctx.mu.Unlock()
	if !exists {
		event := syscall.EpollEvent{
			Events: syscall.EPOLLIN | (uint32(1) << 31), // EPOLLET
			Fd:     int32(fd),
		}

		if err := syscall.EpollCtl(epctx.epfd, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
			return fmt.Errorf("EPOLL_CTL_ADD failed: %s", err)
		}
	}

	epctx.mu.Lock()
	epctx.socks[fd] = sock
	epctx.mu.Unlock()

	return nil
}

func stopListenSock(fd int) {
	epctx.mu.Lock()
	if epctx.socks[fd] == nil {
		epctx.mu.Unlock()
		return
	}
	delete(epctx.socks, fd)
	epctx.mu.Unlock()

	if err := syscall.EpollCtl(epctx.epfd, syscall.EPOLL_CTL_DEL, fd, nil); err != nil {
		log.Printf("[WARNING] EPOLL_CTL_DEL failed for fd %d: %s", fd, err)
	}
}

func runEpollLoop(ctx context.Context) error {
	var events [32]syscall.EpollEvent

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		nevents, err := syscall.EpollWait(epctx.epfd, events[:], -1)
		if err != nil {
			return err
		}

		for _, ev := range events[:nevents] {
			fd := int(ev.Fd)
			epctx.mu.Lock()
			sock := epctx.socks[fd]
			epctx.mu.Unlock()
			if sock == nil {
				continue
			}
			if (ev.Events&syscall.EPOLLERR) != 0 || (ev.Events&syscall.EPOLLHUP) != 0 {
				sock.onClosed()
			}
			if (ev.Events & syscall.EPOLLIN) != 0 {
				sock.onReadable()
			}
		}
	}
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
