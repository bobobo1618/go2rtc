package petlibro

import (
	"errors"
	"os"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
)

// petlibro divergence vs pkg/tutk: tutk's worker (pkg/tutk/conn.go:164)
// is a single goroutine that owns the socket: Conn.Read decrypts inline
// and dispatches via handleMsg without any intermediate channel.  Petlibro
// decouples the read syscall from decryption with a reader goroutine
// feeding a 4096-deep channel, so a slow decrypt or GC pause can't
// stall the read syscall and cause kernel UDP drops on high-bitrate
// HD streams.  The cost is one channel hop per packet; the benefit is
// the readerDrops counter making any backlog visible.

// handleEncryptedDatagram decrypts a raw wire packet and dispatches it.
// Tests prefer parseDatagram (plaintext input) directly.
func (c *Client) handleEncryptedDatagram(raw []byte) {
	// petlibro divergence vs pkg/tutk: tutk uses
	// ReverseTransCodePartial in the receive direction (pkg/tutk/conn.go:84)
	// — same primitive, same direction; the only thing different here is
	// that the call lives in a processor goroutine downstream of the
	// read syscall instead of inside the read loop itself.
	c.parseDatagram(tutk.ReverseTransCodePartial(nil, raw))
}

// recvLoop runs two goroutines: a tight reader that does nothing but
// drain the UDP socket into a channel, and a processor that decrypts
// and dispatches.  Decoupling means a slow decrypt or GC pause can't
// stall the read syscall and cause kernel UDP drops.
func (c *Client) recvLoop() {
	defer c.Close()

	rawChan := make(chan []byte, 4096) // ~4 MiB at peak packet sizes
	go c.readerGoroutine(rawChan)

	lastForce := time.Now()
	lastStats := time.Now()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-c.done:
			return
		case raw, ok := <-rawChan:
			if !ok {
				return
			}
			c.stats.bytesIn.Add(uint64(len(raw)))
			c.stats.pktsIn.Add(1)
			c.handleEncryptedDatagram(raw)
		case <-tick.C:
			// fall through to periodic work below
		}

		// Shorter forceDrain interval — the camera (over LAN) has
		// usually retransmitted lost packets within ~50-100 ms or
		// they're not coming at all.  Holding longer just adds
		// rendering latency without recovering more data.
		if time.Since(lastForce) > 100*time.Millisecond {
			c.forceDrain()
			lastForce = time.Now()
		}
		if c.verbose && time.Since(lastStats) > 5*time.Second {
			c.dumpStats()
			lastStats = time.Now()
		}
	}
}

// readerGoroutine does nothing but pull bytes off the wire as fast as
// the kernel will deliver them and hand them to the processor.  Each
// iteration allocates a fresh buffer because the channel may queue
// many at once.
func (c *Client) readerGoroutine(out chan<- []byte) {
	defer close(out)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		buf := make([]byte, 65535)
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				c.stats.recvTimeouts.Add(1)
				continue
			}
			return
		}
		if n == 0 {
			continue
		}
		select {
		case <-c.done:
			return
		case out <- buf[:n]:
		default:
			// Processor is behind by 1024 packets.  Drop the new
			// one rather than block the reader and let the kernel
			// drop instead — the inner cmd counter / frag_idx gap
			// detection will mark the resulting AU as incomplete.
			c.stats.readerDrops.Add(1)
		}
	}
}

// dumpStats prints a one-line stream-health summary every ~5 s when
// the client is in verbose mode.  Numbers cover the most recent
// interval; the cumulative counters are also visible.
func (c *Client) dumpStats() {
	cur := c.stats.snapshot()
	delta := cur
	if c.havePrevStats {
		delta = countersSnapshot{
			bytesIn:      cur.bytesIn - c.prevStats.bytesIn,
			pktsIn:       cur.pktsIn - c.prevStats.pktsIn,
			mainFrags:    cur.mainFrags - c.prevStats.mainFrags,
			subFrags:     cur.subFrags - c.prevStats.subFrags,
			audioFrags:   cur.audioFrags - c.prevStats.audioFrags,
			otherFrags:   cur.otherFrags - c.prevStats.otherFrags,
			vidFrags:     cur.vidFrags - c.prevStats.vidFrags,
			vidFramesIn:  cur.vidFramesIn - c.prevStats.vidFramesIn,
			vidFramesOut: cur.vidFramesOut - c.prevStats.vidFramesOut,
			vidDropped:   cur.vidDropped - c.prevStats.vidDropped,
			fragSkips:    cur.fragSkips - c.prevStats.fragSkips,
			fragsLost:    cur.fragsLost - c.prevStats.fragsLost,
			forceDrains:  cur.forceDrains - c.prevStats.forceDrains,
			recvTimeouts: cur.recvTimeouts - c.prevStats.recvTimeouts,
			readerDrops:  cur.readerDrops - c.prevStats.readerDrops,
			emitDrops:    cur.emitDrops - c.prevStats.emitDrops,
			dualStreamIL: cur.dualStreamIL - c.prevStats.dualStreamIL,
		}
	}
	log.Debug().Msgf("stats: in=%d pkts (%d KiB) channels: main=%d sub=%d audio=%d other=%d | video: %d frames in -> %d out (drop %d) | frag skips: %d (%d frags lost) | forceDrain: %d | qDrops reader=%d emit=%d | dualStreamIL=%d",
		delta.pktsIn, delta.bytesIn/1024,
		delta.mainFrags, delta.subFrags, delta.audioFrags, delta.otherFrags,
		delta.vidFramesIn, delta.vidFramesOut, delta.vidDropped,
		delta.fragSkips, delta.fragsLost, delta.forceDrains,
		delta.readerDrops, delta.emitDrops, delta.dualStreamIL)
	c.prevStats = cur
	c.havePrevStats = true
}

// maintenanceLoop fires heartbeat, alive, and sliding-window ACK
// packets on cadences calibrated to the official Petlibro app's
// behaviour.  Exits on c.done.
func (c *Client) maintenanceLoop() {
	tickBase := time.Now().UnixMilli()
	tick32 := func() uint32 { return uint32((time.Now().UnixMilli() - tickBase + 0xC000) & 0xFFFFFFFF) }
	tick16 := func() uint16 { return uint16(time.Now().UnixMilli() & 0xFFFF) }

	hb := time.NewTicker(1 * time.Second)
	alive := time.NewTicker(1500 * time.Millisecond)
	ack := time.NewTicker(25 * time.Millisecond)
	defer hb.Stop()
	defer alive.Stop()
	defer ack.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-hb.C:
			_ = c.sendInner(innerHeartbeat(c.icounter, tick32()))
			c.icounter++
		case <-alive.C:
			_ = c.send(buildAliveC2D(c.nonce))
		case <-ack.C:
			tw := uint16(c.avHighExt & 0xFFFF)
			if tw != c.avPrevSubWire && c.avHighExt >= 0x4000 {
				_ = c.sendInner(innerAck(c.icounter, c.avPrevSubWire, tw, 3, 0x34, tick16()))
				c.icounter++
				c.avPrevSubWire = tw
			}
		}
	}
}
