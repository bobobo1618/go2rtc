package petlibro

import (
	"encoding/binary"
	"encoding/hex"
)

// Petlibro / Kalay LAN protocol constants.
// PROTOCOL_NOTES.txt in the project root has the full byte-level docs.

const (
	LANPort = 32761

	// Outer Kalay header (28 bytes) - byte 2 is the protocol version
	// (0x1D for Petlibro firmware vs 0x19 for Wyze).
	magicVersion = 0x1D

	// flags at outer byte 3
	flagsControl = 0x02 // LAN_SEARCH3, KNOCK2
	flagsSession = 0x0B // 0x407 phone→cam data
	flagsRecv    = 0x0A // 0x408 cam→phone

	// IOTC msg-type field at offset 8..9 of the outer header
	msgLANSearch3 uint16 = 0x0601
	msgLANSearchR uint16 = 0x0602
	msgKnock2     uint16 = 0x0402
	msgKnockRR2   uint16 = 0x0404
	msgSessionC2D uint16 = 0x0407
	msgSessionD2C uint16 = 0x0408
	msgAliveC2D   uint16 = 0x0427
)

// sdkVersion is what we put in the LAN_SEARCH3 / KNOCK2 packets at
// offset 0x34/0x30 — matches what the Petlibro Android app sends.
var sdkVersion = []byte{0x00, 0x08, 0x03, 0x04}

// IOCtrl IDs (from PROTOCOL_NOTES — verified live).
const (
	IOCtrlSetStreamCtrl uint32 = 0x0024
	IOCtrlVendor0322    uint32 = 0x0322
	IOCtrlVendor032A    uint32 = 0x032A
	IOCtrlVendor0372    uint32 = 0x0372
	IOCtrlStart         uint32 = 0x01FF
	IOCtrlStop          uint32 = 0x02FF
	IOCtrlAudioOn       uint32 = 0x0300
	IOCtrlAudioOff      uint32 = 0x0301
)

// SETSTREAMCTRL bodies (12 bytes) — chan + type + padding.
var (
	qualityHD = []byte{0x24, 0, 0, 0, 0x01, 0x00, 0xff, 0x3f, 0, 0, 0, 0}
	qualitySD = []byte{0x24, 0, 0, 0, 0x02, 0x00, 0x8a, 0x81, 0, 0, 0, 0}
)

// Inner-cmd "channel" markers at offset 16..17.
const (
	innerChMain  byte = 0x05 // AV main stream (keyframes)
	innerChSub   byte = 0x07 // AV sub stream (P-frames)
	innerChAudio byte = 0x03 // AAC audio
)

// XOR key applied to IOCtrl bodies (the "outer" Charlie key — same one
// pkg/tutk uses for its TransCodePartial, just here at the body level).
var xorKey = []byte("Charlie is the d")

func xorBody(b []byte) []byte {
	out := make([]byte, len(b))
	for i, x := range b {
		out[i] = x ^ xorKey[i%16]
	}
	return out
}

// buildOuter builds a 28-byte Kalay outer header + body.
//
//	0..3   04 02 1D <flags>
//	4..5   plen LE = body+12
//	6..7   seq LE
//	8..9   msg_type LE (0x0407 = phone→cam data)
//	10..11 subtype LE (0x0021)
//	12..13 nonce[0..1]
//	14     channel_id  (0 normal, 1 after PLAY re-handshake)
//	15     datatype    (1 for DTLS-shaped, 0 otherwise)
//	16..19 0x0000000C
//	20..27 full 8-byte nonce
//	28..   body
func buildOuter(nonce []byte, seq uint16, body []byte, datatype, channelID, flags byte) []byte {
	plen := uint16(len(body) + 0x0C)
	p := make([]byte, 0x1C+len(body))
	p[0] = 0x04
	p[1] = 0x02
	p[2] = magicVersion
	p[3] = flags
	binary.LittleEndian.PutUint16(p[4:], plen)
	binary.LittleEndian.PutUint16(p[6:], seq)
	binary.LittleEndian.PutUint16(p[8:], msgSessionC2D)
	binary.LittleEndian.PutUint16(p[10:], 0x0021)
	p[12] = nonce[0]
	p[13] = nonce[1]
	p[14] = channelID
	p[15] = datatype
	binary.LittleEndian.PutUint32(p[16:], 0x0000000C)
	copy(p[20:], nonce)
	copy(p[28:], body)
	return p
}

