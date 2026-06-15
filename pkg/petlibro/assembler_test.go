package petlibro

import (
	"encoding/binary"
	"testing"
)

// ---------- wrapSeq.extend wrap-around ---------------------------------

// TestWrapSeqExtendForwardSmall — simple forward step within the same
// 16-bit window, no wrap involved. Baseline sanity.
func TestWrapSeqExtendForwardSmall(t *testing.T) {
	w := wrapSeq{ext: 0x4000}
	got, ok := w.extend(0x4001)
	if !ok {
		t.Fatalf("extend(0x4001) returned ok=false")
	}
	if got != 0x4001 {
		t.Fatalf("extend(0x4001) from ext=0x4000 = %d, want 0x4001", got)
	}
}

// TestWrapSeqExtendThroughUint16Boundary — the camera's 16-bit AV-seq
// counter wraps every ~65k fragments (a few seconds at HD bitrates).
// extend() must keep the 64-bit projection monotonic across the
// 0xFFFF -> 0x0000 boundary.
func TestWrapSeqExtendThroughUint16Boundary(t *testing.T) {
	w := wrapSeq{ext: 0xFFFF}
	got, ok := w.extend(0x0000)
	if !ok {
		t.Fatalf("extend(0x0000) returned ok=false")
	}
	if got != 0x10000 {
		t.Fatalf("extend(0x0000) from ext=0xFFFF = 0x%x, want 0x10000", got)
	}
	w.advanceTo(0x0000)
	if w.ext != 0x10000 {
		t.Fatalf("advanceTo(0x0000) left ext=0x%x, want 0x10000", w.ext)
	}
	// Another small step after the wrap stays monotonic.
	got, ok = w.extend(0x0005)
	if !ok {
		t.Fatalf("post-wrap extend(0x0005) returned ok=false")
	}
	if got != 0x10005 {
		t.Fatalf("post-wrap extend(0x0005) = 0x%x, want 0x10005", got)
	}
}

// TestWrapSeqExtendFarBackwardLouddrop — P1-3 turned the "silently
// saturate to 0" behaviour into an explicit ok=false return so the
// caller can drop the fragment.  To take the back branch, fwd =
// (wire - lastWire) & 0xFFFF must be >= 0x8000.  With ext=5
// (lastWire=5), wire=0x8005 yields fwd = 0x8000 → back-distance =
// 0x8000 > 5, returns ok=false.
func TestWrapSeqExtendFarBackwardLouddrop(t *testing.T) {
	w := wrapSeq{ext: 5}
	_, ok := w.extend(0x8005)
	if ok {
		t.Fatalf("far-back extend should return ok=false, got ok=true")
	}
}

// TestWrapSeqExtendNearBackwardWorks — back-distance smaller than ext
// is honoured: a late-arriving fragment from "just before" the current
// position resolves to the correct earlier ext value.
func TestWrapSeqExtendNearBackwardWorks(t *testing.T) {
	w := wrapSeq{ext: 0x10010}
	// lastWire = 0x10 (low 16 of 0x10010). wire=0x000F → fwd =
	// (0xF-0x10)&0xFFFF = 0xFFFF → back path, back-distance=1, fits.
	got, ok := w.extend(0x000F)
	if !ok {
		t.Fatalf("near-back extend(0x000F) returned ok=false")
	}
	if got != 0x1000F {
		t.Fatalf("near-back extend(0x000F) from ext=0x10010 = 0x%x, want 0x1000F", got)
	}
}

// TestWrapSeqAdvanceToNoRegress — advanceTo must never lower ext.
func TestWrapSeqAdvanceToNoRegress(t *testing.T) {
	w := wrapSeq{ext: 0x12345}
	w.advanceTo(0x2345) // same low-16 as ext, no movement
	if w.ext != 0x12345 {
		t.Fatalf("advanceTo same-low-16 changed ext to 0x%x, want 0x12345", w.ext)
	}
	// A small forward step.
	w.advanceTo(0x2346)
	if w.ext != 0x12346 {
		t.Fatalf("advanceTo +1 left ext=0x%x, want 0x12346", w.ext)
	}
	// A look-back attempt must not regress.
	w.advanceTo(0x2340)
	if w.ext != 0x12346 {
		t.Fatalf("backward advanceTo regressed ext to 0x%x, want 0x12346", w.ext)
	}
}

