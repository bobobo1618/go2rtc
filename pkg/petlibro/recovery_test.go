package petlibro

import (
	"testing"
	"time"
)

func testP(seq uint16, frame, ts uint32) []byte {
	p := makeTrailerPayload([]byte{0, 0, 0, 1, 0x41, 0xaa}, false, 1, ts)
	return makePkt(fragSpec{channel: innerChSub, subWire: seq, frameNum: frame, totalFrags: 1, paylen: uint16(len(p)), payload: p})
}
func timedClient() (*Client, *time.Time) {
	c := newTestClient(0x4000, "hd")
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	return c, &now
}
func TestReorderGapRecoveredInOrder(t *testing.T) {
	c, n := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	c.parseDatagram(testP(0x4002, 3, 180))
	*n = n.Add(90 * time.Millisecond)
	c.forceDrain()
	if len(c.frames) != 1 {
		t.Fatal("premature drain")
	}
	c.parseDatagram(testP(0x4001, 2, 140))
	p := drainAll(c)
	if len(p) != 3 || p[1].Timestamp != 3600 || p[2].Timestamp != 7200 {
		t.Fatalf("order: %+v", p)
	}
	if c.v2.recovered != 1 || c.stats.vidDropped.Load() != 0 {
		t.Fatal("recovery/drop")
	}
	if ack, _ := c.contiguousAckExt(); ack != 0x4002 {
		t.Fatal(ack)
	}
}
func TestPlaybackTimeoutDoesNotAcknowledgeLoss(t *testing.T) {
	c, n := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	c.parseDatagram(testP(0x4002, 3, 180))
	*n = n.Add(150 * time.Millisecond)
	c.forceDrain()
	if c.avNextExt != 0x4003 {
		t.Fatal("playout stalled")
	}
	if ack, _ := c.contiguousAckExt(); ack != 0x4000 {
		t.Fatalf("false ACK %x", ack)
	}
	c.parseDatagram(testP(0x4001, 2, 140))
	if ack, _ := c.contiguousAckExt(); ack != 0x4002 {
		t.Fatal("late arrival did not repair ACK")
	}
	if c.v2.late != 1 || len(c.frames) != 2 {
		t.Fatal("late packet emitted or not counted")
	}
}
func TestBoundedAckGiveup(t *testing.T) {
	c, n := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	c.parseDatagram(testP(0x4002, 3, 180))
	*n = n.Add(500 * time.Millisecond)
	c.forceDrain()
	if c.v2.giveups != 0 {
		t.Fatal("early giveup")
	}
	*n = n.Add(600 * time.Millisecond)
	c.forceDrain()
	if ack, _ := c.contiguousAckExt(); ack != 0x4002 {
		t.Fatal("ACK stuck")
	}
	if c.v2.giveups != 1 || c.v2.waived != 1 {
		t.Fatal("giveup not explicit")
	}
}
func TestIDRLongerGraceAndDeferredP(t *testing.T) {
	c, n := timedClient()
	data := []byte{0, 0, 0, 1, 0x65, 0xaa}
	head := makePkt(fragSpec{channel: innerChMain, subWire: 0x4000, frameNum: 1, totalFrags: 2, paylen: uint16(len(data)), payload: data})
	tail := makeTrailerPayload([]byte{0xbb}, true, 1, 100)
	end := makePkt(fragSpec{channel: innerChMain, b1: 1, sub17: 1, subWire: 0x4002, frameNum: 1, totalFrags: 2, fragIdx: 1, paylen: uint16(len(tail)), payload: tail})
	c.parseDatagram(head)
	c.parseDatagram(testP(0x4001, 2, 140))
	c.parseDatagram(testP(0x4003, 3, 180))
	*n = n.Add(200 * time.Millisecond)
	c.forceDrain()
	if len(c.frames) != 0 || c.avNextExt != 0x4002 {
		t.Fatal("IDR grace lost")
	}
	c.parseDatagram(end)
	p := drainAll(c)
	if len(p) != 3 || !p[0].IsKeyframe || len(p[0].Payload) != 7 {
		t.Fatal("IDR/deferred P not recovered")
	}
}
func TestMissingPOnlyDropsAffectedAU(t *testing.T) {
	c, n := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	tail := makeTrailerPayload([]byte{0xbb, 0xcc, 0xdd, 0xee, 0xff}, false, 1, 140)
	c.parseDatagram(makePkt(fragSpec{channel: innerChSub, b1: 1, sub17: 1, subWire: 0x4002, frameNum: 2, totalFrags: 2, fragIdx: 1, paylen: uint16(len(tail)), payload: tail}))
	c.parseDatagram(testP(0x4003, 3, 180))
	*n = n.Add(150 * time.Millisecond)
	c.forceDrain()
	if len(c.frames) != 2 || c.stats.vidDropped.Load() != 1 {
		t.Fatalf("frames=%d drops=%d", len(c.frames), c.stats.vidDropped.Load())
	}
}
func TestDrainOnlyOneExpiredGap(t *testing.T) {
	c, n := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	c.parseDatagram(testP(0x4002, 3, 180))
	*n = n.Add(100 * time.Millisecond)
	c.parseDatagram(testP(0x4004, 5, 260))
	*n = n.Add(50 * time.Millisecond)
	c.forceDrain()
	if c.avNextExt != 0x4003 || len(c.frames) != 2 {
		t.Fatal("flushed younger gap")
	}
	*n = n.Add(100 * time.Millisecond)
	c.forceDrain()
	if c.avNextExt != 0x4005 {
		t.Fatal("next gap never drained")
	}
}
func TestNominalInterpolationMonotonicBurst(t *testing.T) {
	var v videoClock
	var last uint32
	for i := 0; i < 100; i++ {
		valid := i%7 != 6
		p, _ := v.timestamp(uint32(1000+i*50), valid)
		if i > 0 && p-last != 4500 {
			t.Fatalf("i=%d delta=%d", i, p-last)
		}
		last = p
	}
	if v.interval() != 4500 {
		t.Fatal("did not learn 20fps")
	}
	for i := 0; i < 10; i++ {
		p, _ := v.timestamp(5950, true)
		if p-last != 4500 {
			t.Fatal("duplicate camera timestamp caused tiny delta")
		}
		last = p
	}
}
func TestClockCameraAndRTPWrap(t *testing.T) {
	var v videoClock
	v.timestamp(0xfffffff0, true)
	p, _ := v.timestamp(34, true)
	if p != 4500 {
		t.Fatal(p)
	}
	v.last = 0xfffffff0
	v.anchor = v.last
	p, _ = v.timestamp(84, true)
	if p != 4484 {
		t.Fatal("RTP rollover", p)
	}
}
func TestIncompleteIDRNeverEmitted(t *testing.T) {
	c, _ := timedClient()
	c.mainAsm.buf = []byte{0, 0, 0, 1, 0x65, 0xaa}
	c.mainAsm.framePending = true
	c.flushMainIDR(100)
	if len(c.frames) != 0 || !c.gopPoisoned || c.v2.idrDrops != 1 {
		t.Fatal("partial IDR escaped")
	}
}
func TestReceiveFrontierDoesNotUsePlaybackMutation(t *testing.T) {
	c, _ := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	c.avNextExt = 0x4100
	c.avHighExt = 0x4200
	if a, _ := c.contiguousAckExt(); a != 0x4000 {
		t.Fatalf("ACK coupled to playback: %x", a)
	}
}

