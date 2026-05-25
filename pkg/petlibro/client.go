// Package petlibro is a clean-room LAN P2P client for Petlibro pet
// cameras (PLAF203 / PLAF103 / etc).  These cameras run a Kalay/TUTK
// firmware variant: same luffy crypto as Wyze (we reuse pkg/tutk's
// TransCodePartial / ReverseTransCodePartial directly) but with a
// different protocol-version byte, direction flag, LOGIN structure
// and bootstrap IOCtrl sequence.
//
// Pkg layout:
//
//	templates.go  — protocol constants + LOGIN templates + frame builders
//	client.go     — Client struct + Dial + send/recv helpers + public surface
//	handshake.go  — LAN_SEARCH3 / KNOCK2 / LOGIN A+B sequencing
//	bootstrap.go  — post-LOGIN 7-IOCtrl bootstrap (SETSTREAMCTRL → IPCAM_START)
//	recv.go       — UDP recv/processor goroutines + maintenance loop + stats
//	assembler.go  — fragment reassembly (wrapSeq, channelAsm, parseDatagram)
//	producer.go   — wraps the Client into a go2rtc Producer
//
// petlibro divergence vs pkg/tutk: petlibro shares pkg/tutk's Luffy
// crypto (tutk.TransCodePartial / tutk.ReverseTransCodePartial) but
// reimplements every other layer because the wire protocol differs.
// Each new file above carries a short "petlibro divergence vs pkg/tutk"
// header comment pointing at the parallel tutk site for future
// consolidation reference; the user-locked decision #5 was to keep
// petlibro standalone and add these breadcrumbs rather than reshape
// pkg/tutk in this PR.
package petlibro

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
	"github.com/rs/zerolog"
)

// Codec IDs we surface to the producer (subset of pkg/tutk's table).
const (
	CodecH264    byte = 0x4E
	CodecAACADTS byte = 0x87
)

// log is the package-level zerolog instance.  internal/petlibro's
// Init() injects app.GetLogger("petlibro") via SetLogger so library
// messages reach the same sink as the rest of go2rtc.  Default is a
// no-op so unit tests don't depend on internal/ being loaded.
var log = zerolog.Nop()

// SetLogger lets the internal/petlibro glue inject the
// app.GetLogger("petlibro") instance so library messages go through
// the same zerolog sink as the rest of go2rtc.  Matches the wyze/tapo
// pattern of internal-side logger configuration, just made explicit
// instead of hidden behind a fmt.Printf chain.
func SetLogger(l zerolog.Logger) { log = l }

// Packet is one fully-assembled media frame from the camera.
type Packet struct {
	Codec      byte
	Payload    []byte
	Timestamp  uint32
	FrameNo    uint32
	IsKeyframe bool
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
	wrap          wrapSeq
	avBuffer      map[uint64]*pendingFrag
	avNextExt     uint64
	avHighExt     uint64
	avPrevSubWire uint16

	// monotonic counters for outgoing Packets (camera's per-channel
	// frame_num is independent for main vs sub, so we use our own)
	emitSeq      uint32
	emitAudioSeq uint32
	startedAt    time.Time

	// frames carries assembled AUs to the consumer.  done is closed
	// by Close() to signal every worker goroutine to exit; we select
	// on it instead of carrying a separate closed-bool + mutex +
	// recover() dance.  Closing done first means the
	// send-on-closed-frames race is structurally impossible — every
	// caller writing to frames first selects on done.
	frames    chan *Packet
	done      chan struct{}
	closeOnce sync.Once

	audio       bool
	quality     string
	strict      bool
	verbose     bool
	mainAsm     channelAsm // per-channel assembly state for ch=0x05
	subAsm      channelAsm // per-channel assembly state for ch=0x07
	gopPoisoned bool       // strict mode only: a fragment was lost in this GOP — drop P-frames until next clean IDR

	// Camera-clock PTS state.  pendingFrameTs is the millisecond value
	// extracted from the most recently seen metadata trailer; it
	// becomes the PTS for the next emitted AU.  firstFrameTs is the
	// first ts we saw, used to anchor the 90kHz PTS at 0.
	pendingFrameTs uint32
	havePendingTs  bool
	firstFrameTs   uint32
	haveFirstTs    bool
	lastEmitTs     uint32 // last PTS we emitted (in 90 kHz) — monotonic guard

	stats         counters
	prevStats     countersSnapshot
	havePrevStats bool

	// forceDrain stall tracking.  If avHighExt hasn't advanced for
	// forceDrainStallTicks consecutive force-drain ticks and the
	// buffer is non-empty, we flush regardless of the high-water
	// threshold — recovers from a stalled camera leaving partial AUs
	// stranded forever.
	stallTicks  uint8
	lastHighExt uint64
}