// ---------- stripFragmentMetadataTrailer positive + negative -----------

// makeTrailerPayload constructs a synthetic end-fragment payload: a
// few "slice" bytes followed by the 16-byte metadata trailer the
// Petlibro firmware appends. Layout per stripFragmentMetadataTrailer's
// docstring:
//
//	<slice bytes ...> <codec_id 1B = 0x4e> <00 b1 00 streamID> <00*7> <ts:4 LE>
func makeTrailerPayload(slice []byte, isIDR bool, streamID byte, ts uint32) []byte {
	out := append([]byte{}, slice...)
	out = append(out, CodecH264) // codec_id 0x4e
	b1 := byte(0x00)             // P-frame
	if isIDR {
		b1 = 0x01
	}
	out = append(out, 0x00, b1, 0x00, streamID)
	out = append(out, 0, 0, 0, 0, 0, 0, 0) // 7 zero bytes
	var tsBytes [4]byte
	binary.LittleEndian.PutUint32(tsBytes[:], ts)
	out = append(out, tsBytes[:]...)
	return out
}

// TestStripFragmentMetadataTrailerPositive — a synthetic end-fragment
// with a valid signature returns stripped bytes, ts, and stream-id.
func TestStripFragmentMetadataTrailerPositive(t *testing.T) {
	slice := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	p := makeTrailerPayload(slice, true, 0x01, 0xDEADBEEF)

	stripped, ts, sid, ok := stripFragmentMetadataTrailer(p)
	if !ok {
		t.Fatalf("hasTs=false on a valid trailer; len(p)=%d", len(p))
	}
	if string(stripped) != string(slice) {
		t.Fatalf("stripped=%x, want %x", stripped, slice)
	}
	if ts != 0xDEADBEEF {
		t.Fatalf("ts=0x%x, want 0xDEADBEEF", ts)
	}
	if sid != 0x01 {
		t.Fatalf("streamID=0x%02x, want 0x01", sid)
	}
}

// TestStripFragmentMetadataTrailerSubStreamID — same shape but with
// stream-id = 0x02 (SD sub-stream).
func TestStripFragmentMetadataTrailerSubStreamID(t *testing.T) {
	slice := []byte{0x01, 0x02, 0x03}
	p := makeTrailerPayload(slice, false, 0x02, 0x12345678)

	_, ts, sid, ok := stripFragmentMetadataTrailer(p)
	if !ok {
		t.Fatalf("hasTs=false")
	}
	if sid != 0x02 {
		t.Fatalf("streamID=0x%02x, want 0x02 (SD)", sid)
	}
	if ts != 0x12345678 {
		t.Fatalf("ts=0x%x, want 0x12345678", ts)
	}
}

// TestStripFragmentMetadataTrailerNegativeWrongCodec — the 16-byte
// trailer-signature check must reject a payload whose [-16] byte
// isn't 0x4e (the H.264 codec_id). This is the discriminator that
// keeps mid-frame data fragments out of the strip path.
func TestStripFragmentMetadataTrailerNegativeWrongCodec(t *testing.T) {
	slice := []byte{0xAA, 0xBB}
	p := makeTrailerPayload(slice, false, 0x01, 1)
	// Corrupt the codec_id byte (which lives at p[len(p)-16]).
	p[len(p)-16] = 0x42

	stripped, ts, sid, ok := stripFragmentMetadataTrailer(p)
	if ok {
		t.Fatalf("hasTs=true on a wrong-codec trailer; should not match")
	}
	if &stripped[0] != &p[0] {
		t.Fatalf("on negative match, stripped should alias p (not copy)")
	}
	if ts != 0 || sid != 0 {
		t.Fatalf("on negative match, ts/sid must be zero; got ts=0x%x sid=0x%02x", ts, sid)
	}
}

