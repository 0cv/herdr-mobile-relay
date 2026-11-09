package hc

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestTextTransport(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "secret"}[secret], func(t *testing.T) {
			const text = "Upper é漢 !? spaces"
			received := make(chan string, 1)
			closed := make(chan struct{})
			c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				switch req.Method {
				case "terminal.get":
					reply(conn, req.ID, "result", map[string]any{"address": "t1", "pid": 42, "state": "live", "cols": 123, "rows": 37})
				case "terminal.attach":
					if req.Params["cols"] != float64(123) || req.Params["rows"] != float64(37) || req.Params["observe"] != false {
						t.Error("attach changed size or observes")
					}
					reply(conn, req.ID, "result", map[string]any{"channel": 7})
					fc := conn.(*framedTestConn)
					go func() {
						defer close(closed)
						ch, kind, p, err := readFrame(fc.reader)
						if err != nil || ch != 7 || kind != frameData {
							t.Error("wrong input frame", err)
						}
						received <- string(p)
						if _, _, _, err := readFrame(fc.reader); err == nil {
							t.Error("attachment not closed")
						}
					}()
				default:
					t.Errorf("unexpected RPC %s", req.Method)
				}
			}))
			var err error
			want := text
			if secret {
				err = c.SendSecret(t.Context(), "t1", text)
				want += "\r"
			} else {
				err = c.SendInput(t.Context(), "t1", herdr.PaneInput{Text: text})
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-received:
				if got != want {
					t.Fatal("bytes differ")
				}
			case <-time.After(time.Second):
				t.Fatal("no input")
			}
			<-closed
		})
	}
}

func TestTextRefusedBeforeConnect(t *testing.T) {
	c := New("no-socket")
	for _, text := range []string{"a\nb", "a\r", "\t", "\x00", "\x7f", "\u0085"} {
		err := c.SendText(t.Context(), "t1", text)
		if err == nil || !strings.Contains(err.Error(), "single-line") {
			t.Fatal(err)
		}
	}
}

type shortInputConn struct {
	net.Conn
	writes int
}

func (c *shortInputConn) SetWriteDeadline(time.Time) error { return nil }
func (c *shortInputConn) Write(p []byte) (int, error)      { c.writes++; return 10, nil }

func TestInputPartialWriteUnknown(t *testing.T) {
	c := &shortInputConn{}
	a := &attachment{conn: c, channel: 9}
	err := a.input([]byte("private-marker"))
	if !errors.Is(err, herdr.ErrDispatchedUnknown) || c.writes != 1 || strings.Contains(err.Error(), "private-marker") {
		t.Fatal(err, c.writes)
	}
}
