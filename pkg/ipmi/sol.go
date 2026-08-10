package ipmi

import (
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/serial"
)

// solState manages a single SOL session backed by the shared serial broker.
type solState struct {
	mu     sync.Mutex
	active bool
	outSeq byte
	sess   *session
}

var solSession = &solState{}

// solWriter adapts the broker's per-session output into IPMI SOL packets
// sent over UDP. It implements io.Writer so the broker's readLoop can
// push serial data through it.
type solWriter struct {
	sol *solState
}

func (w *solWriter) Write(p []byte) (int, error) {
	w.sol.sendData(p)
	return len(p), nil
}

// handleActivatePayload starts a SOL session over the shared serial broker.
func handleActivatePayload(sess *session, cmdData []byte) []byte {
	if len(cmdData) < 2 {
		return []byte{ccInvalidParam}
	}

	payloadType := cmdData[0] & 0x3F
	if payloadType != 0x01 {
		return []byte{ccInvalidParam}
	}

	solSession.mu.Lock()
	defer solSession.mu.Unlock()

	if solSession.active {
		return []byte{ccPayloadAlready}
	}

	broker := serial.GetBroker()
	writer := &solWriter{sol: solSession}

	if _, err := broker.Connect("ipmi-sol", writer); err != nil {
		log.Errorf("SOL: failed to connect to serial broker: %s", err)
		return []byte{ccUnspecified}
	}

	solSession.active = true
	solSession.outSeq = 0
	solSession.sess = sess

	log.Info("SOL: session activated via serial broker")

	// Response: cc(1) + aux(4) + inbound_size(2) + outbound_size(2) + port(2) + vlan(2)
	resp := make([]byte, 13)
	resp[0] = ccOK
	// resp[1:5] = auxiliary data (zeros)
	resp[5] = 0x00
	resp[6] = 0x04 // inbound payload size = 1024
	resp[7] = 0x00
	resp[8] = 0x04 // outbound payload size = 1024
	resp[9] = 0x6F
	resp[10] = 0x02 // UDP port = 623
	// resp[11:13] = VLAN (zeros)
	return resp
}

// handleDeactivatePayload stops the active SOL session.
func handleDeactivatePayload(cmdData []byte) []byte {
	solSession.stop()
	return []byte{ccOK}
}

func (sol *solState) stop() {
	sol.mu.Lock()
	defer sol.mu.Unlock()

	if !sol.active {
		return
	}

	sol.active = false
	serial.GetBroker().Disconnect("ipmi-sol")
	sol.sess = nil

	log.Info("SOL: session deactivated")
}

// stopIfSession deactivates SOL only if it is bound to sess. Used when a session
// is closed or reaped so an abandoned console cannot pin the serial broker open.
func (sol *solState) stopIfSession(sess *session) {
	sol.mu.Lock()
	bound := sol.active && sol.sess == sess
	sol.mu.Unlock()
	if bound {
		sol.stop()
	}
}

// sendData sends SOL payload data to the remote console.
func (sol *solState) sendData(data []byte) {
	sol.mu.Lock()
	if !sol.active || sol.sess == nil {
		sol.mu.Unlock()
		return
	}

	sol.outSeq++
	seq := sol.outSeq
	sess := sol.sess
	srv := sess.server
	addr := sess.addr
	sol.mu.Unlock()

	if srv == nil || addr == nil {
		return
	}

	// SOL payload: seq(1) + ackSeq(1) + acceptedChars(1) + status(1) + data
	solPayload := make([]byte, 4+len(data))
	solPayload[0] = seq
	// solPayload[1] = 0 — ack seq
	// solPayload[2] = 0 — accepted chars
	// solPayload[3] = 0 — status OK
	copy(solPayload[4:], data)

	outSeq := atomic.AddUint32(&sess.outSeq, 1)

	var pkt []byte
	if sess.integAlgo != integAlgoNone && sess.k1 != nil {
		pkt = wrapRMCP(rmcpClassIPMI,
			wrapRMCPPlusAuth(payloadTypeSOL, sess.consoleSessionID, outSeq, solPayload, sess.k1))
	} else {
		pkt = wrapRMCP(rmcpClassIPMI,
			wrapRMCPPlus(payloadTypeSOL, sess.consoleSessionID, outSeq, solPayload))
	}

	if _, err := srv.conn.WriteToUDP(pkt, addr); err != nil {
		log.Errorf("SOL: send error: %s", err)
	}
}
