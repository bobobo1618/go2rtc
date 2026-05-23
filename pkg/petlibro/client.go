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

	audio           bool
	quality         string
	disableSub      bool
	verbose         bool
	videoChannel    byte   // inner-channel byte of the video stream we consume
	curFrameNum     uint32 // camera frame counter of the AU currently being assembled
	curAUIncomplete  bool   // true if a frag_idx gap occurred during the current AU
	curAUWasKeyframe bool   // current AU begins with SPS (drop on loss to avoid GOP poison)
	expectFragIdx    uint16 // next frag_idx we expect within the current frame

	stats     counters
	prevStats counters

	dumpFile *os.File // optional raw-stream dump for debugging
}

// counters holds running totals for a stream-health summary.
type counters struct {
	bytesIn      uint64 // bytes read from the UDP socket (encrypted)
	pktsIn       uint64 // UDP datagrams successfully read
	mainFrags    uint64 // fragments on channel 0x05 (HD)
	subFrags     uint64 // fragments on channel 0x07 (SD video)
	audioFrags   uint64 // fragments on channel 0x03
	otherFrags   uint64 // anything else (control, etc.)
	vidFrags     uint64 // video fragments (matching videoChannel) reaching emit
	vidFramesIn  uint64 // distinct frame_num values seen on video channel
	vidFramesOut uint64 // AUs successfully queued to consumers
	vidDropped   uint64 // AUs discarded because a frag_idx gap was detected
	fragSkips    uint64 // count of frag_idx-skip events (fragments lost on the wire)
	fragsLost    uint64 // total fragments lost (sum of frag_idx gap sizes)
	forceDrains  uint64 // times forceDrain ran with buffered packets to flush
	recvTimeouts uint64 // SetReadDeadline-triggered timeouts (no UDP data)
}

