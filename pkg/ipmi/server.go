package ipmi

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	log "github.com/sirupsen/logrus"
)

// maxConcurrentHandlers bounds the number of in-flight packet handlers. Each
// inbound datagram is handled in its own goroutine; without a cap a UDP flood
// would spawn unbounded goroutines and allocations. When the cap is reached the
// listener stops reading (back-pressure), so excess datagrams are dropped by the
// kernel socket buffer rather than buffered in the process.
const maxConcurrentHandlers = 64

// Server handles IPMI over LAN on a UDP socket.
type Server struct {
	conn     *net.UDPConn
	sessions *sessionManager
	stop     chan struct{}
	wg       sync.WaitGroup
	sem      chan struct{}
}

// Start creates and starts the IPMI UDP server on the given port. powerCtrl
// and fwCtrl are injected into the session manager for the chassis/OEM
// command handlers instead of being reached via package-level singletons.
func Start(port int, powerCtrl *power.Controller, fwCtrl *firmware.Controller) (*Server, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("resolve udp addr: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}

	s := &Server{
		conn:     conn,
		sessions: newSessionManager(powerCtrl, fwCtrl),
		stop:     make(chan struct{}),
		sem:      make(chan struct{}, maxConcurrentHandlers),
	}

	s.wg.Add(2)
	go s.listen()
	go s.reapLoop()

	log.Infof("IPMI server started on port %d", port)
	return s, nil
}

// Stop gracefully shuts down the IPMI server, waiting for the listener, the
// reaper, and any in-flight packet handlers to finish before returning.
func (s *Server) Stop() {
	close(s.stop)
	_ = s.conn.Close()
	s.wg.Wait()
	s.sessions.closeAll()
	log.Info("IPMI server stopped")
}

// reapLoop periodically drops idle sessions until the server stops.
func (s *Server) reapLoop() {
	defer s.wg.Done()

	t := time.NewTicker(sessionReapInterval)
	defer t.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sessions.reap(sessionIdleTimeout)
		}
	}
}

func (s *Server) listen() {
	defer s.wg.Done()

	buf := make([]byte, 4096)
	for {
		select {
		case <-s.stop:
			return
		default:
		}

		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
				log.Errorf("IPMI read error: %s", err)
				continue
			}
		}

		if n < 4 {
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		telemetry.IPMIPacketReceived(context.Background())

		// Acquire a handler slot, or bail out if the server is stopping. When all
		// slots are busy this blocks, pausing reads so a flood is dropped by the
		// kernel socket buffer instead of spawning unbounded goroutines.
		select {
		case s.sem <- struct{}{}:
		case <-s.stop:
			return
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.handlePacket(pkt, addr)
		}()
	}
}

func (s *Server) handlePacket(data []byte, addr *net.UDPAddr) {
	if data[0] != rmcpVersion {
		log.Debugf("IPMI: invalid RMCP version: 0x%02x", data[0])
		return
	}

	class := data[3]
	body := data[4:]

	switch class {
	case rmcpClassASF:
		s.handleASF(data, addr)
	case rmcpClassIPMI:
		s.handleIPMI(body, addr)
	default:
		log.Debugf("IPMI: unknown RMCP class: 0x%02x", class)
	}
}

// handleASF responds to ASF Presence Ping with a Pong.
func (s *Server) handleASF(data []byte, addr *net.UDPAddr) {
	if len(data) < 12 {
		return
	}

	asfBody := data[4:]
	msgType := asfBody[4]

	if msgType != asfMessagePing {
		return
	}

	msgTag := asfBody[5]

	resp := make([]byte, 28)
	// RMCP header
	resp[0] = rmcpVersion
	resp[1] = rmcpReserved
	resp[2] = rmcpSeqNone
	resp[3] = rmcpClassASF
	// ASF header
	binary.BigEndian.PutUint32(resp[4:8], asfIANA)
	resp[8] = asfMessagePong
	resp[9] = msgTag
	resp[10] = 0x00 // reserved
	resp[11] = 0x10 // data length = 16
	// Pong payload
	binary.BigEndian.PutUint32(resp[12:16], asfIANA)
	// OEM-defined (4 bytes zero at resp[16:20])
	resp[20] = 0x81 // IPMI supported + ASF v1.0
	resp[21] = 0x80 // security extensions (RMCP+) supported
	// reserved 6 bytes zero at resp[22:28]

	s.send(resp, addr)
}

