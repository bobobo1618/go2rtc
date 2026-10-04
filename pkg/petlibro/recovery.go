package petlibro

import "time"

type recoveryState struct {
	next        uint64
	high        uint64
	seen        map[uint64]bool
	missing     map[uint64]time.Time
	playMissing map[uint64]time.Time
}
type v2Counters struct {
	fecParity, fecValid, fecInvalid, fecRecovered                                             uint64
	extended                                                                                  uint64
	gaps, recovered, late, timeouts, giveups, waived, idrGaps, idrRecovered, idrDrops, pDrops uint64
	maxDepth                                                                                  uint64
	maxAge                                                                                    time.Duration
	interpolated                                                                              uint64
}

func (c *Client) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
func (c *Client) recoveryInit() {
	if c.recovery.seen != nil {
		return
	}
	c.recovery = recoveryState{next: c.avNextExt, high: c.avNextExt - 1, seen: make(map[uint64]bool), missing: make(map[uint64]time.Time), playMissing: make(map[uint64]time.Time)}
	if c.playGrace == 0 {
		c.playGrace = 120 * time.Millisecond
	}
	if c.idrGrace == 0 {
		c.idrGrace = 350 * time.Millisecond
	}
	if c.ackGrace == 0 {
		c.ackGrace = 1000 * time.Millisecond
	}
	c.ackPublished.Store(c.recovery.next - 1)
}

// receiveSequence runs before the playout late-packet check. A packet that
// can no longer repair playback still closes a transport ACK hole.
func (c *Client) receiveSequence(seq uint64, now time.Time) {
	c.recoveryInit()
	r := &c.recovery
	delete(r.playMissing, seq)
	if seq < r.next {
		return
	}
	if since, ok := r.missing[seq]; ok {
		age := now.Sub(since)
		if age > c.v2.maxAge {
			c.v2.maxAge = age
		}
		delete(r.missing, seq)
		if seq < c.avNextExt {
			c.v2.late++
		} else {
			c.v2.recovered++
			if c.mainAsm.framePending {
				c.v2.idrRecovered++
			}
		}
	}
	if seq > r.high {
		for n := r.high + 1; n < seq; n++ {
			r.missing[n] = now
			r.playMissing[n] = now
			c.v2.gaps++
		}
		r.high = seq
	}
	r.seen[seq] = true
	c.advanceAck(now, false)
	if seq > c.avNextExt && seq-c.avNextExt > c.v2.maxDepth {
		c.v2.maxDepth = seq - c.avNextExt
	}
}
func (c *Client) advanceAck(now time.Time, allowGiveup bool) {
	r := &c.recovery
	if r.seen == nil {
		return
	}
	gaveUp := false
	for r.next <= r.high {
		if r.seen[r.next] {
			delete(r.seen, r.next)
			r.next++
			continue
		}
		since, ok := r.missing[r.next]
		if !allowGiveup || !ok || now.Sub(since) < c.ackGrace {
			break
		}
		// Explicit bounded waiver, never coupled to a playout skip.
		delete(r.missing, r.next)
		r.next++
		c.v2.waived++
		gaveUp = true
	}
	if gaveUp {
		c.v2.giveups++
	}
	c.ackPublished.Store(r.next - 1)
	c.ackPending.Store(len(r.missing) > 0)
}
func (c *Client) gapGrace() time.Duration {
	if c.mainAsm.framePending {
		return c.idrGrace
	}
	// A lost head of an IDR can be inferred from the first queued main fragment.
	for _, p := range c.avBuffer {
		if p.channel == innerChMain {
			return c.idrGrace
		}
	}
	return c.playGrace
}
func (c *Client) forceDrain() {
	c.recoveryInit()
	now := c.timeNow()
	c.advanceAck(now, true)
	c.drainContiguous()
	if len(c.avBuffer) == 0 {
		return
	}
	since, ok := c.recovery.playMissing[c.avNextExt]
	if !ok {
		return
	}
	age := now.Sub(since)
	if age > c.v2.maxAge {
		c.v2.maxAge = age
	}
	if age < c.gapGrace() {
		return
	}
	next := c.avHighExt + 1
	for seq := range c.avBuffer {
		if seq < next {
			next = seq
		}
	}
	if next <= c.avNextExt {
		return
	}
	c.v2.timeouts++
	if c.gapGrace() == c.idrGrace {
		c.v2.idrGaps++
	}
	c.stats.forceDrains.Add(1)
	// Only this gap is skipped. drainContiguous stops at the next hole.
	for seq := c.avNextExt; seq < next; seq++ {
		delete(c.recovery.playMissing, seq)
	}
	c.avNextExt = next
	c.drainContiguous()
}
func (c *Client) dumpV2() {
	s, p := c.v2, c.prevV2
	log.Debug().Msgf("fec: parity=%d validated=%d invalid=%d recovered=%d", s.fecParity-p.fecParity, s.fecValid-p.fecValid, s.fecInvalid-p.fecInvalid, s.fecRecovered-p.fecRecovered)
	log.Debug().Msgf("transport: extendedHeaders=%d", s.extended-p.extended)
	log.Debug().Msgf("recovery: ack=%d playback=%d gaps=%d recovered=%d lateAfterPlayout=%d gapTimeout=%d ackGiveups=%d waivedPackets=%d IDRgap=%d IDRrecovered=%d IDRdrop=%d Pdrop=%d maxDepth=%d maxAgeMs=%.1f interpolation=%d nominalMs=%.3f", c.ackPublished.Load(), c.avNextExt, s.gaps-p.gaps, s.recovered-p.recovered, s.late-p.late, s.timeouts-p.timeouts, s.giveups-p.giveups, s.waived-p.waived, s.idrGaps-p.idrGaps, s.idrRecovered-p.idrRecovered, s.idrDrops-p.idrDrops, s.pDrops-p.pDrops, s.maxDepth, float64(s.maxAge.Microseconds())/1000, s.interpolated-p.interpolated, float64(c.clock.interval())/90)
	c.prevV2 = s
	c.v2.maxDepth = 0
	c.v2.maxAge = 0
}
