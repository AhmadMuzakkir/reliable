package reliable

import (
	"fmt"
	"github.com/lithdew/seq"
	"io"
	"net"
	"sync"
	"time"
)

type Conn struct {
	writeBufferSize uint16 // write buffer size that must be a divisor of 65536
	readBufferSize  uint16 // read buffer size that must be a divisor of 65536

	updatePeriod  time.Duration // how often time-dependant parts of the protocol get checked
	resendTimeout time.Duration // how long we wait until unacked packets should be resent

	conn net.PacketConn
	addr net.Addr
	pool *Pool

	ph PacketHandler
	eh ErrorHandler

	mu   sync.Mutex    // mutex over everything
	die  bool          // is this conn closed?
	exit chan struct{} // signal channel to close the conn

	lui uint16    // last sent packet index that hasn't been sent via an ack yet
	oui uint16    // oldest sent packet index that hasn't been acked yet
	ouc sync.Cond // stop writes if the next write given oui may flood our peers read buffer
	ls  time.Time // last time data was sent to our peer

	wi uint16 // write index
	ri uint16 // read index

	wq []uint32 // write queue
	rq []uint32 // read queue

	wqe []writtenPacket // write queue entries
}

func NewConn(conn net.PacketConn, addr net.Addr, opts ...ConnOption) *Conn {
	c := &Conn{conn: conn, addr: addr, exit: make(chan struct{})}

	for _, opt := range opts {
		opt.applyConn(c)
	}

	if c.writeBufferSize == 0 {
		c.writeBufferSize = DefaultWriteBufferSize
	}

	if c.readBufferSize == 0 {
		c.readBufferSize = DefaultReadBufferSize
	}

	if c.resendTimeout == 0 {
		c.resendTimeout = DefaultResendTimeout
	}

	if c.updatePeriod == 0 {
		c.updatePeriod = DefaultUpdatePeriod
	}

	if c.pool == nil {
		c.pool = new(Pool)
	}

	c.wq = make([]uint32, c.writeBufferSize)
	c.rq = make([]uint32, c.readBufferSize)

	emptyBufferIndices(c.wq)
	emptyBufferIndices(c.rq)

	c.wqe = make([]writtenPacket, c.writeBufferSize)

	c.ouc.L = &c.mu

	return c
}

func (c *Conn) WriteReliablePacket(buf []byte) error {
	return c.writePacket(true, buf)
}

func (c *Conn) WriteUnreliablePacket(buf []byte) error {
	return c.writePacket(false, buf)
}

func (c *Conn) writePacket(reliable bool, buf []byte) error {
	var (
		idx     uint16
		ack     uint16
		ackBits uint32
		ok      = true
	)

	if reliable {
		idx, ack, ackBits, ok = c.waitForNextWriteDetails()
	} else {
		ack, ackBits = c.nextAckDetails()
	}

	if !ok {
		return io.EOF
	}

	c.trackAcked(ack)

	if err := c.write(PacketHeader{Sequence: idx, ACK: ack, ACKBits: ackBits, Unordered: !reliable}, buf); err != nil {
		return err
	}

	//log.Printf("%s: send    (seq=%05d) (ack=%05d) (ack_bits=%032b) (size=%d) (reliable=%t)", c.conn.LocalAddr(), idx, ack, ackBits, len(buf), reliable)

	return nil
}

func (c *Conn) waitUntilReaderAvailable() {
	for !c.die && seq.GT(c.wi+1, c.oui+uint16(len(c.rq))) {
		c.ouc.Wait()
	}
}

func (c *Conn) waitForNextWriteDetails() (idx uint16, ack uint16, ackBits uint32, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.waitUntilReaderAvailable()

	idx, ok = c.nextWriteIndex(), !c.die
	ack, ackBits = c.nextAckDetails()
	return idx, ack, ackBits, ok
}

func (c *Conn) nextWriteIndex() (idx uint16) {
	idx, c.wi = c.wi, c.wi+1
	return idx
}

