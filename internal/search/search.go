// search handles beacons, search requests and responses
package search

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"time"

	"github.com/quentinmit/go-pvaccess/internal/connection"
	"github.com/quentinmit/go-pvaccess/internal/ctxlog"
	"github.com/quentinmit/go-pvaccess/internal/proto"
	"github.com/quentinmit/go-pvaccess/internal/udpconn"
	"github.com/quentinmit/go-pvaccess/pvdata"
)

type server struct {
	lastBeacon proto.BeaconMessage
}

const startupInterval = time.Second
const startupCount = 15

// TODO: EPICS_PVA_BEACON_PERIOD environment variable
const beaconInterval = 5 * time.Second

// Serve transmits beacons and listens for searches on every interface on the machine.
// If serverAddr specifies an IP, beacons will advertise that address.
// If it does not, beacons will advertise the address of the interface they are transmitted on.
func Serve(ctx context.Context, serverAddr *net.TCPAddr) error {
	var beacon proto.BeaconMessage
	if _, err := rand.Read(beacon.GUID[:]); err != nil {
		return err
	}
	if len(serverAddr.IP) > 0 {
		copy(beacon.ServerAddress[:], serverAddr.IP.To16())
	}
	beacon.ServerPort = uint16(serverAddr.Port)
	beacon.Protocol = "tcp"

	// We need a bunch of sockets.
	// One socket on INADDR_ANY with a random port to send beacons from
	// For each interface,
	//   Listen on addr:5076
	//     IP_MULTICAST_IF 127.0.0.1
	//     IP_MULTICAST_LOOP 1
	//   Listen on broadcast:5076 (if interface has broadcast flag)
	// One socket listening on 224.0.0.128 on lo
	//   Listen on 224.0.0.128:5076
	//   IP_ADD_MEMBERSHIP 224.0.0.128, 127.0.0.1

	ln, err := udpconn.Listen(ctx)
	if err != nil {
		return err
	}

	beaconSender := connection.New(ln.BroadcastConn(), proto.FLAG_FROM_SERVER)
	beaconSender.Version = pvdata.PVByte(2)

	ctxlog.L(ctx).Infof("sending beacons to %v", ln.BroadcastSendAddresses())

	go func() {
		if err := (&searchServer{
			GUID:       beacon.GUID,
			ServerPort: serverAddr.Port,
		}).serve(ctx, ln); err != nil && err != io.EOF {
			ctxlog.L(ctx).Errorf("failed handling search request: %v", err)
		}
	}()

	ticker := time.NewTicker(startupInterval)
	defer func() { ticker.Stop() }()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			beacon.BeaconSequenceID++
			beaconSender.SendApp(ctx, proto.APP_BEACON, &beacon)
			i++
			if i == startupCount {
				ticker.Stop()
				ticker = time.NewTicker(beaconInterval)
			}
		}
	}
}

type searchServer struct {
	GUID       [12]byte
	ServerPort int
}

func (s *searchServer) serve(ctx context.Context, ln *udpconn.Listener) (err error) {
	defer func() {
		if err != nil {
			ctxlog.L(ctx).Errorf("error listening for search requests: %v", err)
		}
	}()
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		laddr := conn.LocalAddr()
		ctx = ctxlog.WithField(ctx, "local_addr", laddr)
		go s.handleConnection(ctx, ln.LocalAddr(), conn)
	}
}

func (s *searchServer) handleConnection(ctx context.Context, laddr *net.UDPAddr, conn *udpconn.Conn) (err error) {
	defer func() {
		if err != nil && err != io.EOF {
			ctxlog.L(ctx).Warnf("error handling UDP packet: %v", err)
		}
	}()
	defer conn.Close()

	ctx = ctxlog.WithField(ctx, "remote_addr", conn.Addr())

	c := connection.New(conn, proto.FLAG_FROM_SERVER)
	c.Version = pvdata.PVByte(2)
	msg, err := c.Next(ctx)
	if err != nil {
		return err
	}

	if msg.Header.MessageCommand == proto.APP_SEARCH_REQUEST {
		var req proto.SearchRequest
		if err := msg.Decode(&req); err != nil {
			return err
		}
		ctxlog.L(ctx).Debugf("search request received: %#v", req)
		// Process search
		// TODO: Send to local multicast group for other local apps
		// TODO: Clear unicast flag, set response address to raddr if unset, add origin tag prefix,
		resp := &proto.SearchResponse{
			GUID:             s.GUID,
			SearchSequenceID: req.SearchSequenceID,
			ServerPort:       pvdata.PVUShort(s.ServerPort),
			Protocol:         "tcp",
		}
		copy(resp.ServerAddress[:], []byte(laddr.IP.To16()))
		var found []pvdata.PVUInt
		// TODO: Find channels
		if len(found) == 0 {
			resp.Found = false
			for _, channel := range req.Channels {
				resp.SearchInstanceIDs = append(resp.SearchInstanceIDs, channel.SearchInstanceID)
			}
		}
		if len(found) > 0 || req.Flags&proto.SEARCH_REPLY_REQUIRED == proto.SEARCH_REPLY_REQUIRED {
			c.SendApp(ctx, proto.APP_SEARCH_RESPONSE, resp)
			// TODO: Send response to req.ResponseAddr if set
		}
	}
	return io.EOF
}