// counters holds running totals for a stream-health summary. Atomic
// fields because the reader goroutine writes bytesIn/pktsIn/
// recvTimeouts/readerDrops while the processor writes every other
// field — plain uint64 access would trip `go test -race`.
type counters struct {
	bytesIn      atomic.Uint64 // bytes read from the UDP socket (encrypted)
	pktsIn       atomic.Uint64 // UDP datagrams successfully read
	mainFrags    atomic.Uint64 // fragments on the main video channel (ch=0x05, IDR-bearing)
	subFrags     atomic.Uint64 // fragments on the sub video channel (ch=0x07, P-frames)
	audioFrags   atomic.Uint64 // fragments on the audio channel (ch=0x03)
	otherFrags   atomic.Uint64 // anything else (control, etc.)
	vidFrags     atomic.Uint64 // video fragments (ch=0x05 + ch=0x07) reaching emit
	vidFramesIn  atomic.Uint64 // distinct frame_num values seen on video channel
	vidFramesOut atomic.Uint64 // AUs successfully queued to consumers
	vidDropped   atomic.Uint64 // AUs discarded because a frag_idx gap was detected
	fragSkips    atomic.Uint64 // count of frag_idx-skip events (fragments lost on the wire)
	fragsLost    atomic.Uint64 // total fragments lost (sum of frag_idx gap sizes)
	forceDrains  atomic.Uint64 // times forceDrain ran with buffered packets to flush
	recvTimeouts atomic.Uint64 // SetReadDeadline-triggered timeouts (no UDP data)
	readerDrops  atomic.Uint64 // raw UDP packets dropped because the processor queue was full
	emitDrops    atomic.Uint64 // assembled AUs dropped because the consumer queue was full
	dualStreamIL atomic.Uint64 // dual-stream interleaved end-fragments (decision #6 defensive counter)
}

// countersSnapshot is a plain-value snapshot of counters for diff
// reporting in dumpStats. atomic.Uint64 cannot be copied.
type countersSnapshot struct {
	bytesIn      uint64
	pktsIn       uint64
	mainFrags    uint64
	subFrags     uint64
	audioFrags   uint64
	otherFrags   uint64
	vidFrags     uint64
	vidFramesIn  uint64
	vidFramesOut uint64
	vidDropped   uint64
	fragSkips    uint64
	fragsLost    uint64
	forceDrains  uint64
	recvTimeouts uint64
	readerDrops  uint64
	emitDrops    uint64
	dualStreamIL uint64
}

func (c *counters) snapshot() countersSnapshot {
	return countersSnapshot{
		bytesIn:      c.bytesIn.Load(),
		pktsIn:       c.pktsIn.Load(),
		mainFrags:    c.mainFrags.Load(),
		subFrags:     c.subFrags.Load(),
		audioFrags:   c.audioFrags.Load(),
		otherFrags:   c.otherFrags.Load(),
		vidFrags:     c.vidFrags.Load(),
		vidFramesIn:  c.vidFramesIn.Load(),
		vidFramesOut: c.vidFramesOut.Load(),
		vidDropped:   c.vidDropped.Load(),
		fragSkips:    c.fragSkips.Load(),
		fragsLost:    c.fragsLost.Load(),
		forceDrains:  c.forceDrains.Load(),
		recvTimeouts: c.recvTimeouts.Load(),
		readerDrops:  c.readerDrops.Load(),
		emitDrops:    c.emitDrops.Load(),
		dualStreamIL: c.dualStreamIL.Load(),
	}
}

