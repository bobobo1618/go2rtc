package petlibro

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
)

// bootstrap runs the 7-IOCtrl post-LOGIN sequence that primes the
// camera to start streaming: notice/ack/heartbeat, then SETSTREAMCTRL +
// vendor queries (0x0372/0x0322/0x032A) + IPCAM_START + (optional)
// AUDIO_ENABLE, with per-iteration ack collection, ending with the
// AV-ready ack that switches the camera into AV-streaming mode and
// initialises the wrap-counter state for the assembler.
//
// petlibro divergence vs pkg/tutk: tutk's equivalent post-login work
// is split between WriteCommand (pkg/tutk/conn.go:116, with 1s/5x
// retry on a single command) and Session16.SendIOCtrl
// (pkg/tutk/session16.go:113, builds a 0x00 0x70 control msg).  Petlibro
// instead drives a fixed 5- or 6-command sequence with a 40 ms ack
// window per command (vs tutk's blocking 1s+retry model) and
// terminates with a custom AV-ready 09-ack containing
// (av_prev=0x3FFF, av_curr=bootstrapAVMax, chan_idx=3, sub=0x34) — no
// tutk equivalent.  The channel IDs (0x1000 for SETSTREAMCTRL,
// 0x7000 for vendor + IPCAM_START) are petlibro-specific.
func (c *Client) bootstrap() error {
	// tick32 returns ms-since-bootstrap-start with a 0xC000 (49152)
	// offset baked in.  Every outbound 32-bit tick field observed in
	// captures of the official Petlibro app carries this offset
	// above the session-relative monotonic clock — matches the SDK
	// convention of reserving the low 0xC000 range for control-
	// channel tick values that the camera firmware special-cases
	// (heartbeat acks, notice flushes).  Replicating it keeps the
	// camera's parser on its happy path.  tick16 is the wall-clock
	// low 16 bits and does NOT carry the offset.
	tickBase := time.Now().UnixMilli()
	tick32 := func() uint32 { return uint32((time.Now().UnixMilli() - tickBase + 0xC000) & 0xFFFFFFFF) }
	tick16 := func() uint16 { return uint16(time.Now().UnixMilli() & 0xFFFF) }

	// 1. notice + 09-ack of LOGIN_RESP + heartbeat
	if err := c.sendInner(innerNotice(0, 0x1F, tick32(), 4)); err != nil {
		return err
	}
	if err := c.sendInner(innerAck(c.icounter, 0xFFFF, 0xFFFF, 1, 0, tick16())); err != nil {
		return err
	}
	c.icounter++
	if err := c.sendInner(innerHeartbeat(c.icounter, tick32())); err != nil {
		return err
	}
	c.icounter++

	// 2. IOCtrl bootstrap commands — match the official Petlibro app's
	// sequence (verified against PCAPdroid_22_May_08_31_19.pcap):
	//
	//	1. SETSTREAMCTRL HD     (chan=0x1000, chan=1 type=0x3fff)
	//	2. Vendor 0x0372        (chan=0x7000)
	//	3. GET_AUDIO_OUT_FORMAT (chan=0x7000, IOCtrl 0x0322)
	//	4. GET_FORMAT           (chan=0x7000, IOCtrl 0x032A)
	//	5. IPCAM_START          (chan=0x7000, body[4]=0)
	//	6. (optional) AUDIO_ENABLE if audio=true
	//
	// Previous bootstrap had two extra 0x0000 channel-init commands
	// and sent SETSTREAMCTRL after the vendor cmds rather than first.
	// On a real HD-capable camera this resulted in the camera dual-
	// streaming HD + SD with the SD IDR sometimes winning probe and
	// breaking decoding.
	stream := qualityHD
	if c.quality == "sd" {
		stream = qualitySD
	}
	type cmd struct {
		chanHi  uint16
		payload []byte
	}
	cmds := []cmd{
		{0x1000, stream},
		{0x7000, ioctlBody12(ioctlVendor0372)},
		{0x7000, ioctlBody12(ioctlVendor0322)},
		{0x7000, ioctlBody12(ioctlVendor032A)},
		{0x7000, ioctlBody12(ioctlStart)},
	}
	if c.audio {
		// The app enables audio AFTER IPCAM_START via a separate
		// 0x0300 AUDIO_ENABLE; do not also pack it into IPCAM_START.
		audioOn := ioctlBody12(ioctlAudioOn)
		audioOn[4] = 0x01
		cmds = append(cmds, cmd{0x7000, audioOn})
	}

	var bootstrapAVMax uint16 = 0x3FFF
	pendingAck := []uint16{}
	var maxChanBit uint32 = 1
	for i, cm := range cmds {
		if err := c.sendInner(innerData(c.icounter, cm.chanHi, uint16(i), cm.payload)); err != nil {
			return err
		}
		c.icounter++

		// 40 ms per-IOCtrl ack window.  Calibrated against PLAF203 on
		// 2.4 GHz Wi-Fi where the camera typically responds in
		// 8-25 ms; long-tail responses past 40 ms get caught by the
		// next iteration's pendingAck loop or by the maintenanceLoop
		// ack retransmits, so the trade-off is "occasional ack-drop
		// & retransmit" vs "longer stream-start latency".  Don't
		// raise without re-measuring the camera's response
		// distribution.
		_ = c.conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
		for {
			buf := make([]byte, 65535)
			n, _, err := c.conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			// petlibro divergence vs pkg/tutk: tutk's Conn.Read at
			// pkg/tutk/conn.go:69 also calls ReverseTransCodePartial on
			// every datagram, but petlibro bypasses Conn.Read entirely
			// during bootstrap so that the 40 ms ack-window deadline
			// can apply to a raw UDP read rather than the
			// reader-goroutine path used post-bootstrap.
			pkt := tutk.ReverseTransCodePartial(nil, buf[:n])
			if len(pkt) < 0x1C+20 {
				continue
			}
			if binary.LittleEndian.Uint16(pkt[8:]) != msgSessionD2C {
				continue
			}
			inner := pkt[0x1C:]
			if inner[0] != 0x0c {
				continue
			}
			csub := binary.LittleEndian.Uint16(inner[18:])
			chanHi := binary.LittleEndian.Uint16(inner[16:])
			if csub < 0x4000 {
				pendingAck = append(pendingAck, csub)
				switch chanHi {
				case 0x1000:
					if maxChanBit < 1 {
						maxChanBit = 1
					}
				case 0x7000:
					if maxChanBit < 2 {
						maxChanBit = 2
					}
				}
			} else if csub > bootstrapAVMax {
				bootstrapAVMax = csub
			}
		}
		for _, s := range pendingAck {
			if err := c.sendInner(innerAck(c.icounter, 0xFFFF, 0xFFFF, maxChanBit, s, tick16())); err != nil {
				return err
			}
			c.icounter++
		}
		pendingAck = pendingAck[:0]
	}

	// 3. AV-ready ack.  Validate that we actually observed the camera
	// advance past the 0x3FFF control-channel watermark; if we never
	// saw an AV-channel ACK (csub > 0x3FFF) above, bootstrapAVMax
	// would still be the 0x3FFF default and the maintenance loop
	// would never trigger its high-water ACK loop (gated by
	// c.avHighExt >= 0x4000).  That's a silent stuck-stream failure
	// mode worth catching at bootstrap time.
	if bootstrapAVMax < 0x3FFF {
		return fmt.Errorf("petlibro: bootstrap got AVMax=0x%04x, want >=0x3FFF — camera didn't ack any AV channel", bootstrapAVMax)
	}
	if err := c.sendInner(innerAck(c.icounter, 0x3FFF, bootstrapAVMax, 3, 0x34, tick16())); err != nil {
		return err
	}
	c.icounter++

	c.avPrevSubWire = bootstrapAVMax
	if bootstrapAVMax == 0x3FFF {
		c.wrap = wrapSeq{ext: 0x4000}
	} else {
		c.wrap = wrapSeq{ext: uint64(bootstrapAVMax) + 1}
	}
	c.avNextExt = c.wrap.ext
	c.avHighExt = c.wrap.ext - 1
	c.avBuffer = make(map[uint64]*pendingFrag)
	return nil
}