// TestStripFragmentMetadataTrailerNegativeNonZeroFill — a payload that
// has the right codec_id but corrupt zero-fill region must NOT match.
// This guards against accidental real-slice matches: 84 fixed bits of
// zero are what make the signature 1-in-2^84.
func TestStripFragmentMetadataTrailerNegativeNonZeroFill(t *testing.T) {
	slice := []byte{0xAA, 0xBB}
	p := makeTrailerPayload(slice, true, 0x01, 1)
	// Corrupt one of the 7 zero bytes (positions [len(p)-11 .. len(p)-5]).
	p[len(p)-8] = 0x42

	_, _, _, ok := stripFragmentMetadataTrailer(p)
	if ok {
		t.Fatalf("hasTs=true on a corrupted-zero-fill trailer; signature too lax")
	}
}

// TestStripFragmentMetadataTrailerTooShort — payloads shorter than 16
// bytes can't possibly carry the trailer.
func TestStripFragmentMetadataTrailerTooShort(t *testing.T) {
	for _, n := range []int{0, 1, 15} {
		p := make([]byte, n)
		_, _, _, ok := stripFragmentMetadataTrailer(p)
		if ok {
			t.Errorf("hasTs=true for %d-byte payload; expected false", n)
		}
	}
}

// TestStripFragmentMetadataTrailerStreamIDRange — the prefix accepts
// exactly stream-id ∈ {0x01, 0x02} (HD main, SD sub).  Earlier
// 0x01..0x0f was a defensive over-allow with no PCAP evidence;
// tightening to the observed set catches malformed end-fragments
// that would otherwise misclassify as valid trailers.
func TestStripFragmentMetadataTrailerStreamIDRange(t *testing.T) {
	cases := []struct {
		sid  byte
		want bool
	}{
		{0x00, false},
		{0x01, true},
		{0x02, true},
		{0x03, false},
		{0x0f, false},
		{0x10, false},
	}
	for _, c := range cases {
		p := makeTrailerPayload([]byte{0x99}, true, c.sid, 0)
		_, _, _, ok := stripFragmentMetadataTrailer(p)
		if ok != c.want {
			t.Errorf("streamID=0x%02x: got ok=%v, want %v", c.sid, ok, c.want)
		}
	}
}

// ---------- frame reassembly end-to-end --------------------------------

// fragSpec describes one synthetic wire fragment we'll feed into
// parseDatagram. Encoding mirrors what client.go's switch arms in
// parseDatagram read from `inner`.
type fragSpec struct {
	channel    byte   // inner[16]
	sub17      byte   // inner[17]
	b1         byte   // inner[1]
	subWire    uint16 // inner[18:20]: AV-sequence (the wrap-counter)
	totalFrags byte   // inner[20]
	fragIdx    uint16 // inner[22:24]
	paylen     uint16 // inner[24:26]: real payload length (0 = "rest of buf")
	frameNum   uint32 // inner[28:32]: camera's frame counter for this channel
	payload    []byte // bytes from inner[36:]
}

// makePkt synthesises the post-decryption packet bytes parseDatagram
// expects: a 0x1C-byte outer header followed by an inner of length
// 36 + len(payload).  Only the offsets actually read are populated.
func makePkt(f fragSpec) []byte {
	inner := make([]byte, 36+len(f.payload))
	inner[0] = 0x0c
	inner[1] = f.b1
	inner[16] = f.channel
	inner[17] = f.sub17
	binary.LittleEndian.PutUint16(inner[18:], f.subWire)
	inner[20] = f.totalFrags
	binary.LittleEndian.PutUint16(inner[22:], f.fragIdx)
	binary.LittleEndian.PutUint16(inner[24:], f.paylen)
	binary.LittleEndian.PutUint32(inner[28:], f.frameNum)
	copy(inner[36:], f.payload)

	pkt := make([]byte, 0x1C+len(inner))
	pkt[3] = flagsRecv
	binary.LittleEndian.PutUint16(pkt[8:], msgSessionD2C)
	copy(pkt[0x1C:], inner)
	return pkt
}

