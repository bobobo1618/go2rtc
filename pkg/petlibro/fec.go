package petlibro

import (
	"bytes"
	"encoding/binary"
	"time"
)

// The optional 3.1.54 transport FEC metadata is group:u16,index:u8,k:u8.
// A ch=0x0c packet contains a parity shard over the 20-byte AV header and
// payload. Only enable reconstruction after three complete groups validate
// the exact GF(256) polynomial and systematic interpolation byte-for-byte.
// Unknown variants remain ignored; never guess a recovered media payload.
type fecGroup struct {
	shards  map[byte][]byte
	k       byte
	created time.Time
}
type fecState struct {
	groups    map[uint16]*fecGroup
	validated uint64
	disabled  bool
}

var gfMulTable = func() [256][256]byte {
	var t [256][256]byte
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			x, y, r := a, b, 0
			for y > 0 {
				if y&1 != 0 {
					r ^= x
				}
				x <<= 1
				if x&256 != 0 {
					x ^= 0x11d
				}
				y >>= 1
			}
			t[a][b] = byte(r)
		}
	}
	return t
}()

func gfInv(v byte) byte {
	for i := 1; i < 256; i++ {
		if gfMulTable[v][i] == 1 {
			return byte(i)
		}
	}
	return 0
}
func interpolateShard(shards map[byte][]byte, target byte, size int) []byte {
	out := make([]byte, size)
	for x, data := range shards {
		coefficient := byte(1)
		for y := range shards {
			if x != y {
				coefficient = gfMulTable[coefficient][gfMulTable[target^y][gfInv(x^y)]]
			}
		}
		for i, b := range data {
			if i >= size {
				break
			}
			out[i] ^= gfMulTable[coefficient][b]
		}
	}
	return out
}
func (c *Client) handleFEC(inner []byte) {
	if len(inner) < 36 || inner[2] != 0x0b {
		return
	}
	off := 16
	if inner[1] == 8 || inner[1] == 9 || inner[1] == 13 {
		off = 24
	}
	if len(inner) < off+20 {
		return
	}
	groupID := binary.LittleEndian.Uint16(inner[off-8:])
	index, k := inner[off-6], inner[off-5]
	if k < 2 || k > 32 || index > k {
		return
	}
	channel := inner[off]
	parity := channel == 0x0c
	if parity && index != k {
		return
	}
	if !parity && (index >= k || (channel != innerChMain && channel != innerChSub && channel != innerChAudio)) {
		return
	}
	paylen := int(binary.LittleEndian.Uint16(inner[off+8:]))
	if paylen == 0 || off+20+paylen > len(inner) {
		return
	}
	data := inner[off : off+20+paylen]
	if parity {
		data = data[20:]
		c.v2.fecParity++
	}
	f := &c.fec
	if f.disabled {
		return
	}
	if f.groups == nil {
		f.groups = make(map[uint16]*fecGroup)
	}
	now := c.timeNow()
	for id, g := range f.groups {
		if now.Sub(g.created) > 2*time.Second {
			delete(f.groups, id)
		}
	}
	g := f.groups[groupID]
	if g == nil {
		if len(f.groups) >= 64 {
			return
		}
		g = &fecGroup{shards: make(map[byte][]byte), k: k, created: now}
		f.groups[groupID] = g
	}
	if g.k != k {
		return
	}
	g.shards[index] = append([]byte(nil), data...)
	parityData, haveParity := g.shards[k]
	if !haveParity {
		return
	}
	if len(g.shards) == int(k)+1 {
		delete(g.shards, k)
		calculated := interpolateShard(g.shards, k, len(parityData))
		if bytes.Equal(calculated, parityData) {
			f.validated++
			c.v2.fecValid++
		} else {
			c.v2.fecInvalid++
			f.disabled = true
			log.Warn().Msg("FEC validation mismatch; reconstruction disabled for session")
		}
		delete(f.groups, groupID)
		return
	}
	if len(g.shards) != int(k) || f.validated < 3 {
		return
	}
	var missing byte
	for missing = 0; missing < k; missing++ {
		if g.shards[missing] == nil {
			break
		}
	}
	recovered := interpolateShard(g.shards, missing, len(parityData))
	delete(f.groups, groupID)
	if len(recovered) < 20 {
		return
	}
	ch := recovered[0]
	n := int(binary.LittleEndian.Uint16(recovered[8:]))
	seq := binary.LittleEndian.Uint16(recovered[2:])
	if (ch != innerChMain && ch != innerChSub && ch != innerChAudio) || n == 0 || n > len(recovered)-20 {
		c.v2.fecInvalid++
		return
	}
	ext, ok := c.wrap.extend(seq)
	if !ok || ext < c.avNextExt || ext > c.avHighExt+32 {
		return
	}
	// Synthetic wrapper deliberately omits FEC metadata to avoid reinsertion.
	pkt := make([]byte, 0x1c+16+20+n)
	pkt[3] = flagsRecv
	binary.LittleEndian.PutUint16(pkt[8:], msgSessionD2C)
	pkt[0x1c] = 0x0c
	copy(pkt[0x1c+16:], recovered[:20+n])
	if recovered[1] == 1 {
		pkt[0x1c+1] = 1
	}
	c.v2.fecRecovered++
	c.parseDatagram(pkt)
}
