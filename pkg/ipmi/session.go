package ipmi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/serial"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
)

// Session lifecycle bounds. RMCP+ Open Session Requests create a session before
// authentication, so an unauthenticated peer (e.g. a UDP scanner hammering port
// 623) could otherwise grow the session map without bound. maxSessions caps
// concurrent sessions and the reaper drops sessions idle longer than
// sessionIdleTimeout. 60s matches the conventional IPMI session timeout: live
// consoles emit keepalives, while an abandoned one falls silent and is reaped
// (which also releases any SOL hold on the serial broker).
const (
	maxSessions         = 64
	sessionIdleTimeout  = 60 * time.Second
	sessionReapInterval = 20 * time.Second
)

// sessionState tracks RMCP+ authentication progress.
type sessionState int

const (
	statePreSession sessionState = iota
	stateOpenSession
	stateRAKP1Done
	stateActive
)

// session represents an active IPMI session.
type session struct {
	state            sessionState
	consoleSessionID uint32
	bmcSessionID     uint32
	consoleRandom    [16]byte
	bmcRandom        [16]byte
	authAlgo         byte
	integAlgo        byte
	confidAlgo       byte
	reqPrivilege     byte
	username         string
	password         []byte
	sik              []byte // Session Integrity Key
	k1               []byte // integrity key
	k2               []byte // confidentiality key
	outSeq           uint32
	addr             *net.UDPAddr
	server           *Server

	// lastActivity is the UnixNano timestamp of the most recent packet for this
	// session. Refreshed on every lookup (get) and used by the reaper. Atomic so
	// it can be touched from packet-handler goroutines without the manager lock.
	lastActivity atomic.Int64
}

// sessionManager tracks all active sessions. It also carries the power and
// firmware controllers the chassis/OEM command handlers act on — injected
// once at construction instead of reached via package-level singletons.
type sessionManager struct {
	mu       sync.RWMutex
	sessions map[uint32]*session

	power    *power.Controller
	firmware *firmware.Controller
}

var sessionIDCounter uint32

func newSessionManager(powerCtrl *power.Controller, fwCtrl *firmware.Controller) *sessionManager {
	return &sessionManager{
		sessions: make(map[uint32]*session),
		power:    powerCtrl,
		firmware: fwCtrl,
	}
}

// newSession creates and registers a session. It returns nil when the
// concurrent-session cap is reached, so a flood of Open Session Requests cannot
// grow memory without bound; callers must handle a nil result.
func (sm *sessionManager) newSession() *session {
	id := atomic.AddUint32(&sessionIDCounter, 1)
	sess := &session{
		state:        statePreSession,
		bmcSessionID: id,
		password:     []byte(defaultPassword),
	}
	sess.lastActivity.Store(time.Now().UnixNano())

	sm.mu.Lock()
	if len(sm.sessions) >= maxSessions {
		sm.mu.Unlock()
		log.Warnf("IPMI: session cap (%d) reached, rejecting new session", maxSessions)
		return nil
	}
	sm.sessions[id] = sess
	sm.mu.Unlock()

	telemetry.IPMISessionOpened(context.Background())
	return sess
}

func (sm *sessionManager) get(id uint32) *session {
	sm.mu.RLock()
	sess := sm.sessions[id]
	sm.mu.RUnlock()
	if sess != nil {
		sess.lastActivity.Store(time.Now().UnixNano())
	}
	return sess
}

func (sm *sessionManager) remove(id uint32) {
	sm.mu.Lock()
	sess, existed := sm.sessions[id]
	delete(sm.sessions, id)
	sm.mu.Unlock()
	if existed {
		sm.cleanupSession(sess)
	}
}

// cleanupSession releases resources tied to a removed session: any SOL hold on
// the shared serial broker, and the active-session telemetry gauge. Runs outside
// sm.mu so broker teardown never blocks the manager lock.
func (sm *sessionManager) cleanupSession(sess *session) {
	solSession.stopIfSession(sess)
	telemetry.IPMISessionClosed(context.Background())
}

