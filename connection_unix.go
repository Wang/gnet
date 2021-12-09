// Copyright (c) 2019 Andy Pan
// Copyright (c) 2018 Joshua J Baker
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux || freebsd || dragonfly || darwin
// +build linux freebsd dragonfly darwin

package gnet

import (
	"net"
	"os"

	"golang.org/x/sys/unix"

	"github.com/panjf2000/gnet/internal/io"
	"github.com/panjf2000/gnet/internal/netpoll"
	"github.com/panjf2000/gnet/internal/socket"
	"github.com/panjf2000/gnet/pkg/mixedbuffer"
	"github.com/panjf2000/gnet/pkg/pool/bytebuffer"
	"github.com/panjf2000/gnet/pkg/pool/byteslice"
	rbPool "github.com/panjf2000/gnet/pkg/pool/ringbuffer"
	"github.com/panjf2000/gnet/pkg/ringbuffer"
)

type conn struct {
	fd             int                     // file descriptor
	ctx            interface{}             // user-defined context
	peer           unix.Sockaddr           // remote socket address
	loop           *eventloop              // connected event-loop
	codec          ICodec                  // codec for TCP
	opened         bool                    // connection opened event fired
	packets        [][]byte                // reuse it for multiple byte slices
	localAddr      net.Addr                // local addr
	remoteAddr     net.Addr                // remote addr
	inboundBuffer  *ringbuffer.RingBuffer  // buffer for leftover data from the peer
	transitBuffer  *bytebuffer.ByteBuffer  // buffer for a complete packet
	outboundBuffer *mixedbuffer.Buffer     // buffer for data that is eligible to be sent to the peer
	pollAttachment *netpoll.PollAttachment // connection attachment for poller
}

func newTCPConn(fd int, el *eventloop, sa unix.Sockaddr, codec ICodec, localAddr, remoteAddr net.Addr) (c *conn) {
	c = &conn{
		fd:             fd,
		peer:           sa,
		loop:           el,
		codec:          codec,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		inboundBuffer:  rbPool.GetWithSize(ringbuffer.MaxStreamBufferCap),
		outboundBuffer: mixedbuffer.New(ringbuffer.MaxStreamBufferCap),
	}
	c.pollAttachment = netpoll.GetPollAttachment()
	c.pollAttachment.FD, c.pollAttachment.Callback = fd, c.handleEvents
	return
}

func (c *conn) releaseTCP() {
	c.opened = false
	c.peer = nil
	c.ctx = nil
	if addr, ok := c.localAddr.(*net.TCPAddr); ok && c.localAddr != c.loop.ln.addr {
		byteslice.Put(addr.IP)
	}
	if addr, ok := c.remoteAddr.(*net.TCPAddr); ok {
		byteslice.Put(addr.IP)
	}
	c.localAddr = nil
	c.remoteAddr = nil
	rbPool.Put(c.inboundBuffer)
	c.inboundBuffer = ringbuffer.EmptyRingBuffer
	c.outboundBuffer.Release()
	netpoll.PutPollAttachment(c.pollAttachment)
	c.pollAttachment = nil
	c.packets = c.packets[:0]
}

func newUDPConn(fd int, el *eventloop, localAddr net.Addr, sa unix.Sockaddr, connected bool) (c *conn) {
	c = &conn{
		fd:         fd,
		peer:       sa,
		loop:       el,
		localAddr:  localAddr,
		remoteAddr: socket.SockaddrToUDPAddr(sa),
	}
	if connected {
		c.peer = nil
	}
	return
}

func (c *conn) releaseUDP() {
	c.ctx = nil
	if addr, ok := c.localAddr.(*net.UDPAddr); ok && c.localAddr != c.loop.ln.addr {
		byteslice.Put(addr.IP)
	}
	if addr, ok := c.remoteAddr.(*net.UDPAddr); ok {
		byteslice.Put(addr.IP)
	}
	c.localAddr = nil
	c.remoteAddr = nil
	netpoll.PutPollAttachment(c.pollAttachment)
	c.pollAttachment = nil
}

func (c *conn) open(buf []byte) error {
	defer c.loop.eventHandler.AfterWrite(c, buf)

	c.loop.eventHandler.PreWrite(c)
	n, err := unix.Write(c.fd, buf)
	if err != nil && err == unix.EAGAIN {
		_, _ = c.outboundBuffer.Write(buf)
		return nil
	}

	if err == nil && n < len(buf) {
		_, _ = c.outboundBuffer.Write(buf[n:])
	}

	return err
}

func (c *conn) read() ([]byte, error) {
	return c.codec.Decode(c)
}

func (c *conn) write(buf []byte) (err error) {
	defer c.loop.eventHandler.AfterWrite(c, buf)

	var packet []byte
	if packet, err = c.codec.Encode(c, buf); err != nil {
		return
	}

	c.loop.eventHandler.PreWrite(c)

	// If there is pending data in outbound buffer, the current data ought to be appended to the outbound buffer
	// for maintaining the sequence of network packets.
	if !c.outboundBuffer.IsEmpty() {
		_, _ = c.outboundBuffer.Write(packet)
		return
	}

	var n int
	if n, err = unix.Write(c.fd, packet); err != nil {
		// A temporary error occurs, append the data to outbound buffer, writing it back to the peer in the next round.
		if err == unix.EAGAIN {
			_, _ = c.outboundBuffer.Write(packet)
			err = c.loop.poller.ModReadWrite(c.pollAttachment)
			return
		}
		return c.loop.loopCloseConn(c, os.NewSyscallError("write", err))
	}
	// Failed to send all data back to the peer, buffer the leftover data for the next round.
	if n < len(packet) {
		_, _ = c.outboundBuffer.Write(packet[n:])
		err = c.loop.poller.ModReadWrite(c.pollAttachment)
	}
	return
}

