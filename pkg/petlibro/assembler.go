package petlibro

import (
	"encoding/binary"
	"slices"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/h264"
)

// petlibro divergence vs pkg/tutk: tutk's session16 reassembler
// (pkg/tutk/session16.go:181 — Session16.SessionRead 0x01 0x03 branch)
// expects the camera to send a single header (chunkSeq==0) carrying the
// total payloadSize, then subsequent chunks concatenated in fixed
// chunkSeq order; gap = drop the whole frame (msgMediaLost).  Petlibro
// firmware instead labels each fragment with totalFrags (inner[20]),
// fragIdx (inner[22..23]), and an end-marker fragment whose tail
// matches the 16-byte stripFragmentMetadataTrailer signature, with two
// independent channels (innerChMain 0x05 for IDR / innerChSub 0x07 for
// P-frames) time-multiplexed in wire order.  The two-channel + trailer-
// based reassembler in this file has no tutk analog.

const (
	forceDrainStallTicks = 5 // ~500 ms with the 100 ms forceDrain cadence
	maxDeferredVideoAUs  = 64
)

type deferredVideoAU struct {
	payload []byte
	ts      uint32
	haveTs  bool
}

type pendingFrag struct {
	channel byte
	// isAudio: true for either AAC variant — ch=0x03 sub17=0x01 ADTS
	// at offset 36, or ch=0x07 sub17=0x00 b1=0x0d with an 8-byte
	// inner header before ADTS sync.
	isAudio    bool
	subExt     uint64
	frameNum   uint32 // camera's per-channel frame counter (inner[28..31])
	fragIdx    uint16 // index of fragment within frame (inner[22..23])
	totalFrags uint16 // total fragments comprising this frame (inner[20]); widened from byte so >255-fragment IDRs at high bitrates don't silently truncate
	payload    []byte
}

// channelAsm holds the assembly state for a single video channel.  The
// camera interleaves IDR fragments on ch=0x05 with P-frame fragments
// on ch=0x07 in wire order, so they MUST be assembled independently —
// a single shared buffer would constantly drop partial frames whenever
// the channels switch.
//
// framePending is an explicit "an AU is in progress on this channel"
// flag.  Earlier versions overloaded `curFrameNum == 0` as the
// sentinel, which silently treated a legitimate frameNum=0 (camera
// reboot or wrap-around to zero) as "no AU in progress" and merged it
// into whatever the next AU was — a frankenframe.  Splitting the
// concerns avoids that.
type channelAsm struct {
	buf            []byte
	curFrameNum    uint32
	framePending   bool
	curAUTotal     uint16 // inner[20]: total fragments advertised for current frame
	curAUDataCount uint16
	curAUGapped    bool
	expectFragIdx  uint16
}

func (a *channelAsm) reset() {
	a.buf = a.buf[:0]
	a.curAUTotal = 0
	a.curAUDataCount = 0
	a.curAUGapped = false
	a.expectFragIdx = 0
	a.framePending = false
}

