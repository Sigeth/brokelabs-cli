package kube

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sync"
	"time"

	gwebsocket "github.com/gorilla/websocket"
	"k8s.io/client-go/transport/websocket"
)

// kubevirtPlainProtocol is the websocket subprotocol of KubeVirt's
// portforward subresource: binary messages carry the TCP stream as is.
const kubevirtPlainProtocol = "plain.kubevirt.io"

// VMPortForward opens a TCP connection to port of a KubeVirt
// VirtualMachineInstance through the API server: KubeVirt's portforward
// subresource, what "virtctl port-forward" uses. The machines of an
// instance are only reachable from inside its network otherwise.
func (c *Cluster) VMPortForward(ctx context.Context, namespace, vmi string, port int) (net.Conn, error) {
	rt, holder, err := websocket.RoundTripperFor(c.Config)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(c.Config.Host)
	if err != nil {
		return nil, err
	}
	u.Path = path.Join(u.Path, "/apis/subresources.kubevirt.io/v1/namespaces", namespace,
		"virtualmachineinstances", vmi, "portforward", fmt.Sprint(port), "tcp")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	conn, err := websocket.Negotiate(rt, holder, req, kubevirtPlainProtocol)
	if err != nil {
		return nil, fmt.Errorf("port-forwarding to %s/%s:%d: %w", namespace, vmi, port, err)
	}
	return &wsConn{ws: conn}, nil
}

// wsConn is a net.Conn over binary websocket messages.
type wsConn struct {
	ws      *gwebsocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			kind, r, err := c.ws.NextReader()
			if err != nil {
				if gwebsocket.IsCloseError(err, gwebsocket.CloseNormalClosure) {
					return 0, io.EOF
				}
				return 0, err
			}
			if kind != gwebsocket.BinaryMessage {
				continue
			}
			c.reader = r
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(gwebsocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
