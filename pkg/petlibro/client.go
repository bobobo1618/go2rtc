// Package petlibro is a clean-room LAN P2P client for Petlibro pet
// cameras (PLAF203 / PLAF103 / etc).  These cameras run a Kalay/TUTK
// firmware variant: same luffy crypto as Wyze (we reuse pkg/tutk's
// TransCodePartial/ReverseTransCodePartial directly) but with a
// different magic byte, direction byte, LOGIN structure and bootstrap
// IOCtrl sequence — see PROTOCOL_NOTES.txt at the repo root for
// the full byte-level docs.
//
// Pkg layout:
//
//	templates.go  — LOGIN A/B + DTLS-shaped templates + frame builders
//	client.go     — UDP session, handshake, bootstrap, frame reassembly
//	producer.go   — wraps the Client into a go2rtc Producer
package petlibro

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
)

// Codec IDs we surface to the producer (subset of pkg/tutk's table).
const (
	CodecH264    byte = 0x4E
	CodecAACADTS byte = 0x87
)

// Small frames (≤16 data fragments) end with a fragment whose fragIdx
// equals 16; longer frames overrun that and reuse fragIdx=16 as the
// final marker AGAIN.  So fragIdx alone can't separate "middle
// fragment that happened to land at index 16" from "end fragment".
// The reliable end-of-frame detector is the 15-byte ms-timestamp
// trailer (see stripFragmentMetadataTrailer) — 84 fixed bits of
// signature put coincidental matches in the 1-in-2^84 zone.

// Packet is one fully-assembled media frame from the camera.
type Packet struct {
	Codec      byte
	Payload    []byte
	Timestamp  uint32
	FrameNo    uint32
	IsKeyframe bool
}

// DialOptions configure a Petlibro session.
type DialOptions struct {
	UID        string
	Host       string // "ip" or "ip:32761"
	Password   string // default "888888" — encoded into the LOGIN template
	Audio      bool
	Quality    string // "hd" (default) or "sd"
	DisableSub bool   // experimental: try to make the camera stop dual-streaming
	Strict     bool   // drop the entire GOP if any fragment was lost — pristine pixels
	Verbose    bool
}

// Client is one LAN session against a Petlibro camera.
type Client struct {
	conn  *net.UDPConn
	cam   *net.UDPAddr
	uid   string
	nonce []byte

	kseq     uint16 // outer kalay seq (byte 6..7)
	icounter uint16 // inner cmd counter (byte 4..5)

	// in-order reassembly
	wrap          WrapSeq
	avBuffer      map[uint64]*pendingFrag
	avNextExt     uint64
	avHighExt     uint64
	avPrevSubWire uint16

	// per-stream frame accumulation
	curBuf      []byte
	curKeyframe bool

	// monotonic counters for outgoing Packets (camera's per-channel
	// frame_num is independent for main vs sub, so we use our own)
	emitSeq      uint32
	emitAudioSeq uint32
	startedAt    time.Time

	frames    chan *Packet
	closed    bool
	closeOnce sync.Once
	closeMu   sync.Mutex

	audio        bool
	quality      string
	disableSub   bool
	strict       bool
	verbose      bool
	mainAsm      channelAsm // per-channel assembly state for ch=0x05
	subAsm       channelAsm // per-channel assembly state for ch=0x07
	gopPoisoned  bool   // a fragment was lost in this GOP — drop P-frames until next clean IDR
	lastPFrameTs uint32 // last P-frame's trailer ts (kept for legacy callers)

	// Camera-clock PTS state.  pendingFrameTs is the millisecond value
	// extracted from the most recently seen metadata trailer; it
	// becomes the PTS for the next emitted AU.  firstFrameTs is the
	// first ts we saw, used to anchor the 90kHz PTS at 0.
	pendingFrameTs uint32
	havePendingTs  bool
	firstFrameTs   uint32
	haveFirstTs    bool
	lastEmitTs     uint32 // last PTS we emitted (in 90 kHz) — monotonic guard

	stats     counters
	prevStats counters

	dumpFile     *os.File // optional raw-stream dump for debugging
	emitDumpFile *os.File // optional emitted-AU dump for debugging
}

// counters holds running totals for a stream-health summary.
type counters struct {
	bytesIn      uint64 // bytes read from the UDP socket (encrypted)
	pktsIn       uint64 // UDP datagrams successfully read
	mainFrags    uint64 // fragments on channel 0x05 (HD)
	subFrags     uint64 // fragments on channel 0x07 (SD video)
	audioFrags   uint64 // fragments on channel 0x03
	otherFrags   uint64 // anything else (control, etc.)
	vidFrags     uint64 // video fragments (ch=0x05 + ch=0x07) reaching emit
	vidFramesIn  uint64 // distinct frame_num values seen on video channel
	vidFramesOut uint64 // AUs successfully queued to consumers
	vidDropped   uint64 // AUs discarded because a frag_idx gap was detected
	fragSkips    uint64 // count of frag_idx-skip events (fragments lost on the wire)
	fragsLost    uint64 // total fragments lost (sum of frag_idx gap sizes)
	forceDrains  uint64 // times forceDrain ran with buffered packets to flush
	recvTimeouts uint64 // SetReadDeadline-triggered timeouts (no UDP data)
	readerDrops  uint64 // raw UDP packets dropped because the processor queue was full
	emitDrops    uint64 // assembled AUs dropped because the consumer queue was full
}

