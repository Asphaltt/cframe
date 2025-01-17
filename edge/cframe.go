package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/ICKelin/cframe/codec"
	"github.com/ICKelin/cframe/edge/vpc"
	log "github.com/ICKelin/cframe/pkg/logs"
)

type Server struct {
	registry *Registry

	// secret
	key string

	// server listen udp address
	laddr string

	// peers connection
	peerConns map[string]*peerConn

	// tun device wrap
	iface *Interface

	vpcInstance vpc.IVPC
}

type peerConn struct {
	addr *net.UDPAddr
	// conn *net.UDPConn
	// conn *kcp.UDPSession
	// conn net.Conn
	cidr *net.IPNet
}

func NewServer(laddr, key string, iface *Interface) *Server {
	return &Server{
		laddr:     laddr,
		key:       key,
		peerConns: make(map[string]*peerConn),
		iface:     iface,
	}
}

func (s *Server) SetRegistry(r *Registry) {
	s.registry = r
}

func (s *Server) SetVPCInstance(vpcInstance vpc.IVPC) {
	if s.vpcInstance == nil {
		s.vpcInstance = vpcInstance
	}
}

func (s *Server) ListenAndServe() error {
	laddr, err := net.ResolveUDPAddr("udp", s.laddr)
	if err != nil {
		return err
	}
	lconn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return err
	}
	defer lconn.Close()

	go s.readLocal(lconn)
	s.readRemote(lconn)
	return nil
}

func (s *Server) readRemote(lconn *net.UDPConn) {
	rawbytes := make([]byte, 1024*64)
	key := s.key
	klen := len(key)
	for {
		nr, _, err := lconn.ReadFromUDP(rawbytes)
		if err != nil {
			log.Error("read full fail: %v", err)
			continue
		}

		buf := rawbytes[:nr]

		if nr < klen {
			log.Error("pkt to small")
			continue
		}

		// decode key
		rkey := buf[:klen]
		if string(rkey) != key {
			log.Error("access forbidden!!")
			continue
		}

		pkt := buf[klen:nr]
		p := Packet(pkt)
		if p.Invalid() {
			log.Error("invalid ipv4 packet")
			continue
		}

		src := p.Src()
		dst := p.Dst()
		log.Debug("tuple %s => %s", src, dst)

		AddTrafficIn(int64(nr))
		s.iface.Write(pkt)
	}
}

func (s *Server) readLocal(sock *net.UDPConn) {
	for {
		pkt, err := s.iface.Read()
		if err != nil {
			log.Error("read iface error: %v", err)
			continue
		}

		p := Packet(pkt)
		if p.Invalid() {
			log.Error("invalid ipv4 packet")
			continue
		}

		AddTrafficOut(int64(len(pkt)))

		srcIP, dstIP := p.SrcIP(), p.DstIP()
		log.Debug("tuple %s => %s", srcIP, dstIP)

		peer, err := s.route(dstIP)
		if err != nil {
			log.Error("[E] not route to host: ", dstIP.String())
			continue
		}

		// encode key
		buf := make([]byte, 0, len(pkt)+len(s.key))
		buf = append(buf, []byte(s.key)...)
		buf = append(buf, pkt...)
		_, e := sock.WriteToUDP(buf, peer)
		if e != nil {
			log.Error("%v", e)
		}
	}
}

func (s *Server) route(dst net.IP) (*net.UDPAddr, error) {
	for _, p := range s.peerConns {
		if p.cidr.Contains(dst) {
			if p.addr.IP.Equal(dst) {
				continue
			}

			return p.addr, nil
		}
	}

	return nil, fmt.Errorf("no route")
}

func (s *Server) addRoute(peer *codec.Edge) error {
	log.Info("adding peer: %v", peer)

	ipmask := strings.Split(peer.Cidr, "/")
	cidrtype := "-net"
	if len(ipmask) == 1 || ipmask[1] == "32" {
		cidrtype = "-host"
	}

	// add vpc route
	if s.vpcInstance != nil {
		// add vpc route entry
		// route to current instance
		err := s.vpcInstance.CreateRoute(peer.Cidr)
		if err != nil {
			log.Error("create vpc route fail: %v", err)
			AddErrorLog(err)
			// Do not return
		}
	}

	// add local static route
	execCmd("route", []string{"del", cidrtype,
		peer.Cidr, "dev", s.iface.tun.Name()})

	out, err := execCmd("route", []string{"add", cidrtype,
		peer.Cidr, "dev", s.iface.tun.Name()})
	if err != nil {
		log.Error("route add %s %s dev %s, %s %v\n",
			peer.Cidr, cidrtype, s.iface.tun.Name(), out, err)
		AddErrorLog(err)
		return err
	}

	// add memory route
	if cidrtype == "-host" {
		peer.Cidr = fmt.Sprintf("%s/32", ipmask[0])
	}

	_, peerCidr, _ := net.ParseCIDR(peer.Cidr)
	peerAddr, _ := net.ResolveUDPAddr("udp", peer.ListenAddr)
	s.peerConns[peer.Cidr] = &peerConn{
		addr: peerAddr,
		cidr: peerCidr,
	}

	log.Info("added peer %v OK", peer)
	log.Info("==========================\n")
	return nil
}

func (s *Server) delRoute(peer *codec.Edge) {
	log.Info("del peer: %v", peer)
	ipmask := strings.Split(peer.Cidr, "/")
	cidrtype := "-net"
	if len(ipmask) == 1 || ipmask[1] == "32" {
		cidrtype = "-host"
	}
	out, err := execCmd("route", []string{"del", cidrtype,
		peer.Cidr, "dev", s.iface.tun.Name()})
	log.Info("route del %s %s dev %s, %s %v",
		cidrtype, peer.Cidr, s.iface.tun.Name(), out, err)

	if cidrtype == "-host" {
		peer.Cidr = fmt.Sprintf("%s/32", ipmask[0])
	}

	delete(s.peerConns, peer.Cidr)
	log.Info("del peer %s OK", peer)
	log.Info("==========================\n")
}

func (s *Server) AddPeers(peers []*codec.Edge) {
	for _, p := range peers {
		s.addRoute(p)
	}
}

func (s *Server) AddPeer(peer *codec.Edge) {
	s.addRoute(peer)
}

func (s *Server) DelPeer(peer *codec.Edge) {
	s.delRoute(peer)
}

func (s *Server) AddRoute(msg *codec.AddRouteMsg) {
	s.addRoute(&codec.Edge{
		Cidr:       msg.Cidr,
		ListenAddr: msg.Nexthop,
	})
}

func (s *Server) DelRoute(msg *codec.DelRouteMsg) {
	s.delRoute(&codec.Edge{
		Cidr:       msg.Cidr,
		ListenAddr: msg.Nexthop,
	})
}