// Live 3.1.54 headers: 0c080b00 ... AV header at 24; 0c0d0b00
// carries an end fragment at the same extended offset. Previously discarded.
func TestExtendedTransportHeaders(t *testing.T) {
	for _, kind := range []byte{0x08, 0x09, 0x0d} {
		t.Run(string(rune(kind+'A')), func(t *testing.T) {
			c, _ := timedClient()
			normal := testP(0x4000, 1, 100)
			normal[0x1c+1] = kind
			ext := append([]byte(nil), normal[:0x1c+16]...)
			ext = append(ext, []byte{1, 2, 3, 4, 5, 6, 7, 8}...)
			ext = append(ext, normal[0x1c+16:]...)
			if kind == 0x09 {
				ext[0x1c+25] = 1
			}
			c.parseDatagram(ext)
			if len(c.frames) != 1 {
				t.Fatalf("kind=%x lost extended AU", kind)
			}
			if a, _ := c.contiguousAckExt(); a != 0x4000 {
				t.Fatal("wire sequence was read at wrong offset")
			}
		})
	}
}

func TestAckGiveupCannotErasePlaybackGapTimers(t *testing.T) {
	c, n := timedClient()
	c.parseDatagram(testP(0x4000, 1, 100))
	c.parseDatagram(testP(0x4002, 3, 180))
	c.parseDatagram(testP(0x4004, 5, 260))
	*n = n.Add(2 * time.Second)
	c.forceDrain()
	c.forceDrain()
	if c.avNextExt != 0x4005 || len(c.frames) != 3 {
		t.Fatal("expired ACK cleanup stranded playout")
	}
}

func TestAudioVariantsPreserved(t *testing.T) {
	adts := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x7f, 0xfc, 1, 2, 3, 4}
	for _, legacy := range []bool{false, true} {
		c, _ := timedClient()
		payload := adts
		ch, b1, sub := byte(innerChAudio), byte(1), byte(1)
		if legacy {
			payload = append(make([]byte, 8), adts...)
			ch, b1, sub = innerChSub, 0x0d, 0
		}
		c.parseDatagram(makePkt(fragSpec{channel: ch, b1: b1, sub17: sub, subWire: 0x4000, paylen: uint16(len(payload)), payload: payload}))
		p := drainAll(c)
		if len(p) != 1 || p[0].Codec != CodecAACADTS || string(p[0].Payload) != string(adts) {
			t.Fatalf("audio legacy=%v lost", legacy)
		}
	}
}