// handleIPMI dispatches IPMI v1.5 and v2.0/RMCP+ messages.
func (s *Server) handleIPMI(body []byte, addr *net.UDPAddr) {
	if len(body) < 1 {
		return
	}

	authType := body[0]
	switch authType {
	case authTypeNone:
		resp := s.sessions.handleIPMI15(body, s)
		if resp != nil {
			s.send(resp, addr)
		}
	case authTypeRMCPP:
		resp := s.sessions.handleIPMI20(body, addr, s)
		if resp != nil {
			s.send(resp, addr)
		}
	default:
		log.Debugf("IPMI: unsupported auth type: 0x%02x", authType)
	}
}

func (s *Server) send(data []byte, addr *net.UDPAddr) {
	if _, err := s.conn.WriteToUDP(data, addr); err != nil {
		log.Errorf("IPMI send error: %s", err)
		return
	}
	telemetry.IPMIPacketSent(context.Background())
}

// wrapRMCP prepends an RMCP header to a payload.
func wrapRMCP(class byte, payload []byte) []byte {
	pkt := make([]byte, 4+len(payload))
	pkt[0] = rmcpVersion
	pkt[1] = rmcpReserved
	pkt[2] = rmcpSeqNone
	pkt[3] = class
	copy(pkt[4:], payload)
	return pkt
}

// wrapRMCPPlus builds an RMCP+ session wrapper (no integrity).
func wrapRMCPPlus(payloadType byte, sessionID uint32, seqNum uint32, payload []byte) []byte {
	hdr := make([]byte, 12+len(payload))
	hdr[0] = authTypeRMCPP
	hdr[1] = payloadType
	binary.LittleEndian.PutUint32(hdr[2:6], sessionID)
	binary.LittleEndian.PutUint32(hdr[6:10], seqNum)
	binary.LittleEndian.PutUint16(hdr[10:12], uint16(len(payload)))
	copy(hdr[12:], payload)
	return hdr
}

// wrapRMCPPlusAuth builds an RMCP+ session wrapper with HMAC-SHA1-96 integrity.
func wrapRMCPPlusAuth(payloadType byte, sessionID uint32, seqNum uint32, payload []byte, k1 []byte) []byte {
	payloadWithFlag := payloadType | 0x40 // authenticated bit

	// Pad so that (payloadLen + padLen + 2) is a multiple of 4.
	padLen := (4 - (len(payload)+2)%4) % 4

	headerLen := 12
	trailerLen := padLen + 1 + 1 + 12 // pad + padLen + nextHdr + authCode
	total := headerLen + len(payload) + trailerLen

	pkt := make([]byte, total)
	pkt[0] = authTypeRMCPP
	pkt[1] = payloadWithFlag
	binary.LittleEndian.PutUint32(pkt[2:6], sessionID)
	binary.LittleEndian.PutUint32(pkt[6:10], seqNum)
	binary.LittleEndian.PutUint16(pkt[10:12], uint16(len(payload)))
	copy(pkt[12:12+len(payload)], payload)

	off := 12 + len(payload)
	for i := 0; i < padLen; i++ {
		pkt[off+i] = 0xFF
	}
	pkt[off+padLen] = byte(padLen)
	pkt[off+padLen+1] = 0x07 // next header

	// HMAC-SHA1-96 over AuthType through NextHeader
	hmacData := pkt[:off+padLen+2]
	authCode := computeHMACSHA196(k1, hmacData)
	copy(pkt[off+padLen+2:], authCode)

	return pkt
}

// buildIPMI15Wrapper wraps an IPMI message in a v1.5 unauthenticated session header.
func buildIPMI15Wrapper(ipmiMsg []byte) []byte {
	// AuthType(1) + SeqNum(4) + SessionID(4) + MsgLen(1) + ipmiMsg
	wrapper := make([]byte, 10+len(ipmiMsg))
	wrapper[0] = authTypeNone
	// SeqNum and SessionID are 0 for pre-session
	wrapper[9] = byte(len(ipmiMsg))
	copy(wrapper[10:], ipmiMsg)
	return wrapper
}

// buildIPMIMsg builds an IPMI response message with proper checksums.
// bmc_addr is the BMC's address (from request's RsAddr, becomes response RsAddr).
// rq_origin_addr is the requester's address (from request's RqAddr, becomes response RqAddr).
func buildIPMIMsg(bmc_addr byte, rq_origin_addr byte, respNetFnLUN byte, rqSeq byte, cmd byte, data []byte) []byte {
	totalLen := 7 + len(data)
	msg := make([]byte, totalLen)

	msg[0] = bmc_addr
	msg[1] = respNetFnLUN
	msg[2] = ipmiChecksum(msg[0:2])
	msg[3] = rq_origin_addr
	msg[4] = (rqSeq << 2) | (respNetFnLUN & 0x03)
	msg[5] = cmd
	copy(msg[6:6+len(data)], data)
	msg[totalLen-1] = ipmiChecksum(msg[3 : totalLen-1])

	return msg
}