func (c *Conn) nextAckDetails() (ack uint16, ackBits uint32) {
	ack = c.ri - 1
	ackBits = c.prepareAckBits(ack)
	return ack, ackBits
}

func (c *Conn) prepareAckBits(ack uint16) (ackBits uint32) {
	for i, m := uint16(0), uint32(1); i < ACKBitsetSize; i, m = i+1, m<<1 {
		if c.rq[(ack-i)%uint16(len(c.rq))] != uint32(ack-i) {
			continue
		}

		ackBits |= m
	}
	return ackBits
}

func (c *Conn) write(header PacketHeader, buf []byte) error {
	b := c.pool.Get()

	b.B = header.AppendTo(b.B)
	b.B = append(b.B, buf...)

	if header.Unordered {
		defer c.pool.Put(b)
	}

	if !header.Unordered {
		c.trackWrite(header.Sequence, b)
	}

	if err := c.transmit(b.B); err != nil && !isEOF(err) {
		return fmt.Errorf("failed to transmit packet: %w", err)
	}

	return nil
}

func (c *Conn) trackWrite(idx uint16, buf *Buffer) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if seq.GT(idx+1, c.wi) {
		c.clearWrites(c.wi, idx)
		c.wi = idx + 1
	}

	i := idx % uint16(len(c.wq))
	c.wq[i] = uint32(idx)
	if c.wqe[i].buf != nil {
		c.pool.Put(c.wqe[i].buf)
	}
	c.wqe[i].buf = buf
	c.wqe[i].acked = false
	c.wqe[i].written = time.Now()
	c.wqe[i].resent = 0
}

func (c *Conn) clearWrites(start, end uint16) {
	count, size := end-start+1, uint16(len(c.wq))

	if count >= size {
		emptyBufferIndices(c.wq)
		return
	}

	first := c.wq[start%size:]
	length := uint16(len(first))

	if count <= length {
		emptyBufferIndices(first[:count])
		return
	}

	second := c.wq[:count-length]

	emptyBufferIndices(first)
	emptyBufferIndices(second)
}

func (c *Conn) transmit(buf []byte) error {
	n, err := c.conn.WriteTo(buf, c.addr)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	return err
}

func (c *Conn) Read(header PacketHeader, buf []byte) error {
	c.readAckBits(header.ACK, header.ACKBits)

	if !header.Unordered && !c.trackRead(header.Sequence) {
		return nil
	}

	c.trackUnacked()

	if err := c.writeAcksIfNecessary(); err != nil {
		return fmt.Errorf("failed to write acks when necessary: %w", err)
	}

	if header.Empty {
		return nil
	}

	if c.ph != nil {
		c.ph(c.addr, header.Sequence, buf)
	}

	//log.Printf("%s: recv    (seq=%05d) (ack=%05d) (ack_bits=%032b) (size=%d) (reliable=%t)", c.conn.LocalAddr(), header.Sequence, header.ACK, header.ACKBits, len(buf), !header.Unordered)

	return nil
}

func (c *Conn) createAckIfNecessary() (header PacketHeader, needed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	lui := c.lui

	for i := uint16(0); i < ACKBitsetSize; i++ {
		if c.rq[(lui+i)%uint16(len(c.rq))] != uint32(lui+i) {
			return header, needed
		}
	}

	lui += ACKBitsetSize
	c.lui = lui
	c.ls = time.Now()

	c.waitUntilReaderAvailable()

	header.Sequence, header.ACK = c.nextWriteIndex(), lui-1
	header.ACKBits = c.prepareAckBits(header.ACK)
	header.Empty = true

	needed = !c.die

	return header, needed
}

func (c *Conn) writeAcksIfNecessary() error {
	for {
		header, needed := c.createAckIfNecessary()
		if !needed {
			return nil
		}

		//log.Printf("%s: ack     (seq=%05d) (ack=%05d) (ack_bits=%032b)", c.conn.LocalAddr(), header.Sequence, header.ACK, header.ACKBits)

		if err := c.write(header, nil); err != nil {
			return fmt.Errorf("failed to write ack packet: %w", err)
		}
	}
}