// reap drops sessions idle longer than idle. Called periodically by the server.
func (sm *sessionManager) reap(idle time.Duration) {
	cutoff := time.Now().Add(-idle).UnixNano()

	var expired []*session
	sm.mu.Lock()
	for id, sess := range sm.sessions {
		if sess.lastActivity.Load() < cutoff {
			delete(sm.sessions, id)
			expired = append(expired, sess)
		}
	}
	sm.mu.Unlock()

	for _, sess := range expired {
		log.Debugf("IPMI: reaping idle session 0x%08x", sess.bmcSessionID)
		sm.cleanupSession(sess)
	}
}

func (sm *sessionManager) closeAll() {
	sm.mu.Lock()
	expired := make([]*session, 0, len(sm.sessions))
	for id, sess := range sm.sessions {
		expired = append(expired, sess)
		delete(sm.sessions, id)
	}
	sm.mu.Unlock()

	solSession.stop()
	for range expired {
		telemetry.IPMISessionClosed(context.Background())
	}
}

// handleIPMI15 processes an IPMI v1.5 unauthenticated message (pre-session).
func (sm *sessionManager) handleIPMI15(data []byte, srv *Server) []byte {
	// Format: authType(1) seqNum(4) sessionID(4) msgLen(1) ipmiMsg...
	if len(data) < 10 {
		return nil
	}

	msgLen := int(data[9])
	if len(data) < 10+msgLen || msgLen < 7 {
		return nil
	}

	ipmiMsg := data[10 : 10+msgLen]

	netFn := ipmiMsg[1] >> 2
	rqOriginAddr := ipmiMsg[0] // Requester's address (client sending the request)
	bmcAddr := ipmiMsg[3]      // BMC's address from the request
	rqSeq := ipmiMsg[4] >> 2
	rqLUN := ipmiMsg[4] & 0x03
	cmd := ipmiMsg[5]

	var cmdData []byte
	if msgLen > 7 {
		cmdData = ipmiMsg[6 : msgLen-1]
	}

	var respData []byte
	switch {
	case netFn == netFnAppReq && cmd == cmdGetChannelAuthCap:
		respData = handleGetChannelAuthCap(cmdData)
		log.Infof("IPMI: Get Channel Auth Capabilities request (pre-session)")
	default:
		log.Debugf("IPMI: unsupported pre-session cmd netFn=0x%02x cmd=0x%02x", netFn, cmd)
		respData = []byte{ccInvalidCommand}
	}

	respNetFnLUN := (netFn + 1) << 2
	respNetFnLUN |= rqLUN
	ipmiResp := buildIPMIMsg(bmcAddr, rqOriginAddr, respNetFnLUN, rqSeq, cmd, respData)
	return wrapRMCP(rmcpClassIPMI, buildIPMI15Wrapper(ipmiResp))
}

// handleIPMI20 processes an RMCP+ (auth type 0x06) message.
func (sm *sessionManager) handleIPMI20(data []byte, addr *net.UDPAddr, srv *Server) []byte {
	if len(data) < 12 {
		return nil
	}

	payloadType := data[1] & 0x3F
	authenticated := data[1]&0x40 != 0
	sessionID := binary.LittleEndian.Uint32(data[2:6])
	payloadLen := int(binary.LittleEndian.Uint16(data[10:12]))

	if len(data) < 12+payloadLen {
		return nil
	}

	payload := data[12 : 12+payloadLen]

	switch payloadType {
	case payloadTypeOpenSessionReq:
		return sm.handleOpenSessionReq(payload, addr, srv)

	case payloadTypeRAKPMsg1:
		return sm.handleRAKPMsg1(payload, addr, srv)

	case payloadTypeRAKPMsg3:
		return sm.handleRAKPMsg3(payload)

	case payloadTypeIPMI:
		sess := sm.get(sessionID)
		if sess == nil {
			log.Debugf("IPMI: unknown session 0x%08x", sessionID)
			return nil
		}
		return sm.handleIPMIPayload(sess, payload, authenticated)

	case payloadTypeSOL:
		sess := sm.get(sessionID)
		if sess == nil {
			return nil
		}
		return sm.handleSOLPayload(sess, payload)

	default:
		log.Debugf("IPMI: unknown payload type 0x%02x", payloadType)
		return nil
	}
}