type pendingFrag struct {
	channel    byte
	b1         byte
	isAudio    bool   // true for either audio variant (channels 0x03 or 0x07/b1=0x0d)
	subExt     uint64
	frameNum   uint32 // camera's per-channel frame counter (inner[28..31])
	fragIdx    uint16 // index of fragment within frame (inner[22..23])
	totalFrags byte   // total fragments comprising this frame (inner[20])
	payload    []byte
}

// channelAsm holds the assembly state for a single video channel.  The
// camera interleaves IDR fragments on ch=0x05 with P-frame fragments
// on ch=0x07 in wire order, so they MUST be assembled independently —
// a single shared buffer would constantly drop partial frames whenever
// the channels switch.
type channelAsm struct {
	buf           []byte
	curFrameNum   uint32
	curAUTotal    uint16 // inner[20]: total fragments advertised for current frame
	curAUDataCount uint16
	curAUGapped   bool
	expectFragIdx uint16
}

func (a *channelAsm) reset() {
	a.buf = a.buf[:0]
	a.curAUTotal = 0
	a.curAUDataCount = 0
	a.curAUGapped = false
	a.expectFragIdx = 0
}

// Dial opens a UDP socket, runs LAN_SEARCH3 + KNOCK2 + DTLS-shaped +
// LOGIN A/B + the 7-cmd Petlibro bootstrap, then starts the receive
// worker.  Returns once IPCAM_START has been sent and the AV-ready
// ack acknowledged — the camera will then stream video (and audio
// if --audio was requested).
func Dial(opts DialOptions) (*Client, error) {
	if opts.UID == "" {
		return nil, fmt.Errorf("petlibro: uid required")
	}
	if opts.Quality == "" {
		opts.Quality = "hd"
	}

	host := opts.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, fmt.Sprintf("%d", LANPort))
	}
	cam, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		return nil, fmt.Errorf("petlibro: resolve %s: %w", host, err)
	}
	udp, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("petlibro: bind: %w", err)
	}
	// HD video bursts > 3 Mbps; the default 768 KiB recv buffer is
	// only ~2 seconds of headroom and easily overflowed by a slow
	// drain.  Ask for 4 MiB — the kernel will clamp if it can't.
	_ = udp.SetReadBuffer(4 * 1024 * 1024)
	if opts.Verbose {
		// Verify what the kernel actually granted us — SetReadBuffer
		// silently clamps and we want to know if we hit a cap.
		if sc, err := udp.SyscallConn(); err == nil {
			var actualBuf int
			_ = sc.Control(func(fd uintptr) {
				actualBuf, _ = syscall.GetsockoptInt(int(fd),
					syscall.SOL_SOCKET, syscall.SO_RCVBUF)
			})
			fmt.Printf("[petlibro] SO_RCVBUF requested=%d granted=%d\n",
				4*1024*1024, actualBuf)
		}
	}

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		_ = udp.Close()
		return nil, err
	}
	c := &Client{
		conn:       udp,
		cam:        cam,
		uid:        opts.UID,
		nonce:      nonce,
		kseq:       2,
		audio:      opts.Audio,
		quality:    opts.Quality,
		disableSub: opts.DisableSub,
		strict:     opts.Strict,
		verbose:    opts.Verbose,
		frames:     make(chan *Packet, 1024),
	}
	if path := os.Getenv("PETLIBRO_DUMP_VIDEO"); path != "" {
		if f, ferr := os.Create(path); ferr == nil {
			c.dumpFile = f
		}
	}
	if path := os.Getenv("PETLIBRO_DUMP_EMITTED"); path != "" {
		if f, ferr := os.Create(path); ferr == nil {
			c.emitDumpFile = f
		}
	}

	if err := c.handshake(); err != nil {
		_ = udp.Close()
		return nil, err
	}
	if err := c.bootstrap(); err != nil {
		_ = udp.Close()
		return nil, err
	}

	go c.recvLoop()
	go c.maintenanceLoop()
	return c, nil
}

// --- I/O helpers (use pkg/tutk for the luffy crypto) ----------------------

func (c *Client) send(p []byte) error {
	_, err := c.conn.WriteToUDP(tutk.TransCodePartial(nil, p), c.cam)
	return err
}

func (c *Client) sendInner(body []byte) error {
	out := buildOuter(c.nonce, c.kseq, body, 0x00, 0x00, flagsSession)
	c.kseq = (c.kseq + 1) & 0xFFFF
	return c.send(out)
}

