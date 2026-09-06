package mdns

// conn.go owns the multicast sockets.
//
// One socket per address family, joined to the mDNS group on exactly one
// interface. The interface is resolved once, by the caller, and never looked
// up again: the receive path must not touch the kernel, which is what made a
// general-purpose DNS-SD library cost a netlink dump per packet here.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// The mDNS group addresses and port (RFC 6762 §3).
var (
	groupV4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	groupV6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
)

// readBufSize is the largest datagram we will read. RFC 6762 §17 allows
// messages up to the interface MTU; 9000 covers jumbo frames and is a trivial
// per-socket cost even on a 256MB device.
const readBufSize = 9000

// packet is one received datagram. buf aliases the reader's own buffer and is
// valid only until that reader's next read.
type packet struct {
	buf  []byte
	from *net.UDPAddr
	v6   bool
}

// conn is the responder's socket pair.
//
// v4 and v6 are set once by listen and never reassigned. A nil one means that
// family could not be joined at all. They must stay immutable because the two
// read loops and the sender goroutine all touch them concurrently with close:
// clearing them on close would be a data race, and one that lands precisely
// on the shutdown path, where a lost close leaves the multicast group joined
// and the sockets bound for the life of the process.
type conn struct {
	iface *net.Interface
	v4    *ipv4.PacketConn
	v6    *ipv6.PacketConn

	closeOnce sync.Once
}

// reusePort lets this responder share :5353 with any other mDNS stack on the
// device (avahi, a container's responder). Without both options the bind fails
// outright wherever something already holds the port, taking discovery down
// rather than degrading it.
func reusePort(_, _ string, c syscall.RawConn) error {
	var opErr error
	err := c.Control(func(fd uintptr) {
		if opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); opErr != nil {
			return
		}
		opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return opErr
}

// Counting sockets on :5353 is a common way to look for a leak here, so know
// what to expect: this responder binds exactly two, one per family, for its
// whole life. Four is also normal — pion/ice opens its own multicast DNS
// listener (another v4/v6 pair) to resolve .local ICE candidates whenever a
// WebRTC video session starts, and that pair is not ours. Confirmed on
// hardware: a device with no video session since boot holds two, and the
// count rises to four the moment /api/vm/video is hit.
//
// listen opens and joins both families on iface.
//
// A family that cannot be joined is left nil rather than failing the whole
// responder: an IPv6-disabled kernel is a normal deployment, and losing IPv4
// discovery over it would be worse than serving one family.
//
// Multicast loopback is disabled on both. Without that the kernel delivers
// every packet we send straight back to us, and a responder that answers its
// own announcements is pure amplification on a link that is already busy.
func listen(iface *net.Interface) (*conn, error) {
	c := &conn{iface: iface}
	lc := net.ListenConfig{Control: reusePort}

	if pc, err := lc.ListenPacket(context.Background(), "udp4", ":5353"); err == nil {
		p := ipv4.NewPacketConn(pc)
		if err := p.JoinGroup(iface, groupV4); err != nil {
			_ = pc.Close()
		} else {
			_ = p.SetControlMessage(ipv4.FlagInterface, true)
			_ = p.SetMulticastInterface(iface)
			_ = p.SetMulticastLoopback(false)
			c.v4 = p
		}
	}

	if pc, err := lc.ListenPacket(context.Background(), "udp6", ":5353"); err == nil {
		p := ipv6.NewPacketConn(pc)
		if err := p.JoinGroup(iface, groupV6); err != nil {
			_ = pc.Close()
		} else {
			_ = p.SetControlMessage(ipv6.FlagInterface, true)
			_ = p.SetMulticastInterface(iface)
			_ = p.SetMulticastLoopback(false)
			c.v6 = p
		}
	}

	if c.v4 == nil && c.v6 == nil {
		return nil, fmt.Errorf("mdns: could not join the multicast group on %s", iface.Name)
	}
	return c, nil
}

// close releases both sockets. Idempotent.
//
// Closing is also what stops the read goroutines: a blocked ReadFrom returns
// net.ErrClosed. Leaving one open would leave the group joined and the kernel
// delivering packets to a responder that no longer exists — which is how a
// restart ends up with two live generations both being handed every packet.
func (c *conn) close() {
	c.closeOnce.Do(func() {
		if c.v4 != nil {
			_ = c.v4.Close()
		}
		if c.v6 != nil {
			_ = c.v6.Close()
		}
	})
}

// readV4 and readV6 block for one datagram each, reusing buf.
func (c *conn) readV4(buf []byte) (packet, error) {
	if c.v4 == nil {
		return packet{}, net.ErrClosed
	}
	n, _, src, err := c.v4.ReadFrom(buf)
	if err != nil {
		return packet{}, err
	}
	from, _ := src.(*net.UDPAddr)
	return packet{buf: buf[:n], from: from}, nil
}

func (c *conn) readV6(buf []byte) (packet, error) {
	if c.v6 == nil {
		return packet{}, net.ErrClosed
	}
	n, _, src, err := c.v6.ReadFrom(buf)
	if err != nil {
		return packet{}, err
	}
	from, _ := src.(*net.UDPAddr)
	return packet{buf: buf[:n], v6: true, from: from}, nil
}

// send writes buf to the group, or to dst when dst is non-nil (a unicast
// reply). v6 selects the family.
func (c *conn) send(buf []byte, v6 bool, dst *net.UDPAddr) error {
	if v6 {
		if c.v6 == nil {
			return net.ErrClosed
		}
		target := groupV6
		if dst != nil {
			target = dst
		}
		_, err := c.v6.WriteTo(buf, &ipv6.ControlMessage{IfIndex: c.iface.Index}, target)
		return err
	}
	if c.v4 == nil {
		return net.ErrClosed
	}
	target := groupV4
	if dst != nil {
		target = dst
	}
	_, err := c.v4.WriteTo(buf, &ipv4.ControlMessage{IfIndex: c.iface.Index}, target)
	return err
}
