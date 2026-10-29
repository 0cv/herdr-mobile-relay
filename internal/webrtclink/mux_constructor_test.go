package webrtclink

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/pion/ice/v4"
)

type queuedSTUNConn struct {
	*net.UDPConn
	processed chan struct{}
	read      bool
}

func (c *queuedSTUNConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	if c.read {
		close(c.processed)
		return 0, nil, net.ErrClosed
	}
	c.read = true
	return c.UDPConn.ReadFrom(buffer)
}

func TestUniversalUDPMuxProcessesQueuedSTUNDuringConstruction(t *testing.T) {
	packet := make([]byte, 20)
	binary.BigEndian.PutUint16(packet, 1)
	binary.BigEndian.PutUint32(packet[4:], stunMagicCookie)
	for range 32 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer conn.Close()
			sender, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			if _, err := sender.Write(packet); err != nil {
				t.Fatal(err)
			}
			queued := &queuedSTUNConn{UDPConn: conn, processed: make(chan struct{})}
			mux := ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: queued})
			defer mux.Close()
			select {
			case <-queued.processed:
			case <-time.After(testTimeout):
				t.Fatal("mux did not process the queued STUN packet")
			}
		}()
	}
}