// newTestClient returns a Client wired up enough to exercise
// parseDatagram / drainContiguous / emit without any actual UDP. The
// internal counters and assembly state are reset to a fresh
// just-finished-bootstrap shape with `firstSubWire` as the first
// expected AV-seq value.
func newTestClient(firstSubWire uint16, quality string) *Client {
	c := &Client{
		quality:  quality,
		frames:   make(chan *Packet, 256),
		avBuffer: make(map[uint64]*pendingFrag),
	}
	if firstSubWire == 0 {
		c.wrap = wrapSeq{ext: 0x4000}
	} else {
		c.wrap = wrapSeq{ext: uint64(firstSubWire)}
	}
	c.avNextExt = c.wrap.ext
	c.avHighExt = c.wrap.ext - 1
	c.avPrevSubWire = uint16(c.wrap.ext - 1)
	return c
}

// drainAll pulls every queued Packet without blocking and returns them.
func drainAll(c *Client) []*Packet {
	var out []*Packet
	for {
		select {
		case p := <-c.frames:
			out = append(out, p)
		default:
			return out
		}
	}
}

// dataFragPayload generates a deterministic n-byte slice with a
// fingerprint so the assembled AU can be validated against a known
// concatenation.
func dataFragPayload(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i%19)
	}
	return out
}

// TestEndToEndMultiFragmentIDR walks an IDR frame split across 4 data
// fragments + 1 end fragment through handleIncoming and asserts a
// single AU is emitted with the concatenated slice bytes.
//
// "78 fragments → one IDR AU" is the production case (PLAF203 HD); we
// scale down to 4+1 for test runtime, but the assembler logic is the
// same — totalFrags + fragIdx gap detection + trailer-signature
// end-fragment detection.
//
// First emit's PTS is anchored at 0 (firstFrameTs is set from the first
// trailer-borne ts seen). Subsequent emits' PTS values are validated
// in TestEndToEndMultiFragmentIDRMonotonicPTS.
func TestEndToEndMultiFragmentIDR(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	const totalFrags = 5 // 4 data + 1 end
	const frameNum = uint32(1)
	const tsMs = uint32(500)

	var wantPayload []byte
	for i := 0; i < 4; i++ {
		data := dataFragPayload(byte(0x10+i), 16)
		wantPayload = append(wantPayload, data...)
		c.parseDatagram(makePkt(fragSpec{
			channel:    innerChMain,
			b1:         0x00,
			subWire:    uint16(0x4000 + i),
			totalFrags: totalFrags,
			fragIdx:    uint16(i),
			paylen:     uint16(len(data)),
			frameNum:   frameNum,
			payload:    data,
		}))
	}

	// End fragment: b1=0x01 sub17=0x01, payload has trailer signature.
	// Use a 5-byte tail so the post-strip total slice is >= 5 (emitAU
	// gate).
	tail := []byte{0xCA, 0xFE, 0xBA, 0xBE, 0xDE}
	wantPayload = append(wantPayload, tail...)
	end := makeTrailerPayload(tail, true, 0x01, tsMs)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChMain,
		b1:         0x01,
		sub17:      0x01,
		subWire:    uint16(0x4000 + 4),
		totalFrags: totalFrags,
		fragIdx:    4,
		paylen:     uint16(len(end)),
		frameNum:   frameNum,
		payload:    end,
	}))

	pkts := drainAll(c)
	if len(pkts) != 1 {
		t.Fatalf("emitted %d packets, want 1; pkts=%+v", len(pkts), pkts)
	}
	got := pkts[0]
	if got.Codec != CodecH264 {
		t.Fatalf("codec=0x%02x, want CodecH264=0x%02x", got.Codec, CodecH264)
	}
	if string(got.Payload) != string(wantPayload) {
		t.Fatalf("payload mismatch:\n got=%x\nwant=%x", got.Payload, wantPayload)
	}
	// First emit anchors firstFrameTs := pendingFrameTs → PTS = 0.
	if got.Timestamp != 0 {
		t.Fatalf("first PTS=%d, want 0 (firstFrameTs anchor)", got.Timestamp)
	}
}