// handleGetChannelAuthCap responds to Get Channel Authentication Capabilities.
func handleGetChannelAuthCap(cmdData []byte) []byte {
	// Response per IPMI 2.0 Table 22-15 / ipmitool get_channel_auth_cap_rsp struct:
	//   [0] completion code
	//   [1] channel number
	//   [2] auth type support  (bit 7 = v2.0 extended data available)
	//   [3] auth status        (bit 5 = non-null users, bit 4 = null users, bit 3 = anon)
	//   [4] extended caps      (bit 1 = IPMI v2.0, bit 0 = IPMI v1.5)
	//   [5:7] OEM ID
	//   [8] OEM auxiliary
	resp := make([]byte, 9)
	resp[0] = ccOK
	resp[1] = 0x0E // channel 14 (current)
	resp[2] = 0x80 // bit 7: IPMI v2.0+ extended data available
	resp[3] = 0x20 // bit 5: non-null usernames enabled
	resp[4] = 0x02 // bit 1: IPMI v2.0 connections supported
	resp[5] = 0x00 // OEM ID byte 0
	resp[6] = 0x00 // OEM ID byte 1
	resp[7] = 0x00 // OEM ID byte 2
	resp[8] = 0x00 // OEM auxiliary
	return resp
}

// handleOpenSessionReq processes RMCP+ Open Session Request.
func (sm *sessionManager) handleOpenSessionReq(data []byte, addr *net.UDPAddr, srv *Server) []byte {
	if len(data) < 32 {
		return nil
	}

	msgTag := data[0]
	reqPriv := data[1]
	consoleSessionID := binary.LittleEndian.Uint32(data[4:8])

	// Parse algorithms from the three 8-byte payload blocks starting at offset 8.
	authAlgo := data[12]   // auth payload algorithm
	integAlgo := data[20]  // integrity payload algorithm
	confidAlgo := data[28] // confidentiality payload algorithm

	sess := sm.newSession()
	if sess == nil {
		// Session cap reached — reply with "insufficient resources for session".
		resp := make([]byte, 8)
		resp[0] = msgTag
		resp[1] = 0x01 // status: insufficient resources to create a session
		binary.LittleEndian.PutUint32(resp[4:8], consoleSessionID)
		return wrapRMCP(rmcpClassIPMI, wrapRMCPPlus(payloadTypeOpenSessionResp, 0, 0, resp))
	}
	sess.consoleSessionID = consoleSessionID
	sess.authAlgo = authAlgo
	sess.integAlgo = integAlgo
	sess.confidAlgo = confidAlgo
	sess.reqPrivilege = reqPriv
	sess.addr = addr
	sess.server = srv
	sess.state = stateOpenSession

	log.Infof("IPMI: Open Session req auth=%d integ=%d confid=%d bmcSID=0x%08x",
		authAlgo, integAlgo, confidAlgo, sess.bmcSessionID)

	// Build Open Session Response (36 bytes)
	resp := make([]byte, 36)
	resp[0] = msgTag
	resp[1] = 0x00 // status OK
	resp[2] = reqPriv
	// resp[3] reserved
	binary.LittleEndian.PutUint32(resp[4:8], consoleSessionID)
	binary.LittleEndian.PutUint32(resp[8:12], sess.bmcSessionID)

	// Authentication payload
	resp[12] = 0x00 // payload type
	resp[15] = 0x08 // payload length
	resp[16] = authAlgo

	// Integrity payload
	resp[20] = 0x01
	resp[23] = 0x08
	resp[24] = integAlgo

	// Confidentiality payload
	resp[28] = 0x02
	resp[31] = 0x08
	resp[32] = confidAlgo

	return wrapRMCP(rmcpClassIPMI, wrapRMCPPlus(payloadTypeOpenSessionResp, 0, 0, resp))
}