// parseDatagram handles one already-decrypted packet.  Splitting the
// crypto pass from the parser lets tests feed plaintext fixtures
// straight in without re-encrypting; assembler_test.go relies on it.
func (c *Client) parseDatagram(pkt []byte) {
	if len(pkt) < 0x1C+36 || pkt[3] != flagsRecv {
		return
	}
	if binary.LittleEndian.Uint16(pkt[8:]) != msgSessionD2C {
		return
	}
	inner := pkt[0x1C:]
	if inner[0] != 0x0c {
		return
	}
	b1 := inner[1]
	channel := inner[16] // low byte of bytes16..17
	sub17 := inner[17]
	subWire := binary.LittleEndian.Uint16(inner[18:])
	switch channel {
	case innerChMain:
		c.stats.mainFrags.Add(1)
	case innerChSub:
		c.stats.subFrags.Add(1)
	case innerChAudio:
		c.stats.audioFrags.Add(1)
	default:
		c.stats.otherFrags.Add(1)
	}
	// paylen at bytes 24..25 covers the actual payload bytes after offset
	// 36; the camera often pads packets with trailing zeros, and feeding
	// those into the stream synthesises spurious H.264 start codes that
	// the decoder rejects.  Only use paylen when it's non-zero AND fits.
	paylen := binary.LittleEndian.Uint16(inner[24:])
	sliceWithPaylen := func(start int) ([]byte, bool) {
		if paylen != 0 && start+int(paylen) <= len(inner) {
			return inner[start : start+int(paylen)], true
		}
		if paylen == 0 {
			return inner[start:], true
		}
		return nil, false
	}

	var (
		isAudio bool
		payload []byte
	)
	switch {
	case (channel == innerChMain || channel == innerChSub) &&
		(b1 == 0x00 || b1 == 0x04 || b1 == 0x05):
		// Data fragment.  paylen MUST be > 0 — a 0-byte data
		// fragment indicates wire corruption or a malformed datagram
		// and concatenating it into the assembler would feed zero
		// bytes (then synthesise spurious H.264 start codes) into
		// the decoder.  Loud-drop.
		if paylen == 0 {
			return
		}
		var ok bool
		payload, ok = sliceWithPaylen(36)
		if !ok {
			return
		}
	case (channel == innerChMain || channel == innerChSub) &&
		b1 == 0x01 && sub17 == 0x01:
		// END-FRAGMENT of a multi-fragment AV frame.
		//   * On ch=0x05: closes a multi-fragment IDR.  Verified on
		//     PLAF203 firmware — a 78-fragment HD IDR is 77 data
		//     fragments (b1=0x00/0x04, paylen=1024) followed by ONE
		//     b1=0x01 sub17=0x01 fragment (paylen ~288, ends with
		//     the trailer signature 4e 00 01 00 <stream_id> 00*7
		//     <ts>).  That last fragment carries the bottom MB rows
		//     plus rbsp_trailing_bits — without it the decoder
		//     errors on rows 66-67 of every IDR.
		//   * On ch=0x07: closes a 2-fragment P-frame (tf=2, with
		//     a b1=0x00 data fragment of paylen=1024 + this b1=0x01
		//     end fragment).  Without it, multi-fragment P-frames
		//     never emit AND their trailer-borne ms timestamp is
		//     lost — forcing subsequent frames onto the
		//     lastEmitTs+1 fallback and producing tiny PTS deltas
		//     that confuse ffmpeg's reorder buffer.
		if paylen == 0 {
			return
		}
		var ok bool
		payload, ok = sliceWithPaylen(36)
		if !ok {
			return
		}
	case channel == innerChAudio && sub17 == 0x01 &&
		len(inner) >= 38 && inner[36] == 0xFF && inner[37] == 0xF1:
		// Audio variant (b): ADTS at offset 36, channel 0x03.
		if paylen == 0 {
			return
		}
		isAudio = true
		var ok bool
		payload, ok = sliceWithPaylen(36)
		if !ok {
			return
		}
	case channel == innerChSub && sub17 == 0x00 && b1 == 0x0d &&
		len(inner) >= 46 && inner[44] == 0xFF && inner[45] == 0xF1:
		// Audio variant (a): 8-byte inner header before ADTS sync,
		// carried on the sub_video channel.  Strip the 8 header
		// bytes so the producer sees clean ADTS.
		if paylen == 0 {
			return
		}
		isAudio = true
		full, ok := sliceWithPaylen(36)
		if !ok {
			return
		}
		if len(full) > 8 {
			payload = full[8:]
		} else {
			payload = nil
		}
	default:
		return
	}

	// Drop empty audio payloads — downstream RTP packetisers don't
	// tolerate zero-byte AUs (some panic, others emit malformed
	// packets) and a legitimately empty AAC frame is never produced
	// by this camera firmware.
	if isAudio && len(payload) == 0 {
		return
	}

	// No filter by subWire — audio fragments legitimately arrive at
	// wire values < 0x4000 alongside the high AV indices, and dropping
	// those would silence the audio stream.
	subExt, ok := c.wrap.extend(subWire)
	if !ok {
		// Wire counter landed before the sequence start — most likely
		// wire corruption.  Loud-drop instead of saturating to zero
		// (which would have hashed into avBuffer at a slot
		// drainContiguous can never reach since avNextExt starts at
		// >=0x4000).
		c.stats.otherFrags.Add(1)
		return
	}
	if subExt > c.avHighExt {
		c.avHighExt = subExt
		c.wrap.advanceTo(subWire)
	}
	if subExt < c.avNextExt {
		return // late dup
	}
	// len(inner) >= 36 was checked at the top of handleIncoming, so
	// all of these offsets are in-bounds — earlier per-offset length
	// guards were dead code.
	frameNum := binary.LittleEndian.Uint32(inner[28:])
	fragIdx := binary.LittleEndian.Uint16(inner[22:])
	totalFrags := uint16(inner[20]) // camera's reported total fragment count for this frame
	c.avBuffer[subExt] = &pendingFrag{
		channel: channel, isAudio: isAudio,
		subExt: subExt, frameNum: frameNum, fragIdx: fragIdx,
		totalFrags: totalFrags, payload: payload,
	}
	c.drainContiguous()
}

