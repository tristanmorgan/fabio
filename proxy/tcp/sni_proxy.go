package tcp

import (
	"bufio"
	gkm "github.com/go-kit/kit/metrics"
	"io"
	"log"
	"net"
	"time"

	"github.com/fabiolb/fabio/route"
)

// SNIProxy implements an SNI aware transparent TCP proxy which captures the
// TLS client hello, extracts the host name and uses it for finding the
// upstream server. Then it replays the ClientHello message and copies data
// transparently allowing to route a TLS connection based on the SNI header
// without decrypting it.
type SNIProxy struct {

	// Conn counts the number of connections.
	Conn gkm.Counter

	// ConnFail counts the failed upstream connection attempts.
	ConnFail gkm.Counter

	// Noroute counts the failed Lookup() calls.
	Noroute gkm.Counter

	// Lookup returns a target host for the given server name.
	// The proxy will panic if this value is nil.
	Lookup func(host string) *route.Target

	// DialTimeout sets the timeout for establishing the outbound
	// connection.
	DialTimeout time.Duration
}

func (p *SNIProxy) ServeTCP(in net.Conn) error {
	defer in.Close()

	if p.Conn != nil {
		p.Conn.Add(1)
	}

	tlsReader := bufio.NewReader(in)
	tlsHeaders, err := tlsReader.Peek(9)
	if err != nil {
		log.Print("[DEBUG] tcp+sni: TLS handshake failed (failed to peek data)")
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return err
	}

	bufferSize, err := clientHelloBufferSize(tlsHeaders)
	if err != nil {
		log.Printf("[DEBUG] tcp+sni: TLS handshake failed (%s)", err)
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return err
	}

	data := make([]byte, bufferSize)
	_, err = io.ReadFull(tlsReader, data)
	if err != nil {
		log.Printf("[DEBUG] tcp+sni: TLS handshake failed (%s)", err)
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return err
	}

	// readServerName wants only the handshake message so ignore the first
	// 5 bytes which is the TLS record header
	host, ok := readServerName(data[5:])
	if !ok {
		log.Print("[DEBUG] tcp+sni: TLS handshake failed (unable to parse client hello)")
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return nil
	}

	if host == "" {
		log.Print("[DEBUG] tcp+sni: server_name missing")
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return nil
	}

	t := p.Lookup(host)
	if t == nil {
		if p.Noroute != nil {
			p.Noroute.Add(1)
		}
		return nil
	}
	addr := t.URL.Host

	if t.AccessDeniedTCP(in) {
		return nil
	}

	out, err := net.DialTimeout("tcp", addr, p.DialTimeout)
	if err != nil {
		log.Print("[WARN] tcp+sni: cannot connect to upstream ", addr)
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return err
	}
	defer out.Close()

	// enable PROXY protocol support on outbound connection
	if t.ProxyProto {
		err := WriteProxyHeader(out, in)
		if err != nil {
			log.Print("[WARN] tcp+sni: write proxy protocol header failed. ", err)
			if p.ConnFail != nil {
				p.ConnFail.Add(1)
			}
			return err
		}
	}

	// write the data already read from the connection
	n, err := out.Write(data)
	if err != nil {
		log.Print("[WARN] tcp+sni: copy client hello failed. ", err)
		if p.ConnFail != nil {
			p.ConnFail.Add(1)
		}
		return err
	}

	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader, c gkm.Counter) {
		errc <- copyBuffer(dst, src, c)
	}

	// we've received the ClientHello already
	if t.RxCounter != nil {
		t.RxCounter.Add(float64(n))
	}

	go cp(in, out, t.RxCounter)
	go cp(out, in, t.TxCounter)
	err = <-errc
	if err != nil && err != io.EOF {
		log.Print("[WARN]: tcp+sni:  ", err)
		return err
	}
	return nil
}