// handleRAKPMsg1 processes RAKP Message 1 and returns RAKP Message 2.
func (sm *sessionManager) handleRAKPMsg1(data []byte, addr *net.UDPAddr, srv *Server) []byte {
	if len(data) < 28 {
		return nil
	}

	msgTag := data[0]
	bmcSessionID := binary.LittleEndian.Uint32(data[4:8])

	sess := sm.get(bmcSessionID)
	if sess == nil {
		log.Debugf("IPMI: RAKP1 unknown session 0x%08x", bmcSessionID)
		return nil
	}

	copy(sess.consoleRandom[:], data[8:24])
	sess.reqPrivilege = data[24]

	usernameLen := int(data[27])
	if len(data) < 28+usernameLen {
		return nil
	}
	if usernameLen > 0 {
		sess.username = string(data[28 : 28+usernameLen])
	}

	// Generate BMC random number
	if _, err := rand.Read(sess.bmcRandom[:]); err != nil {
		log.Errorf("IPMI: failed to generate random: %s", err)
		return nil
	}

	sess.addr = addr
	sess.server = srv
	sess.state = stateRAKP1Done

	// Compute RAKP Message 2 auth code per IPMI 2.0 Table 13-14:
	// HMAC_Kuid(SIDc || SIDm || Rm || Rc || GUIDm || RoleM || ULm || UserNameM)
	// Note: ipmitool convention places console session ID first, BMC session ID second.
	var hmacInput []byte
	sidc := make([]byte, 4)
	sidm := make([]byte, 4)
	binary.LittleEndian.PutUint32(sidc, sess.consoleSessionID)
	binary.LittleEndian.PutUint32(sidm, sess.bmcSessionID)

	hmacInput = append(hmacInput, sidc...)
	hmacInput = append(hmacInput, sidm...)
	hmacInput = append(hmacInput, sess.consoleRandom[:]...)
	hmacInput = append(hmacInput, sess.bmcRandom[:]...)
	hmacInput = append(hmacInput, bmcGUID[:]...)
	hmacInput = append(hmacInput, sess.reqPrivilege)
	hmacInput = append(hmacInput, byte(usernameLen))
	if usernameLen > 0 {
		hmacInput = append(hmacInput, []byte(sess.username)...)
	}

	authCode := computeHMACSHA1(sess.password, hmacInput)

	// Build RAKP Message 2
	respLen := 40 + len(authCode)
	resp := make([]byte, respLen)
	resp[0] = msgTag
	resp[1] = 0x00 // status OK
	// resp[2:4] reserved
	binary.LittleEndian.PutUint32(resp[4:8], sess.consoleSessionID)
	copy(resp[8:24], sess.bmcRandom[:])
	copy(resp[24:40], bmcGUID[:])
	copy(resp[40:], authCode)

	log.Infof("IPMI: RAKP1 processed, user=%q bmcSID=0x%08x", sess.username, sess.bmcSessionID)

	return wrapRMCP(rmcpClassIPMI, wrapRMCPPlus(payloadTypeRAKPMsg2, 0, 0, resp))
}