// Dial parses a petlibro:// URL, opens a UDP socket, optionally
// discovers the camera address by UID, runs LAN_SEARCH3 + KNOCK2 +
// LOGIN A/B + the 7-cmd Petlibro bootstrap, then starts the receive
// worker.  Returns once IPCAM_START has been sent and the AV-ready ack
// acknowledged — the camera will then stream video (and audio if
// &audio=true).
//
// URL shape:
//
//	petlibro://<host>?uid=<UID>[&audio=true][&quality=hd|sd][&strict=1][&verbose=1]
//	petlibro://?uid=<UID>[&subnet=192.168.1.0/24][&audio=true][&quality=hd|sd][&strict=1][&verbose=1]
//
//	strict=1 — drop any IDR with a fragment loss and poison the GOP
//	           (pristine pixels at the cost of multi-second freezes
//	           on lossy networks).  Default is to emit gapped IDRs
//	           with localised macroblock artefacts and drop gapped
//	           P-frames (avoids cascading inter-frame errors).
//
// Matches the project-wide Dial(rawURL string) (*Client, error)
// signature used by every other UDP/TCP camera adapter — see
// pkg/wyze, pkg/tapo, pkg/kasa, pkg/dvrip.
//
// petlibro divergence vs pkg/tutk: pkg/tutk's Dial
// (pkg/tutk/conn.go:12) takes (host, uid, username, password)
// positionally and goes through Nebula/relay if the port is 10001 —
// neither applies here.  Petlibro is LAN-only and parses options out
// of a URL because the per-camera tweaks (strict/quality/verbose) are
// petlibro-specific runtime flags.
func Dial(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("petlibro: bad url: %w", err)
	}
	q := u.Query()
	uid := q.Get("uid")
	if uid == "" {
		return nil, fmt.Errorf("petlibro: uid query parameter required")
	}
	// 20-character TUTK UID — anything else is a user typo, and
	// silently truncating / zero-padding it through buildLANSearch3
	// just makes the camera not respond, leaving the user to debug a
	// LOGIN_RESP timeout with no hint.  Fail early with a clear msg.
	if len(uid) != 20 {
		return nil, fmt.Errorf("petlibro: uid must be 20 chars (got %d)", len(uid))
	}
	quality := q.Get("quality")
	if quality == "" {
		quality = "hd"
	}

	var cam *net.UDPAddr
	if host := u.Host; host != "" {
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, strconv.Itoa(lanPort))
		}
		cam, err = net.ResolveUDPAddr("udp", host)
		if err != nil {
			return nil, fmt.Errorf("petlibro: resolve %s: %w", host, err)
		}
	}
	udp, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, fmt.Errorf("petlibro: bind: %w", err)
	}
	// HD video bursts > 3 Mbps; the default 768 KiB recv buffer is
	// only ~2 seconds of headroom and easily overflowed by a slow
	// drain.  Ask for 4 MiB — the kernel will clamp if it can't.
	const wantBuf = 4 * 1024 * 1024
	_ = udp.SetReadBuffer(wantBuf)
	verbose := boolQuery(q, "verbose")
	// Verify what the kernel actually granted us — SetReadBuffer
	// silently clamps to net.core.rmem_max and the symptom is
	// readerDrops climbing with no other signal.  Warn
	// unconditionally on clamp so users notice the kernel limit.
	if sc, err := udp.SyscallConn(); err == nil {
		var actualBuf int
		_ = sc.Control(func(fd uintptr) {
			actualBuf, _ = syscall.GetsockoptInt(int(fd),
				syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		})
		// Linux SO_RCVBUF reports double the granted size (man 7
		// socket); halve before comparing.
		granted := actualBuf
		if granted >= 2*wantBuf {
			granted /= 2
		}
		if granted < wantBuf {
			log.Warn().Msgf("SO_RCVBUF clamped: requested=%d granted=%d (consider raising net.core.rmem_max)", wantBuf, granted)
		} else if verbose {
			log.Debug().Msgf("SO_RCVBUF requested=%d granted=%d", wantBuf, granted)
		}
	}

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		_ = udp.Close()
		return nil, err
	}
	if cam == nil {
		cam, err = discoverByUID(udp, uid, nonce, q["subnet"], verbose)
		if err != nil {
			_ = udp.Close()
			return nil, err
		}
	}
	c := &Client{
		conn:    udp,
		cam:     cam,
		uid:     uid,
		nonce:   nonce,
		kseq:    2,
		audio:   boolQuery(q, "audio"),
		quality: quality,
		strict:  boolQuery(q, "strict"),
		verbose: verbose,
		frames:  make(chan *Packet, 1024),
		done:    make(chan struct{}),
	}
	if err := c.handshake(); err != nil {
		clearDiscoveryCache(uid, q["subnet"])
		_ = udp.Close()
		return nil, err
	}
	if err := c.bootstrap(); err != nil {
		clearDiscoveryCache(uid, q["subnet"])
		_ = udp.Close()
		return nil, err
	}

	go c.recvLoop()
	go c.maintenanceLoop()
	return c, nil
}