// TestEndToEndMultiFragmentIDRMonotonicPTS — two back-to-back IDRs
// with strictly-increasing trailer timestamps must produce two AUs
// whose 90 kHz PTS values are strictly increasing. The first AU
// anchors firstFrameTs (= 0); the second AU's PTS = (ts2-ts1)*90.
func TestEndToEndMultiFragmentIDRMonotonicPTS(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	sendIDR := func(frameNum uint32, baseSub uint16, tsMs uint32) {
		// 2 data + 1 end = totalFrags 3. tail of 8 bytes keeps the
		// post-strip AU >= 5 for the emitAU minimum-len gate.
		for i := 0; i < 2; i++ {
			c.parseDatagram(makePkt(fragSpec{
				channel:    innerChMain,
				b1:         0x00,
				subWire:    baseSub + uint16(i),
				totalFrags: 3,
				fragIdx:    uint16(i),
				paylen:     8,
				frameNum:   frameNum,
				payload:    dataFragPayload(byte(frameNum), 8),
			}))
		}
		end := makeTrailerPayload([]byte{0x99, 0x88, 0x77, 0x66, 0x55}, true, 0x01, tsMs)
		c.parseDatagram(makePkt(fragSpec{
			channel:    innerChMain,
			b1:         0x01,
			sub17:      0x01,
			subWire:    baseSub + 2,
			totalFrags: 3,
			fragIdx:    2,
			paylen:     uint16(len(end)),
			frameNum:   frameNum,
			payload:    end,
		}))
	}

	sendIDR(1, 0x4000, 100)
	sendIDR(2, 0x4003, 200)

	pkts := drainAll(c)
	if len(pkts) != 2 {
		t.Fatalf("emitted %d packets, want 2", len(pkts))
	}
	if pkts[0].Timestamp >= pkts[1].Timestamp {
		t.Fatalf("PTS not monotonic: %d then %d", pkts[0].Timestamp, pkts[1].Timestamp)
	}
	// First PTS is 0 because firstFrameTs anchors to the first ts seen.
	if pkts[0].Timestamp != 0 {
		t.Fatalf("first PTS=%d, want 0 (anchored to firstFrameTs)", pkts[0].Timestamp)
	}
	if pkts[1].Timestamp != (200-100)*90 {
		t.Fatalf("second PTS=%d, want %d", pkts[1].Timestamp, (200-100)*90)
	}
}

// ---------- dual-stream stream-id discrimination -----------------------

