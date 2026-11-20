package libvirt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"github.com/hk9890/werft/internal/failure"
)

const (
	remoteProgram        = 0x20008086
	procConnectOpen      = 1
	procConnectClose     = 2
	procDomainLookup     = 23
	procAuthList         = 66
	procDomainScreenshot = 211
)

var png = []byte("\x89PNG\r\n\x1a\nimage data")

// fakeDaemon answers the calls Screenshot makes. The screenshot stream sends
// png in one data packet, then ends as end does.
type fakeDaemon struct {
	conn net.Conn
	end  func(d *fakeDaemon, serial uint32)
}

func (d *fakeDaemon) send(proc, typ, serial, status uint32, payload []byte) {
	var b bytes.Buffer
	for _, v := range []uint32{uint32(28 + len(payload)), remoteProgram, 1, proc, typ, serial, status} {
		_ = binary.Write(&b, binary.BigEndian, v)
	}
	b.Write(payload)
	_, _ = d.conn.Write(b.Bytes())
}

func (d *fakeDaemon) serve() {
	for {
		head := make([]byte, 28)
		if _, err := io.ReadFull(d.conn, head); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(head[0:4])-28)
		if _, err := io.ReadFull(d.conn, body); err != nil {
			return
		}
		proc, serial := binary.BigEndian.Uint32(head[12:16]), binary.BigEndian.Uint32(head[20:24])
		switch proc {
		case procAuthList:
			d.send(proc, socket.Reply, serial, socket.StatusOK, xdrUint32(0))
		case procConnectOpen, procConnectClose:
			d.send(proc, socket.Reply, serial, socket.StatusOK, nil)
		case procDomainLookup:
			payload := append(xdrString("vm1"), make([]byte, 16+4)...)
			d.send(proc, socket.Reply, serial, socket.StatusOK, payload)
		case procDomainScreenshot:
			d.send(proc, socket.Reply, serial, socket.StatusOK, append(xdrUint32(1), xdrString("image/png")...))
			d.send(proc, socket.Stream, serial, socket.StatusContinue, png)
			d.end(d, serial)
		}
	}
}

func xdrUint32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func xdrString(s string) []byte {
	b := append(xdrUint32(uint32(len(s))), s...)
	return append(b, make([]byte, (4-len(s)%4)%4)...)
}

func endOfStream(d *fakeDaemon, serial uint32) {
	d.send(procDomainScreenshot, socket.Stream, serial, socket.StatusOK, nil)
}

func hangUp(d *fakeDaemon, _ uint32) { _ = d.conn.Close() }

func streamError(d *fakeDaemon, serial uint32) {
	var payload []byte
	payload = append(payload, xdrUint32(uint32(golibvirt.ErrOperationInvalid))...)
	payload = append(payload, xdrUint32(0)...)
	payload = append(payload, 0, 0, 0, 0)
	payload = append(payload, xdrUint32(1)...)
	payload = append(payload, xdrString("domain is not running")...)
	payload = append(payload, xdrUint32(2)...)
	d.send(procDomainScreenshot, socket.Stream, serial, socket.StatusError, payload)
}

func adapterOn(t *testing.T, end func(*fakeDaemon, uint32)) *Adapter {
	t.Helper()
	client, server := net.Pipe()
	go (&fakeDaemon{conn: server, end: end}).serve()
	l := golibvirt.NewWithDialer(dialers.NewAlreadyConnected(client))
	if err := l.Connect(); err != nil {
		t.Fatalf("connect to fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &Adapter{l: l}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestScreenshotSniffsTheTypeWhenTheEndPacketIsEmpty(t *testing.T) {
	var out bytes.Buffer
	mime, err := adapterOn(t, endOfStream).Screenshot("vm1", &out)
	if err != nil || mime != "image/png" || !bytes.Equal(out.Bytes(), png) {
		t.Errorf("Screenshot = %q, %v, %d bytes", mime, err, out.Len())
	}
}

func TestScreenshotFailsWhenTheConnectionDrops(t *testing.T) {
	_, err := adapterOn(t, hangUp).Screenshot("vm1", io.Discard)
	if !errors.Is(err, golibvirt.ErrInterrupted) {
		t.Errorf("Screenshot error = %v, want ErrInterrupted", err)
	}
}

func TestACallAfterTheConnectionDropsReportsTheLostConnection(t *testing.T) {
	a := adapterOn(t, hangUp)
	_, _ = a.Screenshot("vm1", io.Discard)
	<-a.l.Disconnected()
	_, err := a.DomainState("vm1")
	want := "domain vm1: lost the connection to libvirt (libvirtd restarted?); the domain may still run"
	if err == nil || err.Error() != want || !failure.Is(err, failure.Failed) {
		t.Errorf("DomainState error = %v (%s), want %q", err, failure.KindOf(err), want)
	}
}

func TestScreenshotFailsWhenTheWriterFails(t *testing.T) {
	a := adapterOn(t, endOfStream)
	_, err := a.Screenshot("vm1", failingWriter{})
	if err == nil || err.Error() != "screenshot of vm1: no space left on device" {
		t.Errorf("Screenshot error = %v", err)
	}
	if _, err := a.Screenshot("vm1", io.Discard); err != nil {
		t.Errorf("the connection is unusable after a write error: %v", err)
	}
}

func TestScreenshotFailsOnALibvirtError(t *testing.T) {
	_, err := adapterOn(t, streamError).Screenshot("vm1", io.Discard)
	if !failure.Is(err, failure.Conflict) {
		t.Errorf("Screenshot error = %v (%s), want conflict", err, failure.KindOf(err))
	}
}