func (c *conn) writev(bs [][]byte) (err error) {
	defer func() {
		for _, b := range bs {
			c.loop.eventHandler.AfterWrite(c, b)
		}
		c.packets = c.packets[:0]
	}()

	var sum int
	for _, b := range bs {
		var packet []byte
		if packet, err = c.codec.Encode(c, b); err != nil {
			return
		}
		c.packets = append(c.packets, packet)
		sum += len(packet)
		c.loop.eventHandler.PreWrite(c)
	}

	// If there is pending data in outbound buffer, the current data ought to be appended to the outbound buffer
	// for maintaining the sequence of network packets.
	if !c.outboundBuffer.IsEmpty() {
		_, _ = c.outboundBuffer.Writev(c.packets)
		return
	}

	var n int
	if n, err = io.Writev(c.fd, c.packets); err != nil {
		// A temporary error occurs, append the data to outbound buffer, writing it back to the peer in the next round.
		if err == unix.EAGAIN {
			_, _ = c.outboundBuffer.Writev(c.packets)
			err = c.loop.poller.ModReadWrite(c.pollAttachment)
			return
		}
		return c.loop.loopCloseConn(c, os.NewSyscallError("write", err))
	}
	// Failed to send all data back to the peer, buffer the leftover data for the next round.
	if n < sum {
		var pos int
		for i := range c.packets {
			np := len(c.packets[i])
			if n < np {
				c.packets[i] = c.packets[i][n:]
				pos = i
				break
			}
			n -= np
		}
		_, _ = c.outboundBuffer.Writev(c.packets[pos:])
		err = c.loop.poller.ModReadWrite(c.pollAttachment)
	}
	return
}

func (c *conn) asyncWrite(itf interface{}) error {
	if !c.opened {
		return nil
	}
	return c.write(itf.([]byte))
}

func (c *conn) asyncWritev(itf interface{}) error {
	if !c.opened {
		return nil
	}
	return c.writev(itf.([][]byte))
}

func (c *conn) sendTo(buf []byte) error {
	c.loop.eventHandler.PreWrite(c)
	defer c.loop.eventHandler.AfterWrite(c, buf)
	if c.peer == nil {
		return unix.Send(c.fd, buf, 0)
	}
	return unix.Sendto(c.fd, buf, 0, c.peer)
}

// ================================== Non-concurrency-safe API's ==================================

func (c *conn) Read() []byte {
	head, tail := c.inboundBuffer.PeekAll()
	if tail == nil {
		return head
	}
	if c.transitBuffer == nil {
		c.transitBuffer = c.inboundBuffer.ByteBuffer()
		return c.transitBuffer.B
	}
	c.transitBuffer.Reset()
	_, _ = c.transitBuffer.Write(head)
	_, _ = c.transitBuffer.Write(tail)
	return c.transitBuffer.B
}

func (c *conn) ResetBuffer() {
	c.inboundBuffer.Reset()
	if c.transitBuffer != nil {
		c.transitBuffer.Reset()
	}
}

func (c *conn) ReadN(n int) (int, []byte) {
	inBufferLen := c.inboundBuffer.Length()
	if inBufferLen <= n || n <= 0 {
		return inBufferLen, c.Read()
	}
	head, tail := c.inboundBuffer.Peek(n)
	if tail == nil {
		return n, head
	}
	if c.transitBuffer == nil {
		c.transitBuffer = bytebuffer.Get()
	} else {
		c.transitBuffer.Reset()
	}
	_, _ = c.transitBuffer.Write(head)
	_, _ = c.transitBuffer.Write(tail)
	return n, c.transitBuffer.B
}

func (c *conn) ShiftN(n int) int {
	c.inboundBuffer.Discard(n)
	if c.transitBuffer != nil {
		c.transitBuffer.Reset()
	}
	return n
}

func (c *conn) BufferLength() int {
	return c.inboundBuffer.Length()
}

func (c *conn) Context() interface{}       { return c.ctx }
func (c *conn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *conn) LocalAddr() net.Addr        { return c.localAddr }
func (c *conn) RemoteAddr() net.Addr       { return c.remoteAddr }

// ==================================== Concurrency-safe API's ====================================

func (c *conn) AsyncWrite(buf []byte) error {
	return c.loop.poller.Trigger(c.asyncWrite, buf)
}

func (c *conn) AsyncWritev(bs [][]byte) error {
	return c.loop.poller.Trigger(c.asyncWritev, bs)
}

func (c *conn) SendTo(buf []byte) error {
	return c.sendTo(buf)
}

func (c *conn) Wake() error {
	return c.loop.poller.UrgentTrigger(func(_ interface{}) error { return c.loop.loopWake(c) }, nil)
}

func (c *conn) Close() error {
	return c.loop.poller.Trigger(func(_ interface{}) error { return c.loop.loopCloseConn(c, nil) }, nil)
}