// boolQuery parses a URL query bool using strconv.ParseBool's
// canonical set (1/0/true/false/...).  Empty value returns false.
// Stand-in for the four `q.Get("x") == "true" || q.Get("x") == "1"`
// chains that used to live in NewProducer.
func boolQuery(q url.Values, key string) bool {
	v := q.Get(key)
	if v == "" {
		return false
	}
	b, _ := strconv.ParseBool(v)
	return b
}

// --- I/O helpers (use pkg/tutk for the luffy crypto) ----------------------

// send writes a single Kalay datagram to the camera. Byte count is
// discarded: all packets we generate fit comfortably under MTU (the
// largest is the 572-byte LOGIN_1 packet), so a short write would
// only happen on a closed/broken socket — and the returned error
// already signals that.
//
// petlibro divergence vs pkg/tutk: pkg/tutk wraps the same primitive
// at pkg/tutk/conn.go:91 (Conn.Write), but uses tutk.TransCodePartial
// as the encrypt direction — identical here because the Luffy primitive
// is symmetric across protocol variants.  The only thing that differs
// is the inverted naming convention in pkg/tutk (TransCodePartial is
// the encrypt direction in tutk too — see crypto.go:81); we point this
// out at every call site for the inevitable consolidation pass.
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
	// petlibro divergence vs pkg/tutk: tutk's Conn.Read decrypts via
	// ReverseTransCodePartial at pkg/tutk/conn.go:84 — same direction,
	// same primitive.  Only difference is petlibro uses this synchronous
	// helper exclusively during handshake/bootstrap; post-bootstrap
	// reads go through the readerGoroutine/processor split in recv.go.
	return tutk.ReverseTransCodePartial(nil, buf[:n]), nil
}

// --- public surface ------------------------------------------------------

// ReadPacket blocks until a Packet is available or the connection closes.
func (c *Client) ReadPacket() (*Packet, error) {
	select {
	case <-c.done:
		return nil, io.EOF
	case p, ok := <-c.frames:
		if !ok {
			return nil, io.EOF
		}
		return p, nil
	}
}

// Close shuts the session down.  closeOnce.Do(close(done) +
// conn.Close() + close(frames)) — closing done before frames means
// every goroutine selecting on done (recvLoop, readerGoroutine,
// maintenanceLoop, queuePacket, ReadPacket) drops out before we touch
// the frames channel they were writing to, so the
// send-on-closed-channel race is structurally impossible.  Investigated
// pkg/wyze (closeMu+bool), pkg/tapo / pkg/dvrip (bare conn.Close()),
// and pkg/onvif (no Close).  None of those use a done channel today,
// but for a multi-goroutine UDP client this is the simpler, mutex-free
// equivalent of wyze's pattern.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
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