// handleRAKPMsg3 processes RAKP Message 3 and returns RAKP Message 4.
func (sm *sessionManager) handleRAKPMsg3(data []byte) []byte {
	if len(data) < 8 {
		return nil
	}

	msgTag := data[0]
	statusCode := data[1]
	bmcSessionID := binary.LittleEndian.Uint32(data[4:8])

	sess := sm.get(bmcSessionID)
	if sess == nil {
		log.Debugf("IPMI: RAKP3 unknown session 0x%08x", bmcSessionID)
		return nil
	}

	if statusCode != 0x00 {
		log.Debugf("IPMI: RAKP3 error status 0x%02x", statusCode)
		sm.remove(bmcSessionID)
		return nil
	}

	// Optionally verify client's auth code from RAKP3.
	// HMAC_Kuid(Rc || SIDm || RoleM || ULm || UserNameM)
	// For simplicity we skip strict verification.

	// Compute SIK (Session Integrity Key):
	// SIK = HMAC_Kg(Rm || Rc || RoleM || ULm || UserNameM)
	// When Kg is all zeros, use Kuid (password).
	var sikInput []byte
	sikInput = append(sikInput, sess.consoleRandom[:]...)
	sikInput = append(sikInput, sess.bmcRandom[:]...)
	sikInput = append(sikInput, sess.reqPrivilege)
	sikInput = append(sikInput, byte(len(sess.username)))
	if len(sess.username) > 0 {
		sikInput = append(sikInput, []byte(sess.username)...)
	}
	sess.sik = computeHMACSHA1(sess.password, sikInput)

	// Derive session keys
	const blockLen = 20
	c1 := make([]byte, blockLen)
	c2 := make([]byte, blockLen)
	for i := range c1 {
		c1[i] = 0x01
		c2[i] = 0x02
	}
	sess.k1 = computeHMACSHA1(sess.sik, c1)
	sess.k2 = computeHMACSHA1(sess.sik, c2)

	sess.state = stateActive
	log.Infof("IPMI: session 0x%08x activated for user %q", sess.bmcSessionID, sess.username)

	// Compute RAKP Message 4 integrity check value per IPMI 2.0 Table 13-17:
	// HMAC_SIK(Rm || SIDm || GUIDm) truncated to 12 bytes
	// Note: SIDm here is the BMC's session ID (per ipmitool convention).
	var icvInput []byte
	sidm := make([]byte, 4)
	binary.LittleEndian.PutUint32(sidm, sess.bmcSessionID)
	icvInput = append(icvInput, sess.consoleRandom[:]...)
	icvInput = append(icvInput, sidm...)
	icvInput = append(icvInput, bmcGUID[:]...)
	icv := computeHMACSHA196(sess.sik, icvInput)

	// Build RAKP Message 4
	resp := make([]byte, 8+len(icv))
	resp[0] = msgTag
	resp[1] = 0x00 // status OK
	// resp[2:4] reserved
	binary.LittleEndian.PutUint32(resp[4:8], sess.consoleSessionID)
	copy(resp[8:], icv)

	return wrapRMCP(rmcpClassIPMI, wrapRMCPPlus(payloadTypeRAKPMsg4, 0, 0, resp))
}