// TestDualStreamSerialised verifies the serialised-not-interleaved
// assumption: with quality=hd, an HD IDR (stream-id 0x01) immediately
// followed by an SD IDR (stream-id 0x02) on the same channel must
// produce ONE emit (the HD one). The SD frame's data fragments are
// accumulated optimistically and discarded at the end-fragment
// trailer-streamID check.
func TestDualStreamSerialised(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	hdData := dataFragPayload(0xAA, 16)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChMain,
		b1:         0x00,
		subWire:    0x4000,
		totalFrags: 2,
		fragIdx:    0,
		paylen:     uint16(len(hdData)),
		frameNum:   10,
		payload:    hdData,
	}))
	hdTail := []byte{0x11, 0x22, 0x33, 0x44, 0x55}
	hdEnd := makeTrailerPayload(hdTail, true, 0x01, 1000)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChMain,
		b1:         0x01,
		sub17:      0x01,
		subWire:    0x4001,
		totalFrags: 2,
		fragIdx:    1,
		paylen:     uint16(len(hdEnd)),
		frameNum:   10,
		payload:    hdEnd,
	}))

	// SD IDR right after — same channel (ch=0x05) but stream-id 0x02
	// in its trailer. Should be discarded by the wantStreamID filter
	// once the end fragment reveals the stream.
	sdData := dataFragPayload(0xBB, 16)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChMain,
		b1:         0x00,
		subWire:    0x4002,
		totalFrags: 2,
		fragIdx:    0,
		paylen:     uint16(len(sdData)),
		frameNum:   11,
		payload:    sdData,
	}))
	sdTail := []byte{0x33, 0x44, 0x55, 0x66, 0x77}
	sdEnd := makeTrailerPayload(sdTail, true, 0x02, 1100)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChMain,
		b1:         0x01,
		sub17:      0x01,
		subWire:    0x4003,
		totalFrags: 2,
		fragIdx:    1,
		paylen:     uint16(len(sdEnd)),
		frameNum:   11,
		payload:    sdEnd,
	}))

	pkts := drainAll(c)
	if len(pkts) != 1 {
		t.Fatalf("emitted %d packets, want 1 (HD only, SD discarded by trailer-streamID); pkts=%+v", len(pkts), pkts)
	}
	want := append(append([]byte{}, hdData...), hdTail...)
	if string(pkts[0].Payload) != string(want) {
		t.Fatalf("HD payload mismatch:\n got=%x\nwant=%x", pkts[0].Payload, want)
	}
}

// TestDualStreamFilterSubSingleFragment verifies the single-fragment-
// on-ch=0x07-with-trailer fast-path filter: a stream-id=0x02 fragment
// on ch=0x07 when quality=hd must be filtered (no emit). The
// drainContiguous still advances avNextExt past it so subsequent
// in-order fragments aren't blocked.
//
// Note: a `return` early-exit at the wantStreamID check fires AFTER
// drainContiguous has already done avNextExt++, so wire-order
// progression is preserved. The follow-up matching HD fragment
// therefore reaches emit cleanly.
func TestDualStreamFilterSubSingleFragment(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Single-fragment P-frame on ch=0x07, stream-id 0x02 (SD).
	// totalFrags=1 means the trailer-bearing fragment IS the whole
	// frame, and the wantStreamID filter at the top of emit() rejects
	// it for being the wrong stream.
	sdEnd := makeTrailerPayload([]byte{0x55, 0x66, 0x77, 0x88, 0x99}, false, 0x02, 100)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChSub,
		b1:         0x00,
		subWire:    0x4000,
		totalFrags: 1,
		fragIdx:    0,
		paylen:     uint16(len(sdEnd)),
		frameNum:   5,
		payload:    sdEnd,
	}))

	// Then a single-fragment HD P-frame (stream-id 0x01) — should emit.
	hdEnd := makeTrailerPayload([]byte{0xCC, 0xDD, 0xEE, 0xFF, 0xAA}, false, 0x01, 200)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChSub,
		b1:         0x00,
		subWire:    0x4001,
		totalFrags: 1,
		fragIdx:    0,
		paylen:     uint16(len(hdEnd)),
		frameNum:   6,
		payload:    hdEnd,
	}))

	pkts := drainAll(c)
	if len(pkts) != 1 {
		t.Fatalf("emitted %d packets, want 1 (HD only)", len(pkts))
	}
	want := []byte{0xCC, 0xDD, 0xEE, 0xFF, 0xAA}
	if string(pkts[0].Payload) != string(want) {
		t.Fatalf("payload=%x, want %x (HD P-frame slice bytes)", pkts[0].Payload, want)
	}
}