func (c *Client) recvOne(timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	}
	buf := make([]byte, 65535)
	n, addr, err := c.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	if c.cam.Port != addr.Port && addr.IP.Equal(c.cam.IP) {
		c.cam.Port = addr.Port
	}
	return tutk.ReverseTransCodePartial(nil, buf[:n]), nil
}

// --- handshake -----------------------------------------------------------

func (c *Client) handshake() error {
	logRecv := func(label string, pkt []byte) {
		if !c.verbose || len(pkt) < 12 {
			return
		}
		mt := binary.LittleEndian.Uint16(pkt[8:])
		flag := byte(0)
		if len(pkt) > 3 {
			flag = pkt[3]
		}
		var innerInfo string
		if len(pkt) >= 0x1C+0x1A {
			inner := pkt[0x1C:]
			innerInfo = fmt.Sprintf(" inner[0]=%02x inner[1]=%02x inner[24]=%02x",
				inner[0], inner[1], inner[0x18])
		}
		fmt.Printf("[petlibro/hs] %s: mt=0x%04x flags=0x%02x len=%d%s\n",
			label, mt, flag, len(pkt), innerInfo)
	}

	if err := c.send(buildLANSearch3(c.uid, c.nonce, 1)); err != nil {
		return err
	}
	gotLANSearchR := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pkt, err := c.recvOne(500 * time.Millisecond)
		if err != nil || len(pkt) < 12 {
			continue
		}
		logRecv("after-LAN_SEARCH3-w3=1", pkt)
		if binary.LittleEndian.Uint16(pkt[8:]) == msgLANSearchR {
			gotLANSearchR = true
			break
		}
	}
	if c.verbose && !gotLANSearchR {
		fmt.Printf("[petlibro/hs] WARNING: never received LAN_SEARCH_R from %s\n", c.cam)
	}

	_ = c.send(buildLANSearch3(c.uid, c.nonce, 2))
	time.Sleep(30 * time.Millisecond)
	_ = c.send(buildKnock2(c.uid, c.nonce))
	gotKnockRR2 := false
	deadline = time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pkt, err := c.recvOne(500 * time.Millisecond)
		if err != nil || len(pkt) < 12 {
			continue
		}
		logRecv("after-KNOCK2", pkt)
		if binary.LittleEndian.Uint16(pkt[8:]) == msgKnockRR2 {
			gotKnockRR2 = true
			break
		}
	}
	if c.verbose && !gotKnockRR2 {
		fmt.Printf("[petlibro/hs] WARNING: never received KNOCK_RR2 from %s\n", c.cam)
	}

	// DTLS-shaped first packet (datatype=1, kseq=0)
	rand32 := make([]byte, 32)
	_, _ = rand.Read(rand32)
	dtlsBody := buildDTLSBody(rand32)
	if err := c.send(buildOuter(c.nonce, 0, dtlsBody, 0x01, 0x00, flagsSession)); err != nil {
		return err
	}

	// LOGIN A + LOGIN B (fresh random seed, B = seed+1)
	var seedBytes [4]byte
	_, _ = rand.Read(seedBytes[:])
	seedBytes[0] &= 0xFE
	seed := binary.LittleEndian.Uint32(seedBytes[:])
	loginA, loginB := buildLoginPair(seed)
	if err := c.send(buildOuter(c.nonce, 0, loginA, 0x00, 0x00, flagsSession)); err != nil {
		return err
	}
	if err := c.send(buildOuter(c.nonce, 1, loginB, 0x00, 0x00, flagsSession)); err != nil {
		return err
	}

	// Wait for LOGIN_RESP — outer mt=0x408, inner[1]==0x21, inner[0x18]==0
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pkt, err := c.recvOne(500 * time.Millisecond)
		if err != nil || len(pkt) < 0x1C+0x1A {
			continue
		}
		logRecv("after-LOGIN", pkt)
		if binary.LittleEndian.Uint16(pkt[8:]) != msgSessionD2C {
			continue
		}
		inner := pkt[0x1C:]
		if inner[1] == 0x21 && inner[0x18] == 0 {
			return nil
		}
	}
	return fmt.Errorf("petlibro: LOGIN_RESP timeout")
}

// --- bootstrap -----------------------------------------------------------