type pendingFrag struct {
	channel  byte
	b1       byte
	isAudio  bool   // true for either audio variant (channels 0x03 or 0x07/b1=0x0d)
	subExt   uint64
	frameNum uint32 // camera's per-channel frame counter (inner[28..31])
	fragIdx  uint16 // index of fragment within frame (inner[22..23])
	payload  []byte
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
	videoChan := innerChMain
	if opts.Quality == "sd" {
		videoChan = innerChSub
	}
	c := &Client{
		conn:         udp,
		cam:          cam,
		uid:          opts.UID,
		nonce:        nonce,
		kseq:         2,
		audio:        opts.Audio,
		quality:      opts.Quality,
		disableSub:   opts.DisableSub,
		verbose:      opts.Verbose,
		videoChannel: videoChan,
		frames:       make(chan *Packet, 256),
	}
	if path := os.Getenv("PETLIBRO_DUMP_VIDEO"); path != "" {
		if f, ferr := os.Create(path); ferr == nil {
			c.dumpFile = f
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
	if err := c.send(buildLANSearch3(c.uid, c.nonce, 1)); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pkt, err := c.recvOne(500 * time.Millisecond)
		if err != nil || len(pkt) < 12 {
			continue
		}
		if binary.LittleEndian.Uint16(pkt[8:]) == msgLANSearchR {
			break
		}
	}
	_ = c.send(buildLANSearch3(c.uid, c.nonce, 2))
	time.Sleep(30 * time.Millisecond)
	_ = c.send(buildKnock2(c.uid, c.nonce))
	deadline = time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pkt, err := c.recvOne(500 * time.Millisecond)
		if err != nil || len(pkt) < 12 {
			continue
		}
		if binary.LittleEndian.Uint16(pkt[8:]) == msgKnockRR2 {
			break
		}
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

	// 2. The 7 IOCtrl bootstrap commands.
	stream := qualityHD
	if c.quality == "sd" {
		stream = qualitySD
	}
	type cmd struct {
		chanHi  uint16
		payload []byte
	}
	cmds := []cmd{
		{0x7000, []byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0xb0}},
		{0x7000, []byte{0x00, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x28}},
		{0x7000, ioctlBody12(IOCtrlVendor0372)},
		{0x7000, ioctlBody12(IOCtrlVendor0322)},
		{0x7000, ioctlBody12(IOCtrlVendor032A)},
	}
	if c.disableSub {
		// Experimental: try to ask the camera to stop dual-streaming.
		// Send the "disable" SETSTREAMCTRL for chan=2 before the
		// activate command for chan=1.  Both are sent on the same
		// control channel as the regular quality command.
		cmds = append(cmds, cmd{0x1000, disableSubProbe})
	}
	cmds = append(cmds, cmd{0x1000, stream})
	if c.audio {
		audioOn := ioctlBody12(IOCtrlAudioOn)
		audioOn[4] = 0x01 // enable flag
		cmds = append(cmds, cmd{0x7000, audioOn})
		startBody := ioctlBody12(IOCtrlStart)
		startBody[4] = 0x01
		cmds = append(cmds, cmd{0x7000, startBody})
	} else {
		cmds = append(cmds, cmd{0x7000, ioctlBody12(IOCtrlStart)})
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

	rawChan := make(chan []byte, 1024) // ~1 MiB at peak packet sizes
	go c.readerGoroutine(rawChan)

	lastForce := time.Now()
	lastStats := time.Now()
	tick := time.NewTicker(50 * time.Millisecond)
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

		if time.Since(lastForce) > 500*time.Millisecond {
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
			c.stats.recvTimeouts++
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
		}
	}
	fmt.Printf("[petlibro/stats] in=%d pkts (%d KiB) channels: main=%d sub=%d audio=%d other=%d | video: %d frames in -> %d out (drop %d) | frag skips: %d (%d frags lost) | forceDrain: %d\n",
		delta.pktsIn, delta.bytesIn/1024,
		delta.mainFrags, delta.subFrags, delta.audioFrags, delta.otherFrags,
		delta.vidFramesIn, delta.vidFramesOut, delta.vidDropped,
		delta.fragSkips, delta.fragsLost, delta.forceDrains)
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
	c.avBuffer[subExt] = &pendingFrag{
		channel: channel, b1: b1, isAudio: isAudio,
		subExt: subExt, frameNum: frameNum, fragIdx: fragIdx, payload: payload,
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

// emit handles one in-order entry.  Petlibro's b1 fragment-tag (00/04/05)
// turns out to be transport-level, NOT H.264 access-unit boundaries — a
// single AU can span many "frames" in petlibro's terms.  Instead we
// concatenate all video bytes into a rolling buffer and scan for proper
// H.264 Annex-B AU start markers (SPS or non-IDR slice) to delimit AUs.
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
	// Cameras commonly dual-stream: main (0x05) carries a high-quality
	// elementary stream and sub (0x07) carries a low-quality one, each
	// with its own SPS/PPS/IDR/P-slices and its own frame counter.
	// Concatenating both produces garbled H.264 because the streams
	// are independent.  Consume only the channel matching the
	// requested quality.
	if e.channel != c.videoChannel {
		return
	}
	c.stats.vidFrags++
	// `inner[28..31]` was documented in PROTOCOL_NOTES as a per-frame
	// counter, but on this firmware it changes far slower than the
	// SPS-declared 25 fps — empirically once per GOP (~1 Hz).  Using
	// it as the AU boundary gave us slideshow playback.  Track it
	// for stats only.
	if c.curFrameNum != 0 && e.frameNum != c.curFrameNum {
		c.stats.vidFramesIn++
		c.expectFragIdx = 0
	}
	c.curFrameNum = e.frameNum
	if e.fragIdx != c.expectFragIdx {
		c.stats.fragSkips++
		if e.fragIdx > c.expectFragIdx {
			c.stats.fragsLost += uint64(e.fragIdx - c.expectFragIdx)
		}
	}
	c.expectFragIdx = e.fragIdx + 1

	payload := stripFragmentMetadataTrailer(e.payload)
	if c.dumpFile != nil {
		_, _ = c.dumpFile.Write(payload)
	}
	c.curBuf = append(c.curBuf, payload...)
	// AU boundaries come from scanning the byte stream for a NEW
	// access-unit-starting NAL — SPS (7) for keyframes, P-slice (1)
	// for non-IDR pictures.  This is the per-FRAME boundary the
	// decoder needs, not the per-GOP frame_num.  The camera does
	// emit emulation-prevention bytes for SPS-7 patterns (we verified
	// — no false positives observed for `00 00 00 01 67`) so we can
	// safely cut there.  Non-IDR boundaries (NAL=1) DO have false
	// positives in slice payload, so we only cut on SPS or on the
	// first byte after seeing the slice-end pattern `00 80` (a common
	// RBSP trailing marker).  Best-effort but produces ~25 fps.
	c.tryEmitAU()
}

// tryEmitAU scans curBuf for complete access units and emits them.
// An AU is defined as the bytes between two AU-start markers (SPS or
// non-IDR P-slice).  We need TWO start markers to know the first AU
// is complete — otherwise we'd emit a truncated AU and corrupt the
// trailing slice.  The (small) penalty: ~1 frame of latency.
func (c *Client) tryEmitAU() {
	for {
		first := findAUStart(c.curBuf, 0)
		if first < 0 {
			return
		}
		if first > 0 {
			// Drop everything before the first real NAL.
			c.curBuf = c.curBuf[first:]
		}
		// Skip past the leading start code so we don't immediately
		// re-find it.
		skip := 4
		if len(c.curBuf) >= 4 && !(c.curBuf[0] == 0 && c.curBuf[1] == 0 &&
			c.curBuf[2] == 0 && c.curBuf[3] == 1) {
			skip = 3
		}
		next := findAUStart(c.curBuf, skip)
		if next < 0 {
			return
		}
		au := append([]byte(nil), c.curBuf[:next]...)
		c.curBuf = c.curBuf[next:]
		c.emitAU(au)
	}
}

// findAUStart finds the byte offset of the next H.264 access unit
// boundary in b at offset >= start.  An AU starts at an SPS NAL (type
// 7) or a non-IDR slice (type 1).  Returns -1 if none found.
func findAUStart(b []byte, start int) int {
	for i := start; i+4 < len(b); i++ {
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
			return -1
		}
		nal := b[nalPos] & 0x1F
		if nal == 7 || nal == 1 {
			return i
		}
	}
	return -1
}

