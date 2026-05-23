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
		Verbose:    q.Get("verbose") == "true" || q.Get("verbose") == "1",
	}
	if opts.UID == "" {
		return nil, fmt.Errorf("petlibro: uid query parameter required")
	}

	client, err := Dial(opts)
	if err != nil {
		return nil, err
	}

	medias, err := probe(client)
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
		client: client,
	}, nil
}

func (p *Producer) Start() error {
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
// and, if audio was requested, an audio codec.
func probe(client *Client) ([]*core.Media, error) {
	_ = client.SetDeadline(time.Now().Add(core.ProbeTimeout))

	var vcodec, acodec *core.Codec
	for {
		pkt, err := client.ReadPacket()
		if err != nil {
			return nil, fmt.Errorf("petlibro: probe: %w", err)
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
	return medias, nil
}