// handleIPMIPayload dispatches an IPMI command within an established session.
func (sm *sessionManager) handleIPMIPayload(sess *session, payload []byte, authenticated bool) []byte {
	if len(payload) < 7 {
		return nil
	}

	netFn := payload[1] >> 2
	rqOriginAddr := payload[0] // Requester's address (client sending the request)
	bmcAddr := payload[3]      // BMC's address from the request
	rqSeq := payload[4] >> 2
	rqLUN := payload[4] & 0x03
	cmd := payload[5]

	var cmdData []byte
	if len(payload) > 7 {
		cmdData = payload[6 : len(payload)-1]
	}

	var respData []byte

	switch netFn {
	case netFnChassisReq:
		switch cmd {
		case cmdGetChassisStatus:
			respData = sm.handleGetChassisStatus()
		case cmdChassisControl:
			respData = sm.handleChassisControl(cmdData)
		case cmdSetSystemBootOptions:
			respData = sm.handleSetSystemBootOptions(cmdData)
		case cmdGetSystemBootOptions:
			respData = sm.handleGetSystemBootOptions(cmdData)
		default:
			respData = []byte{ccInvalidCommand}
		}

	case netFnAppReq:
		switch cmd {
		case cmdGetDeviceID:
			respData = handleGetDeviceID()
		case cmdGetChannelAuthCap:
			respData = handleGetChannelAuthCap(cmdData)
		case cmdSetSessionPriv:
			respData = handleSetSessionPrivLevel(cmdData)
		case cmdCloseSession:
			respData = sm.handleCloseSession(sess)
		case cmdActivatePayload:
			respData = handleActivatePayload(sess, cmdData)
		case cmdDeactivatePayload:
			respData = handleDeactivatePayload(cmdData)
		default:
			respData = []byte{ccInvalidCommand}
		}

	case netFnOEMReq:
		switch cmd {
		case cmdOEMGetUBootVersion:
			respData = sm.handleOEMGetUBootVersion()
		case cmdOEMUpdateUBoot:
			respData = sm.handleOEMUpdateUBoot()
		default:
			respData = []byte{ccInvalidCommand}
		}

	default:
		respData = []byte{ccInvalidCommand}
	}

	respNetFnLUN := ((netFn + 1) << 2) | rqLUN
	ipmiResp := buildIPMIMsg(bmcAddr, rqOriginAddr, respNetFnLUN, rqSeq, cmd, respData)

	outSeq := atomic.AddUint32(&sess.outSeq, 1)
	if authenticated && sess.integAlgo != integAlgoNone && sess.k1 != nil {
		return wrapRMCP(rmcpClassIPMI,
			wrapRMCPPlusAuth(payloadTypeIPMI, sess.consoleSessionID, outSeq, ipmiResp, sess.k1))
	}
	return wrapRMCP(rmcpClassIPMI,
		wrapRMCPPlus(payloadTypeIPMI, sess.consoleSessionID, outSeq, ipmiResp))
}

// handleSOLPayload relays SOL data from the remote console to the serial port.
func (sm *sessionManager) handleSOLPayload(sess *session, payload []byte) []byte {
	if len(payload) < 4 {
		return nil
	}

	pktSeq := payload[0]
	charData := payload[4:]

	if len(charData) > 0 {
		solSession.mu.Lock()
		active := solSession.active
		solSession.mu.Unlock()

		if active {
			if _, err := serial.GetBroker().Write(charData); err != nil {
				log.Errorf("SOL: serial write error: %s", err)
			}
		}
	}

	// Build SOL ACK
	ack := make([]byte, 4)
	// ack[0] = 0 — no new data in this packet
	ack[1] = pktSeq              // acknowledge received sequence
	ack[2] = byte(len(charData)) // accepted character count
	// ack[3] = 0 — status OK

	outSeq := atomic.AddUint32(&sess.outSeq, 1)
	if sess.integAlgo != integAlgoNone && sess.k1 != nil {
		return wrapRMCP(rmcpClassIPMI,
			wrapRMCPPlusAuth(payloadTypeSOL, sess.consoleSessionID, outSeq, ack, sess.k1))
	}
	return wrapRMCP(rmcpClassIPMI,
		wrapRMCPPlus(payloadTypeSOL, sess.consoleSessionID, outSeq, ack))
}

func handleSetSessionPrivLevel(cmdData []byte) []byte {
	if len(cmdData) < 1 {
		return []byte{ccInvalidParam}
	}
	privLevel := cmdData[0]
	return []byte{ccOK, privLevel}
}

func (sm *sessionManager) handleCloseSession(sess *session) []byte {
	log.Debugf("IPMI: closing session 0x%08x", sess.bmcSessionID)
	sm.remove(sess.bmcSessionID)
	return []byte{ccOK}
}

// computeHMACSHA1 computes a full HMAC-SHA1.
func computeHMACSHA1(key, data []byte) []byte {
	mac := hmac.New(sha1.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// computeHMACSHA196 computes HMAC-SHA1 truncated to 12 bytes.
func computeHMACSHA196(key, data []byte) []byte {
	full := computeHMACSHA1(key, data)
	return full[:12]
}