// emitAU finalises one access unit and queues it for the consumer.
func (c *Client) emitAU(au []byte) {
	if len(au) < 5 {
		return
	}
	isKey := containsNALType(au, 5)
	const ticksPerFrame = 90000 / 25
	pts := c.emitSeq * ticksPerFrame
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


// stripFragmentMetadataTrailer removes the 15-byte per-frame metadata
// trailer the Petlibro firmware appends to the LAST video fragment of
// each frame.  The trailer signature is:
//
//	00 00 00 01  00 00 00 00 00 00 00  <4-byte LE millisecond counter>
//
// The leading 4 bytes look like an Annex-B start code, but if the bytes
// are fed to a strict H.264 decoder they parse as a NAL of type 0
// ("unspecified") which is invalid.  We detect the structural prefix
// (11 fixed bytes) and discard the whole 15-byte trailer at the
// fragment boundary — well before the decoder ever sees it.
//
// Stripping per-fragment (rather than scanning the merged AU buffer)
// matters because the camera does NOT insert H.264 emulation-prevention
// bytes, so the same byte signature occasionally appears inside slice
// payload as a coincidence; an AU-level scanner would mistakenly cut
// real slice bytes.  At the fragment tail there is no such ambiguity.
func stripFragmentMetadataTrailer(p []byte) []byte {
	if len(p) < 15 {
		return p
	}
	t := p[len(p)-15:]
	if t[0] == 0 && t[1] == 0 && t[2] == 0 && t[3] == 1 &&
		t[4] == 0 && t[5] == 0 && t[6] == 0 && t[7] == 0 &&
		t[8] == 0 && t[9] == 0 && t[10] == 0 {
		return p[:len(p)-15]
	}
	return p
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