func (c *Conn) readAckBits(ack uint16, ackBits uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for idx := uint16(0); idx < ACKBitsetSize; idx, ackBits = idx+1, ackBits>>1 {
		if ackBits&1 == 0 {
			continue
		}

		i := (ack - idx) % uint16(len(c.wq))
		if c.wq[i] != uint32(ack-idx) || c.wqe[i].acked {
			continue
		}

		if c.wqe[i].buf != nil {
			c.pool.Put(c.wqe[i].buf)
		}

		c.wqe[i].buf = nil
		c.wqe[i].acked = true
	}
}

func (c *Conn) trackRead(idx uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	i := idx % uint16(len(c.rq))

	if c.rq[i] == uint32(idx) { // duplicate packet
		return false
	}

	if seq.GT(idx+1, c.ri) {
		c.clearReads(c.ri, idx)
		c.ri = idx + 1
	}

	c.rq[i] = uint32(idx)

	return true
}

func (c *Conn) clearReads(start, end uint16) {
	count, size := end-start+1, uint16(len(c.rq))

	if count >= size {
		emptyBufferIndices(c.rq)
		return
	}

	first := c.rq[start%size:]
	length := uint16(len(first))

	if count <= length {
		emptyBufferIndices(first[:count])
		return
	}

	second := c.rq[:count-length]

	emptyBufferIndices(first)
	emptyBufferIndices(second)
}

func (c *Conn) trackAcked(ack uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()

	lui := c.lui

	for lui <= ack {
		if c.rq[lui%uint16(len(c.rq))] != uint32(lui) {
			break
		}
		lui++
	}

	c.lui = lui
	c.ls = time.Now()
}

func (c *Conn) trackUnacked() {
	c.mu.Lock()
	defer c.mu.Unlock()

	oui := c.oui

	for {
		i := oui % uint16(len(c.wq))
		if c.wq[i] != uint32(oui) || !c.wqe[i].acked {
			break
		}
		oui++
	}
	c.oui = oui

	c.ouc.Broadcast()
}

func (c *Conn) close() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.die {
		return false
	}
	close(c.exit)
	c.die = true
	c.ouc.Broadcast()

	return true
}

func (c *Conn) Close() {
	if !c.close() {
		return
	}

	//c.mu.Lock()
	//defer c.mu.Unlock()

	//if strings.Contains(c.conn.LocalAddr().String(), "44444") { // sending
	//log.Printf("send closed (oldest_sent_ack_idx=%05d) (oldest_unacked_idx=%05d)", c.lui, c.oui)
	//} else if strings.Contains(c.conn.LocalAddr().String(), "55555") { // receiving
	//log.Printf("recv closed (oldest_sent_ack_idx=%05d) (oldest_unacked_idx=%05d)", c.lui, c.oui)
	//}
}

func (c *Conn) Run() {
	ticker := time.NewTicker(c.updatePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-c.exit:
			return
		case <-ticker.C:
			if err := c.retransmitUnackedPackets(); err != nil && c.eh != nil {
				c.eh(c.addr, err)
			}
		}
	}
}

func (c *Conn) retransmitUnackedPackets() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for idx := uint16(0); idx < uint16(len(c.wq)); idx++ {
		i := (c.oui + idx) % uint16(len(c.wq))
		if c.wq[i] != uint32(c.oui+idx) || !c.wqe[i].shouldResend(time.Now(), c.resendTimeout) {
			continue
		}

		//log.Printf("%s: resend  (seq=%d)", c.conn.LocalAddr(), c.oui+idx)

		if err := c.transmit(c.wqe[i].buf.B); err != nil {
			if isEOF(err) {
				break
			}
			return fmt.Errorf("failed to retransmit unacked packet: %w", err)
		}

		c.wqe[i].written = time.Now()
		c.wqe[i].resent++
	}

	return nil
}