func (c *Client) bootstrap() error {
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
		{0x7000, ioctlBody12(IOCtrlVendor0372)},
		{0x7000, ioctlBody12(IOCtrlVendor0322)},
		{0x7000, ioctlBody12(IOCtrlVendor032A)},
		{0x7000, ioctlBody12(IOCtrlStart)},
	}
	if c.disableSub {
		// Experimental: try to ask the camera to stop dual-streaming
		// by sending a separate sub-channel SETSTREAMCTRL.
		cmds = append([]cmd{{0x1000, disableSubProbe}}, cmds...)
	}
	if c.audio {
		// The app enables audio AFTER IPCAM_START via a separate
		// 0x0300 AUDIO_ENABLE; do not also pack it into IPCAM_START.
		audioOn := ioctlBody12(IOCtrlAudioOn)
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

		_ = c.conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
		for {
			buf := make([]byte, 65535)
			n, _, err := c.conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
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

	// 3. AV-ready ack
	if err := c.sendInner(innerAck(c.icounter, 0x3FFF, bootstrapAVMax, 3, 0x34, tick16())); err != nil {
		return err
	}
	c.icounter++

	c.avPrevSubWire = bootstrapAVMax
	if bootstrapAVMax == 0x3FFF {
		c.wrap = WrapSeq{Ext: 0x4000}
	} else {
		c.wrap = WrapSeq{Ext: uint64(bootstrapAVMax) + 1}
	}
	c.avNextExt = c.wrap.Ext
	c.avHighExt = c.wrap.Ext - 1
	c.avBuffer = make(map[uint64]*pendingFrag)
	return nil
}

// --- worker loops --------------------------------------------------------

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
		c.closeMu.Lock()
		done := c.closed
		c.closeMu.Unlock()
		if done {
			return
		}

		select {
		case raw, ok := <-rawChan:
			if !ok {
				return
			}
			c.stats.bytesIn += uint64(len(raw))
			c.stats.pktsIn++
			c.handleIncoming(tutk.ReverseTransCodePartial(nil, raw))
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
		c.closeMu.Lock()
		done := c.closed
		c.closeMu.Unlock()
		if done {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		buf := make([]byte, 65535)
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				c.stats.recvTimeouts++
				continue
			}
			return
		}
		if n == 0 {
			continue
		}
		select {
		case out <- buf[:n]:
		default:
			// Processor is behind by 1024 packets.  Drop the new
			// one rather than block the reader and let the kernel
			// drop instead — the inner cmd counter / frag_idx gap
			// detection will mark the resulting AU as incomplete.
			c.stats.readerDrops++
		}
	}
}