func (c *Client) drainContiguous() {
	for {
		e, ok := c.avBuffer[c.avNextExt]
		if !ok {
			return
		}
		delete(c.avBuffer, c.avNextExt)
		c.avNextExt++
		c.emit(e)
	}
}

// forceDrain flushes any avBuffer entries that have fallen too far
// behind the current high-water mark — the missing packet that would
// have filled the contiguous slot is presumed lost.  If avHighExt
// hasn't moved across forceDrainStallTicks consecutive ticks AND the
// buffer is non-empty, we flush EVERYTHING regardless of the
// high-water threshold — recovers from a stalled camera leaving
// partial AUs stranded forever.
func (c *Client) forceDrain() {
	if len(c.avBuffer) == 0 {
		c.stallTicks = 0
		c.lastHighExt = c.avHighExt
		return
	}
	if c.avHighExt == c.lastHighExt {
		c.stallTicks++
	} else {
		c.stallTicks = 0
		c.lastHighExt = c.avHighExt
	}
	stalled := c.stallTicks >= forceDrainStallTicks

	threshold := c.avHighExt
	if !stalled {
		if threshold < 8 {
			return
		}
		threshold -= 8
	}

	var keys []uint64
	for k := range c.avBuffer {
		if stalled || k <= threshold {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	c.stats.forceDrains.Add(1)
	slices.Sort(keys)
	for _, k := range keys {
		e := c.avBuffer[k]
		delete(c.avBuffer, k)
		c.avNextExt = k + 1
		c.emit(e)
	}
	if stalled {
		c.stallTicks = 0
	}
}

// emit handles one in-order, fully-classified fragment.  Video frame
// structure on the wire:
//
//   - 0..N-1 "data" fragments with b1 in {0x00, 0x04, 0x05}.  Slice
//     bytes; paylen=1024 except possibly the last one; no trailer.
//   - 1 final "end" fragment with b1=0x01 sub17=0x01, paylen smaller
//     than 1024, ending in a 16-byte metadata trailer
//     (codec_id 0x4e + 4-byte variant prefix + 7 zero bytes + 4-byte
//     ms-ts).  The trailer's stream-id byte tells us whether this
//     frame is HD (0x01) or SD (0x02).
//
// The end fragment is identified by the trailer signature on its
// payload tail, NOT by fragIdx (which can wrap when N>16 and reuse
// "16" both as a data-fragment index and the end marker).  inner[20]
// gives the camera's reported total fragment count, used as a
// secondary sanity check for end-fragment detection and as the basis
// for the "all data fragments lost" hard-floor in the end-fragment
// path.
//
// CRITICAL: the camera time-multiplexes IDR fragments on ch=0x05 with
// P-frame fragments on ch=0x07 in WIRE ORDER — e.g. mid-IDR a P-frame
// can arrive.  Each channel MUST have its own assembly buffer or the
// constant frame_num switching would drop every partial IDR.  Both
// channels' completed frames are emitted into one PTS-ordered output
// stream, since they share the camera's frame_num counter.
func (c *Client) emit(e *pendingFrag) {
	if c.startedAt.IsZero() {
		c.startedAt = time.Now()
	}
	if e.isAudio {
		pts := uint32(float64(c.emitAudioSeq) * 1024 * 90000 / 44100)
		c.queuePacket(&Packet{
			Codec:     CodecAACADTS,
			Payload:   e.payload,
			FrameNo:   c.emitAudioSeq,
			Timestamp: pts,
		})
		c.emitAudioSeq++
		return
	}
	if e.channel != innerChMain && e.channel != innerChSub {
		return
	}
	c.stats.vidFrags.Add(1)

	// Stream selection.  The camera can be configured (via its
	// Petlibro cloud settings; sticky per-camera) to send either HD
	// only, SD only, or both streams in parallel.  When both are
	// active, IDR fragments from each stream share ch=0x05 and
	// P-frame fragments share ch=0x07, with the only reliable
	// discriminator being the trailer stream-id byte on each frame's
	// end-fragment (p[-12]: 0x01 = HD main, 0x02 = SD sub).  Data
	// fragments (b1=0x00/0x04/0x05) carry no per-fragment stream-id,
	// so we accumulate them optimistically and discard the buffer
	// later if the end-fragment reveals the wrong stream.
	wantStreamID := byte(0x01) // HD by default
	if c.quality == "sd" {
		wantStreamID = 0x02
	}

	// ch=0x07 single-fragment P-frames with a trailer can be
	// discriminated on arrival — drop wrong-stream ones immediately.
	// We deliberately DON'T also flush the pending ch=0x05 IDR here
	// (an earlier version did, and that truncated the IDR at its
	// last fragments when a wrong-stream P-frame arrived between
	// HD IDR fragments — visible in mpv as "corrupted macroblock
	// X 66 / X 67" errors on every frame).  Multi-fragment P-frames
	// (b1=0x00 data + b1=0x01 sub17=0x01 end) reach the in-emit()
	// end-fragment path below instead and are filtered there.
	if e.channel == innerChSub && len(e.payload) >= 16 &&
		e.payload[len(e.payload)-16] == CodecH264 &&
		e.payload[len(e.payload)-12] != wantStreamID {
		return
	}

	stripped, frameTs, trailerStreamID, hasTrailer := stripFragmentMetadataTrailer(e.payload)

	asm := &c.mainAsm
	if e.channel == innerChSub {
		asm = &c.subAsm
	}

	// Frame_num change on THIS channel without having seen the
	// previous frame's end-fragment.  For ch=0x05 this is normally a
	// back-to-back IDR where the previous IDR's end-fragment was lost
	// AND the cross-channel ch=0x07 flush below didn't fire — try to
	// emit the partial previous IDR via flushMainIDR rather than
	// silently drop it (it might still be decodable with localised
	// artefacts).  For ch=0x07 it means a P-frame was abandoned mid-
	// assembly; just reset and count it as a drop.
	if asm.framePending && e.frameNum != asm.curFrameNum && len(asm.buf) > 0 {
		if e.channel == innerChMain {
			c.flushMainIDR(0)
		} else {
			c.stats.fragSkips.Add(1)
			c.stats.fragsLost.Add(1)
			c.stats.vidDropped.Add(1)
			asm.reset()
		}
	}
	if !asm.framePending || e.frameNum != asm.curFrameNum {
		asm.curFrameNum = e.frameNum
		asm.framePending = true
		c.stats.vidFramesIn.Add(1)
		asm.expectFragIdx = 0
		asm.curAUGapped = false
		asm.curAUDataCount = 0
		asm.curAUTotal = e.totalFrags
	}
	if asm.curAUTotal == 0 && e.totalFrags != 0 {
		asm.curAUTotal = e.totalFrags
	}

	if !hasTrailer {
		// Data fragment.  fragIdx==16 is exempt from gap-detection
		// because the camera reuses it as a position index when N>16.
		if e.fragIdx != asm.expectFragIdx && e.fragIdx != 16 {
			c.stats.fragSkips.Add(1)
			if e.fragIdx > asm.expectFragIdx {
				c.stats.fragsLost.Add(uint64(e.fragIdx - asm.expectFragIdx))
			} else {
				c.stats.fragsLost.Add(1)
			}
			asm.curAUGapped = true
		}
		asm.expectFragIdx = e.fragIdx + 1
		asm.curAUDataCount++
		asm.buf = append(asm.buf, e.payload...)
		return
	}

	// End-of-frame fragment.  First, if the trailer's stream-id tells
	// us this frame is from the WRONG stream (camera dual-streaming
	// HD+SD on the same channel and we want one but the end-fragment
	// is the other's), discard the accumulated data fragments — they
	// belonged to the wrong-stream frame.  This pairs with the
	// removal of the per-fragment totalFrags filter: data fragments
	// (b1=0x00) carry no stream-id, so we accumulate them
	// optimistically; the trailer-bearing end-fragment is where we
	// learn the real stream identity and can correct course.
	//
	// Assumption (decision #6): the camera serialises its streams —
	// sends all of HD's fragments contiguously, then all of SD's —
	// matching every dual-stream-mode capture we have.  If a future
	// firmware revision interleaves them within a single AU, the
	// dualStreamIL counter below climbs and we need per-frame_num
	// buffering instead.  The log+drop branch is the defensive
	// fallback per the round-1 review.
	if trailerStreamID != 0 && trailerStreamID != wantStreamID {
		c.stats.dualStreamIL.Add(1)
		log.Trace().Msgf("dual-stream interleave: ch=0x%02x frame=%d got stream-id=0x%02x want=0x%02x — dropping AU", e.channel, e.frameNum, trailerStreamID, wantStreamID)
		c.stats.vidDropped.Add(1)
		asm.reset()
		if e.channel == innerChMain {
			c.dropDeferredVideo()
		}
		return
	}

	asm.buf = append(asm.buf, stripped...)
	expectedData := uint16(0)
	if asm.curAUTotal > 0 {
		expectedData = asm.curAUTotal - 1
	}
	if asm.curAUDataCount < expectedData {
		lost := uint64(expectedData - asm.curAUDataCount)
		c.stats.fragSkips.Add(1)
		c.stats.fragsLost.Add(lost)
		asm.curAUGapped = true
	}

	// Hard floor: if we got the end-fragment but ZERO data fragments
	// for a multi-fragment frame, the AU has no slice header — only
	// trailing bits.  The decoder can't make sense of that and
	// produces a cascade of "out of range intra chroma pred mode" /
	// "mb_type X in I slice too large" / "top block unavailable"
	// errors that contaminate playback well past the next IDR.  Drop
	// it instead of emitting garbage.
	if expectedData >= 1 && asm.curAUDataCount == 0 {
		c.stats.fragSkips.Add(1)
		c.stats.fragsLost.Add(uint64(expectedData))
		c.stats.vidDropped.Add(1)
		asm.reset()
		if c.strict && e.channel == innerChMain {
			c.gopPoisoned = true
		}
		if e.channel == innerChMain {
			c.dropDeferredVideo()
		}
		return
	}

	// Deep-copy boundary: asm.buf is reused for the next AU as
	// fragments arrive, so the slice we hand to emitAU / queuePacket
	// must own its bytes.  Packet.Payload therefore has no shared
	// backing storage with the assembler's working buffer and
	// consumers may retain it past the next ReadPacket() call.
	au := append([]byte(nil), asm.buf...)
	gapped := asm.curAUGapped
	wasMain := e.channel == innerChMain
	asm.reset()

	if gapped {
		// Strict mode: drop the entire GOP for pristine pixels.
		if c.strict {
			c.gopPoisoned = true
			c.stats.vidDropped.Add(1)
			if e.channel == innerChMain {
				c.dropDeferredVideo()
			}
			return
		}
		// Non-strict mode (default), per channel:
		//   * IDR on ch=0x05: emit the partial frame.  A slice with a
		//     mid-frame hole still gives the decoder SOMETHING to
		//     reference for the next ~25-50 P-frames, and macroblock
		//     artefacts at the hole location are far less disruptive
		//     than the multi-second freeze that results from dropping
		//     the IDR and waiting for the next clean one.
		//   * P-frame on ch=0x07: DROP.  A truncated P-frame slice
		//     causes the decoder to mis-parse the bitstream mid-way
		//     and the resulting errors ("mb_type 533 in I slice too
		//     large", "P sub_mb_type 11 out of range", etc) cascade
		//     into every subsequent P-frame in the GOP via reference
		//     prediction.  The decoder recovers automatically at the
		//     next clean P-frame (P-frames only reference the IDR,
		//     not each other for slice-data validity), so dropping
		//     ONE gapped P-frame loses one frame; emitting it loses
		//     the rest of the GOP to cascading decoder errors.
		c.stats.vidDropped.Add(1)
		if e.channel != innerChMain {
			return
		}
	}
	c.pendingFrameTs = frameTs
	c.havePendingTs = true
	// In strict mode only, drop P-frames in a poisoned GOP until the
	// next clean IDR.  In non-strict (default), let them through.
	if c.strict && c.gopPoisoned && !wasMain {
		c.stats.vidDropped.Add(1)
		return
	}
	c.emitAU(au)
}

// flushMainIDR emits the accumulated ch=0x05 IDR buffer in the
// fallback case where we never received its b1=0x01 sub17=0x01
// end-fragment — either it was lost on the wire, or this camera
// firmware variant doesn't send one and we noticed an unrelated
// cross-channel signal (a ch=0x07 P-frame arrival, or a new ch=0x05
// frame_num) telling us the IDR is "as complete as it'll get".  The
// missing end-fragment carries the slice's bottom MB rows plus the
// rbsp_trailing_bits stop byte, so the AU we have is truncated; the
// decoder will show macroblock artefacts in the bottom strip.
//
// Strict mode (?strict=1) drops the truncated IDR and poisons the
// GOP — pristine pixels at the cost of a multi-second freeze until
// the next clean IDR.  Non-strict mode (default) emits it anyway.
func (c *Client) flushMainIDR(nextPFrameTs uint32) {
	if len(c.mainAsm.buf) == 0 {
		c.mainAsm.reset()
		return
	}
	au := append([]byte(nil), c.mainAsm.buf...)
	midGapped := c.mainAsm.curAUGapped
	tailMissing := true
	if c.mainAsm.curAUTotal > 0 && c.mainAsm.curAUDataCount+1 < c.mainAsm.curAUTotal {
		c.stats.fragSkips.Add(1)
		c.stats.fragsLost.Add(uint64(c.mainAsm.curAUTotal - 1 - c.mainAsm.curAUDataCount))
	} else {
		c.stats.fragSkips.Add(1)
		c.stats.fragsLost.Add(1)
	}
	c.mainAsm.reset()
	if c.strict && (midGapped || tailMissing) {
		// Strict mode: drop the IDR if anything was lost — pristine
		// pixels over fluency.  Cascading inter-frame errors are
		// avoided by also poisoning the GOP so subsequent P-frames
		// are dropped until the next clean IDR.
		c.gopPoisoned = true
		c.stats.vidDropped.Add(1)
		c.dropDeferredVideo()
		return
	}
	if midGapped || tailMissing {
		// Non-strict: emit the partial IDR anyway.  ffmpeg will show
		// macroblock artefacts at the gap location for ~50 P-frames
		// until the next clean IDR arrives — visibly noisy but vastly
		// better than the multi-second video freeze that dropping the
		// IDR would produce.
		c.stats.vidDropped.Add(1)
	}
	// Guard the unsigned underflow: the camera clock at boot starts
	// near zero, and a P-frame whose ts is < 40 ms would naively
	// produce nextPFrameTs - 40 = ~0xFFFFFFC0 and lock the rest of
	// the session's PTS into the wrap-around regime.
	if nextPFrameTs >= 40 {
		c.pendingFrameTs = nextPFrameTs - 40
		c.havePendingTs = true
	}
	c.emitAU(au)
}

// emitAU finalises one access unit and queues it for the consumer.
func (c *Client) emitAU(au []byte) {
	if len(au) < 5 {
		return
	}
	frameTs := c.pendingFrameTs
	haveFrameTs := c.havePendingTs
	c.havePendingTs = false

	isKey := annexbContainsNALType(au, h264.NALUTypeIFrame)
	if !isKey && c.mainAsm.framePending && len(c.mainAsm.buf) > 0 {
		c.deferVideoAU(au, frameTs, haveFrameTs)
		return
	}
	if c.emitVideoAU(au, isKey, frameTs, haveFrameTs) && isKey {
		c.flushDeferredVideo(frameTs, haveFrameTs)
	}
}

func (c *Client) emitVideoAU(au []byte, isKey bool, frameTs uint32, haveFrameTs bool) bool {
	if isKey {
		// A fresh IDR clears the GOP-poisoned state.
		c.gopPoisoned = false
	} else if c.strict && c.gopPoisoned {
		// Strict only: drop P-frames referencing a dropped IDR.
		// Non-strict lets the decoder conceal.
		c.stats.vidDropped.Add(1)
		return false
	}

	// PTS comes from the camera's own millisecond clock embedded in
	// the metadata trailer of each frame's last fragment.  This gives
	// per-frame-accurate timestamps that survive forceDrain bursts —
	// wall-clock derived PTS produced "Invalid video timestamp X -> X"
	// duplicates in mpv because multiple AUs flushed within one ms.
	var pts uint32
	if haveFrameTs {
		if !c.haveFirstTs {
			c.firstFrameTs = frameTs
			c.haveFirstTs = true
		}
		// Frame counter is u32 LE ms; subtract origin and convert to
		// the H.264 90 kHz clock.  Wrap-safe via unsigned subtraction.
		ms := frameTs - c.firstFrameTs
		pts = ms * 90
	} else {
		// No trailer seen yet (early packets before the first frame
		// finishes) or the trailer for this AU was lost — fall back
		// to "just-after the last emitted PTS" so playback ordering
		// stays monotonic and the AU doesn't collide with the prior
		// one.
		pts = c.lastEmitTs + 1
	}
	if pts <= c.lastEmitTs && c.lastEmitTs != 0 {
		// Strictly monotonic — never re-use a previous PTS.
		pts = c.lastEmitTs + 1
	}
	c.lastEmitTs = pts

	c.queuePacket(&Packet{
		Codec:      CodecH264,
		Payload:    au,
		FrameNo:    c.emitSeq,
		Timestamp:  pts,
		IsKeyframe: isKey,
	})
	c.emitSeq++
	c.stats.vidFramesOut.Add(1)
	return true
}

func (c *Client) deferVideoAU(au []byte, frameTs uint32, haveFrameTs bool) {
	if len(c.deferredVideo) >= maxDeferredVideoAUs {
		c.stats.vidDropped.Add(1)
		return
	}
	c.deferredVideo = append(c.deferredVideo, deferredVideoAU{
		payload: append([]byte(nil), au...),
		ts:      frameTs,
		haveTs:  haveFrameTs,
	})
}

func (c *Client) flushDeferredVideo(keyTs uint32, haveKeyTs bool) {
	if len(c.deferredVideo) == 0 {
		return
	}
	deferred := c.deferredVideo
	c.deferredVideo = nil
	for _, d := range deferred {
		if haveKeyTs && d.haveTs && d.ts <= keyTs {
			c.stats.vidDropped.Add(1)
			continue
		}
		isKey := annexbContainsNALType(d.payload, h264.NALUTypeIFrame)
		c.emitVideoAU(d.payload, isKey, d.ts, d.haveTs)
	}
}

func (c *Client) dropDeferredVideo() {
	if len(c.deferredVideo) == 0 {
		return
	}
	c.stats.vidDropped.Add(uint64(len(c.deferredVideo)))
	c.deferredVideo = nil
}

// stripFragmentMetadataTrailer removes the 16-byte per-frame metadata
// block the Petlibro firmware appends to the LAST video fragment of
// each frame.  The block is `<codec_id 1B> <variant prefix 4B>
// <7B zeros> <4B LE ms ts>`:
//
//	P-frame:  4e  00 <p_or_k> 00 <stream_id>  00*7  <ts>
//	IDR/key:  4e  00 <p_or_k> 00 <stream_id>  00*7  <ts>
//
// codec_id is 0x4e (= CodecH264) for video.  Byte 1 of the variant
// prefix is 0x00 for P-frames and 0x01 for IDR keyframes.  Byte 3
// (last byte of the variant prefix) is the STREAM ID:
//
//	0x01 = main stream (HD on this camera, 1920x1080)
//	0x02 = sub stream  (SD on this camera, 640x360)
//
// When the camera is in dual-stream mode it sends BOTH streams on
// the same channels and the configured Quality option picks which
// one to keep — see the wantStreamID filter at the top of emit() and
// the end-fragment trailerStreamID discriminator further down.
//
// Stripping only 15 trailer bytes would leave the 0x4e codec_id in
// the slice tail; decoders read it as a stray NAL-14 prefix and bail
// on the next frame with "mb_skip_run invalid at MB 0,0".  Strip all
// 16 bytes.
//
// Callers should only invoke this on the frame's end fragment (whose
// tail unambiguously matches the signature) — in mid-frame fragments
// a coincidental match could shear real slice bytes.  84 fixed bits
// of signature put coincidental matches in the 1-in-2^84 zone.
func stripFragmentMetadataTrailer(p []byte) (stripped []byte, ts uint32, streamID byte, hasTs bool) {
	if len(p) < 16 {
		return p, 0, 0, false
	}
	t := p[len(p)-15:] // 15-byte trailer right after the codec_id byte
	// trailerStreamID range: every PCAP frame observed has stream-id
	// ∈ {0x01, 0x02}.  The wider 0x01..0x0f acceptance that earlier
	// versions used was a defensive over-allow with no evidence
	// behind it — tightening it catches malformed end-fragments that
	// would otherwise be misclassified as valid trailers.
	prefixOK := t[0] == 0x00 && (t[1] == 0x00 || t[1] == 0x01) &&
		t[2] == 0x00 && (t[3] == 0x01 || t[3] == 0x02)
	zerosOK := t[4] == 0 && t[5] == 0 && t[6] == 0 && t[7] == 0 &&
		t[8] == 0 && t[9] == 0 && t[10] == 0
	codecIDOK := p[len(p)-16] == CodecH264
	if prefixOK && zerosOK && codecIDOK {
		ts = binary.LittleEndian.Uint32(t[11:15])
		return p[:len(p)-16], ts, t[3], true
	}
	return p, 0, 0, false
}

// annexbContainsNALType walks an Annex-B buffer and reports whether
// any NAL unit's nal_unit_type matches want.  pkg/h264.NALUType only
// looks at the FIRST NAL of an AU; petlibro AUs may carry AUD/SEI/SPS
// before the IDR slice, so we have to scan all of them.  This is the
// single consolidated NAL-walker for the package — see also the
// AVCC walker used by probe() in producer.go which leans on the same
// h264.NALUType* constants.
func annexbContainsNALType(b []byte, want byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 {
			continue
		}
		var nalPos int
		switch {
		case b[i+2] == 1:
			nalPos = i + 3
		case b[i+2] == 0 && i+4 < len(b) && b[i+3] == 1:
			nalPos = i + 4
		default:
			continue
		}
		if nalPos >= len(b) {
			return false
		}
		if b[nalPos]&0x1F == want {
			return true
		}
	}
	return false
}

// queuePacket hands an assembled Packet to the consumer.  Selecting on
// done first means Close cannot race the send onto the now-closed
// frames channel — recover() and a separate closed-bool aren't needed.
func (c *Client) queuePacket(p *Packet) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case <-c.done:
		return
	case c.frames <- p:
	default:
		c.stats.emitDrops.Add(1)
	}
}

// wrapSeq tracks a monotonic 64-bit counter that follows a 16-bit
// wire counter through wrap-arounds.  Single-step jumps >0x8000 are
// treated as wraps.
type wrapSeq struct {
	ext uint64
}

// extend turns a 16-bit wire counter into the monotonic 64-bit
// extended counter.  Returns ok=false when the wire value would land
// before the start of the sequence (corrupt or stale fragment); the
// caller MUST drop it rather than silently saturate to zero — feeding
// frag 0 into avBuffer poisons drainContiguous because avNextExt
// starts at >= 0x4000.
func (w *wrapSeq) extend(wire uint16) (ext uint64, ok bool) {
	lastWire := uint16(w.ext)
	fwd := (wire - lastWire) & 0xFFFF
	if fwd < 0x8000 {
		return w.ext + uint64(fwd), true
	}
	back := uint64((lastWire - wire) & 0xFFFF)
	if back > w.ext {
		return 0, false
	}
	return w.ext - back, true
}

func (w *wrapSeq) advanceTo(wire uint16) {
	if newExt, ok := w.extend(wire); ok && newExt > w.ext {
		w.ext = newExt
	}
}