// TestChannelAsmInterleavedMainAndSub — ch=0x05 IDR and ch=0x07 P-frame
// fragments arriving interleaved (in wire order) must NOT corrupt each
// other: the per-channel `channelAsm` state means each channel can
// reassemble independently even while the other is mid-frame.
//
// This is the regression for the "shared buffer would drop every
// partial IDR" comment near channelAsm's docstring.
//
// Wire sequence: IDR-data on ch=0x05, then sub P-frame on ch=0x07,
// then IDR-end on ch=0x05. The P-frame must be deferred until the
// complete IDR, including its end-fragment tail, has been emitted.
func TestChannelAsmInterleavedMainAndSub(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Embed an H.264 NAL-5 (IDR-slice) start code into the main data
	// so annexbContainsNALType(au, h264.NALUTypeIFrame) flags this AU
	// as a keyframe in emitAU. The bytes are: Annex-B start-code 00
	// 00 00 01, then
	// NAL header byte 0x65 (forbidden_zero_bit=0, nal_ref_idc=3,
	// nal_unit_type=5), then a few payload bytes.
	mainData := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		paylen: uint16(len(mainData)), frameNum: 100, payload: mainData,
	}))
	// P-frame on ch=0x07 arrives between the IDR's data and end.
	subEnd := makeTrailerPayload([]byte{0x77, 0x88, 0x99, 0xAA, 0xBB}, false, 0x01, 1500)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChSub, b1: 0x00,
		subWire: 0x4001, totalFrags: 1, fragIdx: 0,
		paylen: uint16(len(subEnd)), frameNum: 200, payload: subEnd,
	}))
	mainEnd := makeTrailerPayload([]byte{0xEE, 0xDD, 0xCC, 0xBB, 0xAA}, true, 0x01, 1000)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x01, sub17: 0x01,
		subWire: 0x4002, totalFrags: 2, fragIdx: 1,
		paylen: uint16(len(mainEnd)), frameNum: 100, payload: mainEnd,
	}))

	pkts := drainAll(c)
	if len(pkts) != 2 {
		t.Fatalf("emitted %d packets, want 2 (complete IDR, then deferred P-frame)", len(pkts))
	}
	if !pkts[0].IsKeyframe {
		t.Fatalf("first packet is not the completed IDR")
	}
	if pkts[1].IsKeyframe {
		t.Fatalf("second packet is keyframe, want deferred P-frame")
	}
	wantKey := append(append([]byte{}, mainData...), []byte{0xEE, 0xDD, 0xCC, 0xBB, 0xAA}...)
	if string(pkts[0].Payload) != string(wantKey) {
		t.Fatalf("IDR payload missing tail:\n got=%x\nwant=%x", pkts[0].Payload, wantKey)
	}
	wantP := []byte{0x77, 0x88, 0x99, 0xAA, 0xBB}
	if string(pkts[1].Payload) != string(wantP) {
		t.Fatalf("P payload=%x, want %x", pkts[1].Payload, wantP)
	}
	if pkts[0].Timestamp >= pkts[1].Timestamp {
		t.Fatalf("deferred P-frame timestamp did not follow IDR: %d then %d", pkts[0].Timestamp, pkts[1].Timestamp)
	}
}

func TestStrictMissingIDRTailDropsKeyframeAndDeferredPFrames(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.strict = true

	mainData := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x99}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		paylen: uint16(len(mainData)), frameNum: 100, payload: mainData,
	}))
	subEnd := makeTrailerPayload([]byte{0x77, 0x88, 0x99}, false, 0x01, 1500)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChSub, b1: 0x00,
		subWire: 0x4001, totalFrags: 1, fragIdx: 0,
		paylen: uint16(len(subEnd)), frameNum: 200, payload: subEnd,
	}))
	nextMain := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xAA, 0xBB}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4002, totalFrags: 2, fragIdx: 0,
		paylen: uint16(len(nextMain)), frameNum: 101, payload: nextMain,
	}))

	if pkts := drainAll(c); len(pkts) != 0 {
		t.Fatalf("emitted %d packets after missing IDR tail in strict mode, want 0", len(pkts))
	}
	if !c.gopPoisoned {
		t.Fatalf("gopPoisoned=false after strict-mode IDR tail loss")
	}
	if c.stats.vidDropped.Load() == 0 {
		t.Fatalf("vidDropped=0 after strict-mode IDR tail loss")
	}
}

