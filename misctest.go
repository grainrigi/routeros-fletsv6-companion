package main

import (
	"context"
	"fmt"
	"log"
	"net"
)

func dumpByteSlice(b []byte) {
	var a [16]byte
	n := (len(b) + 15) &^ 15
	for i := 0; i < n; i++ {
		/*
			if i%16 == 0 {
				fmt.Printf("%4d", i)
			}
			if i%8 == 0 {
				fmt.Print(" ")
			}
		*/
		if i < len(b) {
			fmt.Printf(" %02X", b[i])
		} else {
			fmt.Print("   ")
		}
		/*
			if i >= len(b) {
				a[i%16] = ' '
			} else if b[i] < 32 || b[i] > 126 {
				a[i%16] = '.'
			} else {
				a[i%16] = b[i]
			}
		*/
		if i%16 == 15 {
			fmt.Printf("  %s\n", string(a[:]))
		}
	}
}

func miscTest() {
	if err := initEpoll(); err != nil {
		log.Fatalf("initEpoll failed: %s", err)
	}
	go runEpollLoop(context.Background())

	i1, _ := NewDecodedInterface("enp4s0")
	i2, _ := NewDecodedInterface("enp4s0@101")
	ii1, err := i1.Index()
	if err != nil {
		log.Fatalf("i1.Index failed: %s", err)
	}
	ii2, err := i2.Index()
	if err != nil {
		log.Fatalf("i2.Index failed: %s", err)
	}
	fmt.Printf("%s %s %d %d\n", i1.ActualName(), i2.ActualName(), ii1, ii2)
	/*
		s1, err := NewSocket(ii1)
		if err != nil {
			log.Fatalf("NewSocket(ii1) failed: %s", err)
		}
		if err := s1.ApplyBPF(bpfMulticast()); err != nil {
			log.Fatalf("ApplyBPF failed: %s", err)
		}
		if err := s1.ClearBuf(); err != nil {
			log.Fatalf("s1.ClearBuf failed: %s", err)
		}
		ch := make(chan SocketReadResult)
		startListenSock(s1.fd, ch)
		bbuf := make([]byte, 2000)
		for {
			n, err := syscall.Read(s1.fd, bbuf)
			if err == syscall.EAGAIN {
				<-time.After(time.Millisecond * 10)
			} else if err != nil {
				log.Printf("syscall.Read error: %s", err)
			} else {
				log.Printf("syscall.Read %+v", bbuf[:n])
			}
		}
	*/

	s1, err := NewSocket(ii1)
	if err != nil {
		log.Fatalf("NewSocket(ii1) failed: %s", err)
	}
	if err := s1.ApplyBPF(bpfICMPv6(134)); err != nil {
		log.Fatal(err)
	}
	s2, err := NewSocket(ii2)
	if err != nil {
		log.Fatalf("NewSocket(ii2) failed: %s", err)
	}
	if err := s2.ApplyBPF(bpfICMPv6(135)); err != nil {
		log.Fatal(err)
	}
	/*
		startListenSock(s1.fd, make(EpollReadCallback))
		startListenSock(s2.fd, make(EpollReadCallback))
	*/
	/*
		go func() {
			bbuf := make([]byte, 2000)
			for {
				n, err := syscall.Read(s1.fd, bbuf)
				if err == syscall.EAGAIN {
					<-time.After(time.Millisecond * 10)
				} else if err != nil {
					log.Printf("s1 syscall.Read error: %s", err)
				} else {
					log.Printf("s1 syscall.Read %+v", bbuf[:n])
				}
			}
		}()
		func() {
			bbuf := make([]byte, 2000)
			for {
				n, err := syscall.Read(s2.fd, bbuf)
				if err == syscall.EAGAIN {
					<-time.After(time.Millisecond * 10)
				} else if err != nil {
					log.Printf("s2 syscall.Read error: %s", err)
				} else {
					log.Printf("s2 syscall.Read %+v", bbuf[:n])
				}
			}
		}()
	*/
	s1c := s1.ReadOnceChan(nil)
	s2c := s2.ReadOnceChan(nil)
	rs := makeRouterSolicitation(s1.LinkLocal(), s1.netif.HardwareAddr)
	dumpByteSlice(rs)
	if err := s1.WriteOnce(rs); err != nil {
		log.Fatalf("s1.WriteOnce failed: %s", err)
	}
L:
	for {
		select {
		case r, ok := <-s1c:
			if !ok {
				break L
			}
			if r.err != nil {
				log.Printf("error from s1: %s", r.err)
			} else {
				log.Printf("from s1")
				dumpByteSlice(r.data)
				s1c = s1.ReadOnceChan(nil)
			}
		case r, ok := <-s2c:
			if !ok {
				break L
			}
			if r.err != nil {
				log.Printf("error from s2: %s", r.err)
			} else {
				log.Printf("from s2")
				dumpByteSlice(r.data)
				s2c = s2.ReadOnceChan(nil)
			}
		}
	}
}

func rosTest() {
	rosCfg, err := loadROSConfig()
	if err != nil {
		log.Fatalf("loadROSConfig failed: %s", rosCfg)
	}
	log.Printf("%+v", rosCfg)
	c, err := NewROSClient(rosCfg)
	if err != nil {
		log.Fatalf("NewROSClient failed: %s", err)
	}
	ip := net.ParseIP("2001:db8::1")
	cidr := net.IPNet{
		IP:   ip,
		Mask: net.CIDRMask(64, 128),
	}
	err = c.AssignIPv6("loopback", &cidr, "ra-prefix::1/64", ROSIPOptions{Advertise: true, Eui64: true})
	if err != nil {
		log.Fatalf("AssignIPv6 failed: %s", err)
	}
}
