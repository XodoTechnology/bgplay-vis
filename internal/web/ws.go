package web

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Minimal RFC 6455 server: text frames out, ping/pong/close in.
// No extensions, no fragmentation reassembly needed (clients only pong).

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

var errNotWS = errors.New("not a websocket request")

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if r.Header.Get("Upgrade") != "websocket" {
		return nil, errNotWS
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errNotWS
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijack unsupported")
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, br: rw.Reader}, nil
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	hdr := []byte{0x80 | op}
	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, byte(l))
	case l <= 0xffff:
		hdr = append(hdr, 126, byte(l>>8), byte(l))
	default:
		hdr = append(hdr, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(l))
		hdr = append(hdr, b[:]...)
	}
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

func (c *wsConn) WriteText(b []byte) error { return c.writeFrame(1, b) }
func (c *wsConn) Ping() error              { return c.writeFrame(9, nil) }
func (c *wsConn) Close() error {
	c.writeFrame(8, nil)
	return c.conn.Close()
}

// readLoop consumes client frames: replies pong to ping, exits on
// close/error. Client data frames are ignored.
func (c *wsConn) readLoop(done func()) {
	defer done()
	for {
		c.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		b0, err := c.br.ReadByte()
		if err != nil {
			return
		}
		op := b0 & 0x0f
		b1, err := c.br.ReadByte()
		if err != nil {
			return
		}
		masked := b1&0x80 != 0
		l := uint64(b1 & 0x7f)
		if l == 126 {
			var ext [2]byte
			if _, err := readN(c.br, ext[:]); err != nil {
				return
			}
			l = uint64(binary.BigEndian.Uint16(ext[:]))
		} else if l == 127 {
			var ext [8]byte
			if _, err := readN(c.br, ext[:]); err != nil {
				return
			}
			l = binary.BigEndian.Uint64(ext[:])
		}
		if l > 1<<20 {
			return // absurd control/data size from a client
		}
		var mask [4]byte
		if masked {
			if _, err := readN(c.br, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, l)
		if _, err := readN(c.br, payload); err != nil {
			return
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 9: // ping → pong
			c.writeFrame(10, payload)
		case 8: // close
			return
		}
	}
}

func readN(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