func buildLANSearch3(uid string, nonce []byte, w3 byte) []byte {
	p := make([]byte, 88)
	p[0] = 0x04
	p[1] = 0x02
	p[2] = magicVersion
	p[3] = flagsControl
	binary.LittleEndian.PutUint16(p[4:], 72)
	binary.LittleEndian.PutUint16(p[8:], msgLANSearch3)
	binary.LittleEndian.PutUint16(p[10:], 0x0021)
	copy(p[0x10:0x24], []byte(uid))
	copy(p[0x34:0x38], sdkVersion)
	copy(p[0x38:0x40], nonce)
	p[0x40] = w3
	copy(p[0x4A:0x52], []byte("00000000"))
	return p
}

func buildKnock2(uid string, nonce []byte) []byte {
	p := make([]byte, 52)
	p[0] = 0x04
	p[1] = 0x02
	p[2] = magicVersion
	p[3] = flagsControl
	binary.LittleEndian.PutUint16(p[4:], 36)
	binary.LittleEndian.PutUint16(p[8:], msgKnock2)
	binary.LittleEndian.PutUint16(p[10:], 0x0033)
	copy(p[0x10:0x24], []byte(uid))
	copy(p[0x24:0x2C], nonce)
	copy(p[0x30:0x34], sdkVersion)
	return p
}

func buildAliveC2D(nonce []byte) []byte {
	p := make([]byte, 24)
	p[0] = 0x04
	p[1] = 0x02
	p[2] = magicVersion
	p[3] = 0x0A
	binary.LittleEndian.PutUint16(p[4:], 8)
	binary.LittleEndian.PutUint16(p[8:], msgAliveC2D)
	binary.LittleEndian.PutUint16(p[10:], 0x0012)
	copy(p[0x10:0x18], nonce)
	return p
}

// Inner-body builders ----------------------------------------------------

// innerData wraps an IOCtrl-style body:
//
//	0c 00 0c 00 <cnt:2> 00 00  00 00 00 00 00 00 00 00
//	<chan_hi:2> <sub:2>  01 00 00 00
//	<paylen:4>  <sub:4>  00 00 00 00
//	<xor'd payload>
func innerData(counter, chanHi, subIdx uint16, payload []byte) []byte {
	b := make([]byte, 36+len(payload))
	b[0] = 0x0c
	b[2] = 0x0c
	binary.LittleEndian.PutUint16(b[4:], counter)
	binary.LittleEndian.PutUint16(b[16:], chanHi)
	binary.LittleEndian.PutUint16(b[18:], subIdx)
	b[20] = 0x01
	binary.LittleEndian.PutUint32(b[24:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(b[28:], uint32(subIdx))
	copy(b[36:], xorBody(payload))
	return b
}

// innerAck — sliding-window ACK in AV mode.
//
//	09 00 0c 00 <cnt:2> 00 00 <av_prev:2> <av_curr:2>
//	<chan_idx:4>  00 00 <sub:2> <tick16:2>  00 00
func innerAck(counter, avPrev, avCurr uint16, chanIdx uint32, subIdx, tick uint16) []byte {
	b := make([]byte, 24)
	b[0] = 0x09
	b[2] = 0x0c
	binary.LittleEndian.PutUint16(b[4:], counter)
	binary.LittleEndian.PutUint16(b[8:], avPrev)
	binary.LittleEndian.PutUint16(b[10:], avCurr)
	binary.LittleEndian.PutUint32(b[12:], chanIdx)
	binary.LittleEndian.PutUint16(b[18:], subIdx)
	binary.LittleEndian.PutUint16(b[20:], tick)
	return b
}

// innerNotice — 0b channel-state notice (post-LOGIN).
func innerNotice(counter, lastRecv uint16, tick32, code uint32) []byte {
	b := make([]byte, 20)
	b[0] = 0x0b
	b[2] = 0x0c
	binary.LittleEndian.PutUint16(b[4:], counter)
	binary.LittleEndian.PutUint16(b[6:], lastRecv)
	binary.LittleEndian.PutUint32(b[8:], tick32)
	binary.LittleEndian.PutUint32(b[12:], code)
	return b
}

// innerHeartbeat — 0a 08 heartbeat with 32-bit tick.
func innerHeartbeat(counter uint16, tick32 uint32) []byte {
	b := make([]byte, 16)
	b[0] = 0x0a
	b[1] = 0x08
	b[2] = 0x0c
	binary.LittleEndian.PutUint16(b[4:], counter)
	binary.LittleEndian.PutUint32(b[8:], tick32)
	binary.LittleEndian.PutUint16(b[12:], 0x0032)
	return b
}

// IOCtrl helpers -------------------------------------------------------

// ioctlBody12 builds a 12-byte IOCtrl payload — [u32 ctrlID, 8 zero bytes].
func ioctlBody12(ctrlID uint32) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b, ctrlID)
	return b
}