func TestContiguousAckExtIgnoresHighWaterGaps(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.avNextExt = 0x4001
	c.avHighExt = 0x4008

	ackExt, ok := c.contiguousAckExt()
	if !ok {
		t.Fatalf("contiguousAckExt returned ok=false")
	}
	if ackExt != 0x4000 {
		t.Fatalf("ackExt=0x%x, want last contiguous 0x4000 despite high-water 0x4008", ackExt)
	}
}

// TestFragIdxGapDetection — a missing middle data fragment must be
// counted as a frag-skip + frags-lost gap. We model "fragment idx=1
// lost on the wire" by sending only data fragments at fragIdx 0 and
// 2 (totalFrags=3, so 2 data + 1 end) over contiguous wire
// subWires. The fragIdx jump 0→2 against the asm's expectFragIdx=1
// is the gap signal the emit() no-trailer branch is testing for.
func TestFragIdxGapDetection(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Data fragment 0 of 3 (subWire 0x4000, fragIdx 0).
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4000, totalFrags: 3, fragIdx: 0,
		paylen: 8, frameNum: 1, payload: dataFragPayload(0x10, 8),
	}))
	// Data fragment 2 of 3 (subWire 0x4001, fragIdx 2 — gap from 0→2).
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4001, totalFrags: 3, fragIdx: 2,
		paylen: 8, frameNum: 1, payload: dataFragPayload(0x30, 8),
	}))
	// End fragment, fragIdx 16 (the camera reuses 16 as the end-marker;
	// here we just need a trailer-bearing fragment past the data).
	end := makeTrailerPayload([]byte{0x99, 0x88, 0x77, 0x66, 0x55}, true, 0x01, 50)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x01, sub17: 0x01,
		subWire: 0x4002, totalFrags: 3, fragIdx: 16,
		paylen: uint16(len(end)), frameNum: 1, payload: end,
	}))

	if c.stats.fragSkips.Load() == 0 {
		t.Errorf("fragSkips=0; gap from frag 0→2 should have been counted")
	}
	if c.stats.fragsLost.Load() == 0 {
		t.Errorf("fragsLost=0; at least one lost fragment expected")
	}
	if c.stats.vidDropped.Load() == 0 {
		t.Errorf("vidDropped=0; non-strict mode still increments on gapped IDR")
	}
}

// TestPaylenTruncatesPadding — when paylen is non-zero and smaller
// than (len(inner)-36), the assembler must trim trailing padding.
// This prevents synthesised start-code bytes in the padding from
// reaching the decoder.
func TestPaylenTruncatesPadding(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// inner has 16 bytes after offset 36, but paylen says only 4 are real.
	raw := []byte{0xAA, 0xBB, 0xCC, 0xDD /* padding: */, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		paylen: 4, frameNum: 1, payload: raw,
	}))
	end := makeTrailerPayload([]byte{0x99, 0x88, 0x77, 0x66, 0x55}, true, 0x01, 1)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x01, sub17: 0x01,
		subWire: 0x4001, totalFrags: 2, fragIdx: 1,
		paylen: uint16(len(end)), frameNum: 1, payload: end,
	}))

	pkts := drainAll(c)
	if len(pkts) != 1 {
		t.Fatalf("emitted %d, want 1", len(pkts))
	}
	want := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0x99, 0x88, 0x77, 0x66, 0x55}
	if string(pkts[0].Payload) != string(want) {
		t.Fatalf("payload=%x, want %x (padding past paylen=4 trimmed)", pkts[0].Payload, want)
	}
}

func TestMalformedPaylenDropsPayload(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	raw := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x00,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		paylen: 64, frameNum: 1, payload: raw,
	}))
	end := makeTrailerPayload([]byte{0x99, 0x88, 0x77}, true, 0x01, 1)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChMain, b1: 0x01, sub17: 0x01,
		subWire: 0x4001, totalFrags: 2, fragIdx: 1,
		paylen: uint16(len(end)), frameNum: 1, payload: end,
	}))

	if pkts := drainAll(c); len(pkts) != 0 {
		t.Fatalf("emitted %d packets after malformed paylen, want 0", len(pkts))
	}
}
