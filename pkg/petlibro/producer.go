package petlibro

import (
	"fmt"
	"net/url"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
)

type Producer struct {
	core.Connection
	client *Client

	// firstAU holds the SPS-bearing IDR access unit that probe()
	// consumed to derive the H.264 codec parameters.  Start() replays
	// it as the first emitted frame so consumers don't have to wait
	// up to a full GOP for the next IDR.  Annex-B encoded.
	firstAU []byte
	firstTS uint32
	firstFN uint32
}

// NewProducer parses a petlibro:// URL, dials the camera, probes the
// codec (waits for an SPS so we can build a proper SDP), and returns a
// fully-populated Producer.
//
// URL shape:
//
//	petlibro://<host>?uid=<UID>[&audio=true][&quality=hd|sd][&verbose=1]
func NewProducer(rawURL string) (*Producer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("petlibro: bad url: %w", err)
	}
	q := u.Query()

	opts := DialOptions{
		UID:        q.Get("uid"),
		Host:       u.Host,
		Audio:      q.Get("audio") == "true" || q.Get("audio") == "1",
		Quality:    q.Get("quality"),
		DisableSub: q.Get("disable_sub") == "true" || q.Get("disable_sub") == "1",
		Strict:     q.Get("strict") == "true" || q.Get("strict") == "1",
		Verbose:    q.Get("verbose") == "true" || q.Get("verbose") == "1",
	}
	if opts.UID == "" {
		return nil, fmt.Errorf("petlibro: uid query parameter required")
	}

	client, err := Dial(opts)
	if err != nil {
		return nil, err
	}

	medias, firstAU, firstTS, firstFN, err := probe(client)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	return &Producer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "petlibro",
			Protocol:   client.Protocol(),
			RemoteAddr: client.RemoteAddr().String(),
			Source:     rawURL,
			Medias:     medias,
			Transport:  client,
		},
		client:  client,
		firstAU: firstAU,
		firstTS: firstTS,
		firstFN: firstFN,
	}, nil
}

func (p *Producer) Start() error {
	// probe() saved the SPS-bearing IDR.  Replay it as the first frame
	// so the downstream decoder gets a valid GOP head immediately and
	// doesn't have to wait up to a full GOP (~1-2 s) for the next IDR
	// from the camera.  Any P-frames already buffered behind probe's
	// IDR will reference that very IDR — they're now valid too.
	keyframeSeen := false
	if p.firstAU != nil {
		avcc := annexb.EncodeToAVCC(p.firstAU)
		if len(avcc) >= 5 {
			for _, recv := range p.Receivers {
				if recv.Codec.Name == core.CodecH264 {
					// Version=0 (default) is the AVCC-payload
					// sentinel that pkg/h264.RTPPay reads as "fragment
					// me into RTP packets".  Setting Version=2 here
					// would make RTPPay pass our AVCC payload through
					// as if already RTP-packetised — decoders would
					// then see an AVCC length header as a NAL byte and
					// produce only noise.
					recv.WriteRTP(&core.Packet{
						Header: rtp.Header{
							SequenceNumber: uint16(p.firstFN),
							Timestamp:      p.firstTS,
						},
						Payload: avcc,
					})
					break
				}
			}
		}
		keyframeSeen = true
	}

	for {
		_ = p.client.SetDeadline(time.Now().Add(core.ConnDeadline))
		pkt, err := p.client.ReadPacket()
		if err != nil {
			return err
		}
		if pkt == nil {
			continue
		}

		var name string
		var pkt2 *core.Packet

		switch pkt.Codec {
		case CodecH264:
			if !keyframeSeen {
				if !pkt.IsKeyframe {
					continue
				}
				keyframeSeen = true
			}
			name = core.CodecH264
			avcc := annexb.EncodeToAVCC(pkt.Payload)
			if len(avcc) < 5 {
				// AVCC needs at least a 4-byte length + 1 NAL header byte
				// or downstream RepairAVCC panics indexing payload[4].
				continue
			}
			pkt2 = &core.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: avcc,
			}

		case CodecAACADTS:
			name = core.CodecAAC
			payload := pkt.Payload
			if aac.IsADTS(payload) && len(payload) >= 6 {
				// The camera occasionally appends padding bytes after the
				// real AAC frame; trim to the ADTS-declared length so the
				// RTP packetizer doesn't claim a too-large AU.
				frameLen := int(payload[3]&0x03)<<11 | int(payload[4])<<3 | int(payload[5])>>5
				if frameLen > aac.ADTSHeaderLen(payload) && frameLen <= len(payload) {
					payload = payload[:frameLen]
				}
				payload = payload[aac.ADTSHeaderLen(payload):]
			}
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: aac.RTPPacketVersionAAC, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: payload,
			}

		default:
			continue
		}

		for _, recv := range p.Receivers {
			if recv.Codec.Name == name {
				recv.WriteRTP(pkt2)
				break
			}
		}
	}
}

// adtsParams returns (sampleRate, channels) from an ADTS-framed AAC
// header.  Returns (0, 0) if the header isn't valid ADTS.
func adtsParams(b []byte) (int, int) {
	if !aac.IsADTS(b) {
		return 0, 0
	}
	sampleRates := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	sampleRateIdx := (b[2] >> 2) & 0x0F
	if int(sampleRateIdx) >= len(sampleRates) {
		return 0, 0
	}
	ch := (b[2]&0x01)<<2 | (b[3]>>6)&0x03
	return sampleRates[sampleRateIdx], int(ch)
}

// probe reads frames until we have a video codec (with parameter sets)
// and, if audio was requested, an audio codec.  Returns the SPS-bearing
// IDR's full Annex-B AU so the producer can replay it as the first
// emitted frame, saving consumers up to one full GOP of wait time.
func probe(client *Client) ([]*core.Media, []byte, uint32, uint32, error) {
	_ = client.SetDeadline(time.Now().Add(core.ProbeTimeout))

	var vcodec, acodec *core.Codec
	var firstAU []byte
	var firstTS, firstFN uint32
	for {
		pkt, err := client.ReadPacket()
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("petlibro: probe: %w", err)
		}
		if pkt == nil || len(pkt.Payload) < 5 {
			continue
		}

		switch pkt.Codec {
		case CodecH264:
			if vcodec == nil {
				buf := annexb.EncodeToAVCC(pkt.Payload)
				if len(buf) >= 5 && h264.NALUType(buf) == h264.NALUTypeSPS {
					vcodec = h264.AVCCToCodec(buf)
					// Petlibro packs SPS+PPS+IDR into one AU; keep the
					// Annex-B form so Start() can re-emit it verbatim.
					firstAU = append([]byte(nil), pkt.Payload...)
					firstTS = pkt.Timestamp
					firstFN = pkt.FrameNo
				}
			}
		case CodecAACADTS:
			if acodec == nil && aac.IsADTS(pkt.Payload) {
				// Derive sample rate / channels from ADTS to build the
				// AudioSpecificConfig that the RTP layer needs.
				sr, ch := adtsParams(pkt.Payload)
				if sr > 0 && ch > 0 {
					cfg := aac.EncodeConfig(aac.TypeAACLC, uint32(sr), byte(ch), false)
					acodec = aac.ConfigToCodec(cfg)
				}
			}
		}

		if vcodec != nil && (acodec != nil || !client.Audio()) {
			break
		}
	}

	_ = client.SetDeadline(time.Time{})

	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{vcodec},
		},
	}
	if acodec != nil {
		medias = append(medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{acodec},
		})
	}
	return medias, firstAU, firstTS, firstFN, nil
}