// LOGIN A/B/DTLS templates (verbatim from petliapp.pcap frames 1596,
// 1617, 1618 — the only dynamic field is the 4-byte session seed at
// offset 0x14 of LOGIN A/B, replaced in buildLoginPair).
const dtlsTemplateHex = "" +
	"16feff000000000000000000f4010000e800000000000000e8fefd" +
	"00000000000000000000000000000000000000000000000000" + // 25 random bytes
	"46dcc424862dad" +
	"cf000000000000b800" + // 9 dynamic/static bytes
	"7a60c0248d01edee144f2ee82ec0a1a02bacc2e485412d4f6462e462e60cb17c" +
	"6a6c08288e9d61c678eee7728e0cbddc4771cae48e8d6d8f3443eeea2e00addc" +
	"d7e1cbe48d9ded0f245fe576af0cbd6ca6b1c9e48d8dad451402ec76070cb15c" +
	"a6d1c6e4a43dedbc1832e4cc3b0cbdec5671c2e4853d4d6de8f2f64a840cadcc" +
	"86b0c3e4853dad6c7853e762840cad1cb6e042ec043d2b6c3872e668b44cfd2c" +
	"e78082e0f43da50c7872664a84842d5c426b62716d6a67246b7622726a"

const loginATemplateHex = "" +
	"00000c0000000000000000000000000022020100943ea54961646d696e000000" +
	"0000000086d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c0451c1e4840db50ce8d2e640" +
	"878c2e8f86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0cc7d0c2e4e42d0d13e8d2e640" +
	"840cbd0c86d0c2e4843dad2c18d3e640840cad0c436860726c69"

const loginBTemplateHex = "" +
	"00200c0000000000000000000000000024020000953ea54961646d696e000000" +
	"0000000086d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c0451c1e4840db50ce8d2e640" +
	"878c2e8f86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0c86d0c2e4842dad0ce8d2e640" +
	"840cad0c86d0c2e4842dad0ce8d2e640840cad0cc7d0c2e4e42d0d13e8d2e640" +
	"840cbd0c86d0c2e4843dad2c18d3e640840cad0c436861736c696520"

var (
	dtlsTemplate   []byte
	loginATemplate []byte
	loginBTemplate []byte
)

func init() {
	dtlsTemplate = mustHex(dtlsTemplateHex)
	loginATemplate = mustHex(loginATemplateHex)
	loginBTemplate = mustHex(loginBTemplateHex)
	if len(dtlsTemplate) != 257 || len(loginATemplate) != 570 || len(loginBTemplate) != 572 {
		panic("petlibro: template length wrong")
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// buildDTLSBody returns a randomised DTLS-shaped first-packet body
// (257 B).  Camera-side parser only checks the fixed prefix/suffix
// pattern; the dynamic regions get fresh random bytes per session.
func buildDTLSBody(rand32 []byte) []byte {
	b := make([]byte, len(dtlsTemplate))
	copy(b, dtlsTemplate)
	copy(b[27:52], rand32[:25])
	copy(b[60:66], rand32[25:31])
	b[67] = rand32[31]
	return b
}

// buildLoginPair returns (loginA, loginB) inner bodies with a fresh
// 32-bit session seed at offset 0x14.  B uses seed+1.
func buildLoginPair(seed uint32) ([]byte, []byte) {
	a := make([]byte, len(loginATemplate))
	copy(a, loginATemplate)
	binary.LittleEndian.PutUint32(a[0x14:], seed)
	b := make([]byte, len(loginBTemplate))
	copy(b, loginBTemplate)
	binary.LittleEndian.PutUint32(b[0x14:], seed+1)
	return a, b
}