// dumpStats prints a one-line stream-health summary every ~5 s when
// the client is in verbose mode.  Numbers cover the most recent
// interval; the cumulative counters are also visible.
func (c *Client) dumpStats() {
	s := &c.stats
	delta := *s
	if c.prevStats.bytesIn > 0 {
		delta = counters{
			bytesIn:      s.bytesIn - c.prevStats.bytesIn,
			pktsIn:       s.pktsIn - c.prevStats.pktsIn,
			mainFrags:    s.mainFrags - c.prevStats.mainFrags,
			subFrags:     s.subFrags - c.prevStats.subFrags,
			audioFrags:   s.audioFrags - c.prevStats.audioFrags,
			otherFrags:   s.otherFrags - c.prevStats.otherFrags,
			vidFrags:     s.vidFrags - c.prevStats.vidFrags,
			vidFramesIn:  s.vidFramesIn - c.prevStats.vidFramesIn,
			vidFramesOut: s.vidFramesOut - c.prevStats.vidFramesOut,
			vidDropped:   s.vidDropped - c.prevStats.vidDropped,
			fragSkips:    s.fragSkips - c.prevStats.fragSkips,
			fragsLost:    s.fragsLost - c.prevStats.fragsLost,
			forceDrains:  s.forceDrains - c.prevStats.forceDrains,
			recvTimeouts: s.recvTimeouts - c.prevStats.recvTimeouts,
			readerDrops:  s.readerDrops - c.prevStats.readerDrops,
			emitDrops:    s.emitDrops - c.prevStats.emitDrops,
		}
	}
	fmt.Printf("[petlibro/stats] in=%d pkts (%d KiB) channels: main=%d sub=%d audio=%d other=%d | video: %d frames in -> %d out (drop %d) | frag skips: %d (%d frags lost) | forceDrain: %d | qDrops reader=%d emit=%d\n",
		delta.pktsIn, delta.bytesIn/1024,
		delta.mainFrags, delta.subFrags, delta.audioFrags, delta.otherFrags,
		delta.vidFramesIn, delta.vidFramesOut, delta.vidDropped,
		delta.fragSkips, delta.fragsLost, delta.forceDrains,
		delta.readerDrops, delta.emitDrops)
	c.prevStats = *s
}

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
		c.closeMu.Lock()
		done := c.closed
		c.closeMu.Unlock()
		if done {
			return
		}
		select {
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

func isTimeout(err error) bool {
	type t interface{ Timeout() bool }
	if e, ok := err.(t); ok {
		return e.Timeout()
	}
	return false
}

// --- packet classification + frame assembly ------------------------------

func (c *Client) handleIncoming(pkt []byte) {
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
		c.stats.mainFrags++
	case innerChSub:
		c.stats.subFrags++
	case innerChAudio:
		c.stats.audioFrags++
	default:
		c.stats.otherFrags++
	}
	// paylen at bytes 24..25 covers the actual payload bytes after offset
	// 36; the camera often pads packets with trailing zeros, and feeding
	// those into the stream synthesises spurious H.264 start codes that
	// the decoder rejects.  Only use paylen when it's non-zero AND fits.
	paylen := binary.LittleEndian.Uint16(inner[24:])
	sliceWithPaylen := func(start int) []byte {
		if paylen != 0 && start+int(paylen) <= len(inner) {
			return inner[start : start+int(paylen)]
		}
		return inner[start:]
	}

	var (
		isAV    bool
		isAudio bool
		payload []byte
	)
	switch {
	case (channel == innerChMain || channel == innerChSub) &&
		(b1 == 0x00 || b1 == 0x04 || b1 == 0x05):
		isAV = true
		payload = sliceWithPaylen(36)
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
		isAV = true
		payload = sliceWithPaylen(36)
	case channel == innerChAudio && sub17 == 0x01 &&
		len(inner) >= 38 && inner[36] == 0xFF && inner[37] == 0xF1:
		// Audio variant (b): ADTS at offset 36, channel 0x03.
		isAudio = true
		payload = sliceWithPaylen(36)
	case channel == innerChSub && sub17 == 0x00 && b1 == 0x0d &&
		len(inner) >= 46 && inner[44] == 0xFF && inner[45] == 0xF1:
		// Audio variant (a): 8-byte inner header before ADTS sync,
		// carried on sub_video channel — see PROTOCOL_NOTES.txt.
		// Strip the 8 header bytes so the producer sees clean ADTS.
		isAudio = true
		full := sliceWithPaylen(36)
		if len(full) > 8 {
			payload = full[8:]
		} else {
			payload = nil
		}
	default:
		return
	}
	_ = isAV
	_ = isAudio

	// (No subWire filter — Python accepts AV/audio packets at any wire
	// value.  Filtering to >= 0x4000 dropped legitimate audio fragments.)
	subExt := c.wrap.Extend(subWire)
	if subExt > c.avHighExt {
		c.avHighExt = subExt
		c.wrap.AdvanceTo(subWire)
	}
	if subExt < c.avNextExt {
		return // late dup
	}
	var frameNum uint32
	var fragIdx uint16
	if len(inner) >= 32 {
		frameNum = binary.LittleEndian.Uint32(inner[28:])
	}
	if len(inner) >= 24 {
		fragIdx = binary.LittleEndian.Uint16(inner[22:])
	}
	var totalFrags byte
	if len(inner) >= 21 {
		totalFrags = inner[20] // PROTOCOL_NOTES: total fragments in this frame
	}
	c.avBuffer[subExt] = &pendingFrag{
		channel: channel, b1: b1, isAudio: isAudio,
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

func (c *Client) forceDrain() {
	if len(c.avBuffer) == 0 {
		return
	}
	threshold := c.avHighExt
	if threshold < 8 {
		return
	}
	threshold -= 8

	var keys []uint64
	for k := range c.avBuffer {
		if k <= threshold {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	c.stats.forceDrains++
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for _, k := range keys {
		e := c.avBuffer[k]
		delete(c.avBuffer, k)
		c.avNextExt = k + 1
		c.emit(e)
	}
}

// emit handles one in-order entry.  Frame structure on the wire:
//
//   * 0..N-1 "data" fragments containing slice bytes, paylen=1024 each
//     (except possibly the last one); no trailer.
//   * 1 final "end" fragment with a smaller paylen, ending in the
//     16-byte metadata block (codec_id 0x4e + 15-byte ms-ts trailer).
//
// The end fragment is identified by the trailer signature on its
// payload tail, NOT by fragIdx (which can wrap when N>16 and reuse
// "16" both as a data index and the end marker).  inner[20] gives the
// total fragment count, so loss of the trailing data fragment — which
// fragIdx gaps cannot see — is also detectable.
//
// CRITICAL: the camera time-multiplexes IDR fragments on ch=0x05 with
// P-frame fragments on ch=0x07 in WIRE ORDER — e.g. mid-IDR a P-frame
// can arrive.  Each channel MUST have its own assembly buffer or the
// constant frame_num switching would drop every partial IDR.  Both
// channels' completed frames are emitted into one PTS-ordered output
// stream though, since they share the camera's frame_num counter.
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
	c.stats.vidFrags++

	// Camera concurrently sends two video streams (HD = stream_id 0x01,
	// SD = stream_id 0x02) over ch=0x05 (IDR data) + ch=0x07 (P-frames).
	// They interleave in wire order; if we don't filter one out the
	// downstream decoder gets a mix and renders the wrong-resolution
	// frames with scattered MB-level decoder errors.
	//
	// Filter strategy:
	//
	// * ch=0x05 IDR fragments don't carry a per-fragment stream id
	//   (only the end-fragment's trailer does).  Use inner[20]
	//   (total fragment count) as a size-based discriminator:
	//   ~12-16 frags = SD, ~70-78 frags = HD.  Threshold = 30.
	//
	// * ch=0x07 P-frames are single-fragment with a trailer; check
	//   the trailer's stream id byte directly.
	wantStreamID := byte(0x01) // HD = main stream by default
	if c.quality == "sd" {
		wantStreamID = 0x02 // SD = sub stream
	}
	// (no totalFrags-based HD/SD filter on ch=0x05 anymore — the
	// fragment-count discriminator (>=30 = HD) only worked while the
	// camera was running in dual-stream mode where HD IDRs were ~78
	// fragments and parallel SD IDRs were ~12.  In HD-only mode, HD
	// IDRs drop to ~9 fragments and the >=30 threshold filters them
	// all out, leaving the stream stuck.  When dual-stream mode IS
	// active, the SD end-fragment's trailer stream-id discriminator
	// at the in-emit() end-fragment path still handles it.)
	// ch=0x07 P-frame: filter by trailer stream-id byte.  The camera
	// dual-streams regardless of SETSTREAMCTRL — feeding HD-encoded
	// P-frames into an SD decoder produces top-row mb_type / cbp
	// errors and is semantically wrong (the consumer asked for a
	// 360p stream).  Accept only the matching stream.
	//
	// Trailer byte layout for ch=0x07 P-frames (single fragment):
	//   p[-16] = 0x4e codec_id
	//   p[-15] = 0x00
	//   p[-14] = 0x00 (P-frame; 0x01 for IDR — not seen on ch=0x07)
	//   p[-13] = 0x00
	//   p[-12] = stream-id (0x01 HD / 0x02 SD)
	//
	// Note: SD P-frames are rare on this firmware (~10% of ch=0x07
	// traffic), so SD playback is closer to a slideshow than 25 fps
	// video.  That's a camera-side encoding choice — the camera only
	// emits an SD P-frame when there's actual motion to encode.
	if e.channel == innerChSub && len(e.payload) >= 16 &&
		e.payload[len(e.payload)-16] == CodecH264 &&
		e.payload[len(e.payload)-12] != wantStreamID {
		// Filter out the wrong-stream P-frame and DO NOT use it
		// to flush the pending main-stream IDR.  When the camera
		// dual-streams, a wrong-stream ch=0x07 P-frame can arrive
		// on the wire BETWEEN two of our stream's IDR fragments
		// (e.g. between frag 73 and frag 75).  Flushing here
		// truncated the IDR at its last 1–2 fragments which carry
		// the bottom MB rows, producing the "corrupted macroblock
		// X 66 / X 67" pattern in mpv on every frame.
		//
		// The matching-stream ch=0x07 branch below still flushes,
		// and the camera sends those at full frame rate (~15-25
		// fps), so the IDR sits at most one P-frame interval
		// (~40-66 ms) longer than before.  Worst-case fallback:
		// the next IDR's first ch=0x05 fragment flushes via the
		// new-frame-num check immediately below.
		return
	}

	// This firmware variant:
	//   ch=0x05 IDR fragments arrive WITHOUT a trailer-bearing end
	//     fragment.  All ~16 (small) or ~75 (big) data fragments come
	//     in order, then the next thing on the wire is a ch=0x07
	//     P-frame.  The arrival of a ch=0x07 fragment signals that
	//     the preceding ch=0x05 IDR is complete.
	//   ch=0x07 P-frames are single-fragment, each with a trailer.
	//
	// Older firmware (PCAPdroid pcap) DID put an end fragment with
	// trailer on ch=0x05 IDRs.  The code handles both: detect end via
	// trailer signature if present, otherwise flush the IDR on the
	// next ch=0x07 arrival.

	stripped, frameTs, hasTrailer := stripFragmentMetadataTrailer(e.payload)
	if c.dumpFile != nil {
		if hasTrailer {
			_, _ = c.dumpFile.Write(stripped)
		} else {
			_, _ = c.dumpFile.Write(e.payload)
		}
	}

	asm := &c.mainAsm
	if e.channel == innerChSub {
		asm = &c.subAsm
	}

	// Frame_num change on THIS channel without a clean end fragment.
	// For ch=0x05 this is back-to-back IDRs (the previous IDR's
	// end-fragment was lost AND no ch=0x07 P-frame came in between).
	// Treat it the same as a cross-channel flush — try to emit the
	// previous IDR rather than silently drop it.  This matters most
	// when filtering ch=0x07 (e.g. quality=sd where every HD P-frame
	// is filtered out), since cross-channel flush would otherwise
	// never fire.
	if asm.curFrameNum != 0 && e.frameNum != asm.curFrameNum && len(asm.buf) > 0 {
		if e.channel == innerChMain {
			c.flushMainIDR(0)
		} else {
			c.stats.fragSkips++
			c.stats.fragsLost++
			c.stats.vidDropped++
			if c.strict {
				c.gopPoisoned = true
			}
			asm.reset()
		}
	}
	if e.frameNum != asm.curFrameNum {
		asm.curFrameNum = e.frameNum
		c.stats.vidFramesIn++
		asm.expectFragIdx = 0
		asm.curAUGapped = false
		asm.curAUDataCount = 0
		asm.curAUTotal = uint16(e.totalFrags)
	}
	if asm.curAUTotal == 0 && e.totalFrags != 0 {
		asm.curAUTotal = uint16(e.totalFrags)
	}

	// ch=0x07 arrival: flush any pending ch=0x05 IDR first (its end
	// fragment never came on this firmware), then process this P-frame.
	if e.channel == innerChSub && len(c.mainAsm.buf) > 0 {
		c.flushMainIDR(frameTs)
	}

	if !hasTrailer {
		// Data fragment.  fragIdx==16 is exempt from gap-detection
		// because the camera reuses it as a position index when N>16.
		if e.fragIdx != asm.expectFragIdx && e.fragIdx != 16 {
			c.stats.fragSkips++
			if e.fragIdx > asm.expectFragIdx {
				c.stats.fragsLost += uint64(e.fragIdx - asm.expectFragIdx)
			} else {
				c.stats.fragsLost++
			}
			asm.curAUGapped = true
		}
		asm.expectFragIdx = e.fragIdx + 1
		asm.curAUDataCount++
		asm.buf = append(asm.buf, e.payload...)
		return
	}

	// End-of-frame fragment.  Append trailer-stripped payload and emit.
	asm.buf = append(asm.buf, stripped...)
	expectedData := uint16(0)
	if asm.curAUTotal > 0 {
		expectedData = asm.curAUTotal - 1
	}
	if asm.curAUDataCount < expectedData {
		lost := uint64(expectedData - asm.curAUDataCount)
		c.stats.fragSkips++
		c.stats.fragsLost += lost
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
		c.stats.fragSkips++
		c.stats.fragsLost += uint64(expectedData)
		c.stats.vidDropped++
		asm.reset()
		asm.curFrameNum = 0
		if c.strict && e.channel == innerChMain {
			c.gopPoisoned = true
		}
		return
	}

	au := append([]byte(nil), asm.buf...)
	gapped := asm.curAUGapped
	wasMain := e.channel == innerChMain
	asm.reset()
	asm.curFrameNum = 0

	if gapped {
		// Strict mode: drop the entire GOP for pristine pixels.
		if c.strict {
			c.gopPoisoned = true
			c.stats.vidDropped++
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
		c.stats.vidDropped++
		if e.channel != innerChMain {
			return
		}
	}
	c.pendingFrameTs = frameTs
	c.havePendingTs = true
	if !wasMain {
		c.lastPFrameTs = frameTs
	}
	// In strict mode only, drop P-frames in a poisoned GOP until the
	// next clean IDR.  In non-strict (default), let them through.
	if c.strict && c.gopPoisoned && !wasMain {
		c.stats.vidDropped++
		return
	}
	c.emitAU(au)
}

// flushMainIDR emits the accumulated ch=0x05 IDR buffer when a
// ch=0x07 P-frame arrives without us having seen the IDR's
// end-fragment.  The IDR's last ~300 bytes (the bottom MB rows on
// HD) are in that end-fragment, so the slice we have is truncated.
//
// Behaviour mirrors pkg/tutk's handleVideo discipline:
//   * if a MID-frame fragment was lost (fragIdx gap) — DROP, because
//     the slice has a hole somewhere in the middle and decoder errors
//     scatter across the whole frame
//   * if only the trailing end-fragment was lost (no mid gaps) —
//     emit in non-strict mode, drop in strict mode.  ffmpeg tolerates
//     a slice truncated at the very end with minor MB row 66-67
//     artifacts; the alternative is a multi-second video gap.
func (c *Client) flushMainIDR(nextPFrameTs uint32) {
	if len(c.mainAsm.buf) == 0 {
		c.mainAsm.reset()
		c.mainAsm.curFrameNum = 0
		return
	}
	au := append([]byte(nil), c.mainAsm.buf...)
	midGapped := c.mainAsm.curAUGapped
	tailMissing := false
	if c.mainAsm.curAUTotal > 0 && c.mainAsm.curAUDataCount+1 < c.mainAsm.curAUTotal {
		tailMissing = true
		c.stats.fragSkips++
		c.stats.fragsLost += uint64(c.mainAsm.curAUTotal - 1 - c.mainAsm.curAUDataCount)
	}
	c.mainAsm.reset()
	c.mainAsm.curFrameNum = 0
	if c.strict && (midGapped || tailMissing) {
		// Strict mode: drop the IDR if anything was lost — pristine
		// pixels over fluency.  Cascading inter-frame errors are
		// avoided by also poisoning the GOP so subsequent P-frames
		// are dropped until the next clean IDR.
		c.gopPoisoned = true
		c.stats.vidDropped++
		return
	}
	if midGapped {
		// Non-strict: emit the partial IDR anyway.  ffmpeg will show
		// macroblock artefacts at the gap location for ~50 P-frames
		// until the next clean IDR arrives — visibly noisy but vastly
		// better than the multi-second video freeze that dropping the
		// IDR would produce.
		c.stats.vidDropped++
	}
	_ = tailMissing
	if nextPFrameTs != 0 {
		idrTs := nextPFrameTs - 40
		c.pendingFrameTs = idrTs
		c.havePendingTs = true
	}
	c.emitAU(au)
}

// emitAU finalises one access unit and queues it for the consumer.
func (c *Client) emitAU(au []byte) {
	if len(au) < 5 {
		return
	}
	isKey := containsNALType(au, 5)
	if c.emitDumpFile != nil {
		_, _ = c.emitDumpFile.Write(au)
	}
	if isKey {
		// A fresh IDR clears the GOP-poisoned state.
		c.gopPoisoned = false
	} else if c.strict && c.gopPoisoned {
		// Strict only: drop P-frames referencing a dropped IDR.
		// Non-strict lets the decoder conceal.
		c.stats.vidDropped++
		return
	}

	// PTS comes from the camera's own millisecond clock embedded in
	// the metadata trailer of each frame's last fragment.  This gives
	// per-frame-accurate timestamps that survive forceDrain bursts —
	// wall-clock derived PTS produced "Invalid video timestamp X -> X"
	// duplicates in mpv because multiple AUs flushed within one ms.
	var pts uint32
	if c.havePendingTs {
		if !c.haveFirstTs {
			c.firstFrameTs = c.pendingFrameTs
			c.haveFirstTs = true
		}
		// Frame counter is u32 LE ms; subtract origin and convert to
		// the H.264 90 kHz clock.  Wrap-safe via unsigned subtraction.
		ms := c.pendingFrameTs - c.firstFrameTs
		pts = ms * 90
		c.havePendingTs = false
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
	c.stats.vidFramesOut++
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
// The camera concurrently sends BOTH streams; the configured Quality
// option picks which one to keep — see streamIDFromTrailer() and the
// filter at the top of emit().
//
// Stripping only 15 trailer bytes would leave the 0x4e codec_id in
// the slice tail; decoders read it as a stray NAL-14 prefix and bail
// on the next frame with "mb_skip_run invalid at MB 0,0".  Strip all
// 16 bytes.
//
// Callers should only invoke this on the frame's end fragment (whose
// tail unambiguously matches the signature) — in mid-frame fragments
// a coincidental match could shear real slice bytes.
func stripFragmentMetadataTrailer(p []byte) (stripped []byte, ts uint32, hasTs bool) {
	if len(p) < 16 {
		return p, 0, false
	}
	t := p[len(p)-15:] // 15-byte trailer right after the codec_id byte
	prefixOK := t[0] == 0x00 && (t[1] == 0x00 || t[1] == 0x01) &&
		t[2] == 0x00 && t[3] >= 0x01 && t[3] <= 0x0f
	zerosOK := t[4] == 0 && t[5] == 0 && t[6] == 0 && t[7] == 0 &&
		t[8] == 0 && t[9] == 0 && t[10] == 0
	codecIDOK := p[len(p)-16] == CodecH264
	if prefixOK && zerosOK && codecIDOK {
		ts = binary.LittleEndian.Uint32(t[11:15])
		return p[:len(p)-16], ts, true
	}
	return p, 0, false
}

// containsNALType reports whether the Annex-B buffer contains any NAL
// unit of the given H.264 NAL type.
func containsNALType(b []byte, want byte) bool {
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

func (c *Client) queuePacket(p *Packet) {
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()
	if closed {
		return
	}
	defer func() { _ = recover() }() // tolerate races with Close()
	select {
	case c.frames <- p:
	default:
		c.stats.emitDrops++
	}
}

// --- public surface ------------------------------------------------------

// ReadPacket blocks until a Packet is available or the connection closes.
func (c *Client) ReadPacket() (*Packet, error) {
	p, ok := <-c.frames
	if !ok {
		return nil, io.EOF
	}
	return p, nil
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closed = true
		c.closeMu.Unlock()
		_ = c.conn.Close()
		close(c.frames)
	})
	return nil
}

func (c *Client) RemoteAddr() net.Addr { return c.cam }
func (c *Client) Protocol() string     { return "petlibro+udp" }
func (c *Client) Audio() bool          { return c.audio }

func (c *Client) SetDeadline(t time.Time) error {
	if c.conn == nil {
		return nil
	}
	return c.conn.SetReadDeadline(t)
}

// WrapSeq tracks a monotonic 64-bit counter that follows a 16-bit wire
// counter through wrap-arounds.  Single-step jumps >0x8000 are treated
// as wraps.  Public so producer.go can inspect/test.
type WrapSeq struct {
	Ext uint64
}

func (w *WrapSeq) Extend(wire uint16) uint64 {
	lastWire := uint16(w.Ext)
	fwd := (wire - lastWire) & 0xFFFF
	if fwd < 0x8000 {
		return w.Ext + uint64(fwd)
	}
	back := uint64((lastWire - wire) & 0xFFFF)
	if back > w.Ext {
		return 0
	}
	return w.Ext - back
}

func (w *WrapSeq) AdvanceTo(wire uint16) {
	if newExt := w.Extend(wire); newExt > w.Ext {
		w.Ext = newExt
	}
}
