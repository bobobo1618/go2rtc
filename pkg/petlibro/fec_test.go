package petlibro

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestGFInterpolation(t *testing.T) {
	// y=x and y=constant are independent polynomial fixtures.
	got := interpolateShard(map[byte][]byte{0: {0, 7}, 1: {1, 7}, 2: {2, 7}}, 20, 2)
	if !bytes.Equal(got, []byte{20, 7}) {
		t.Fatal(got)
	}
	for n := 0; n < 20; n++ {
		shards := map[byte][]byte{}
		for i := 0; i <= 20; i++ {
			if i != n {
				shards[byte(i)] = []byte{byte(i), byte(i) ^ 71}
			}
		}
		got = interpolateShard(shards, byte(n), 2)
		if !bytes.Equal(got, []byte{byte(n), byte(n) ^ 71}) {
			t.Fatal(n, got)
		}
	}
}

func fecWire(data []byte, group uint16, index, k byte) []byte {
	p := append([]byte(nil), data...)
	p[0x1c+2] = 0x0b
	binary.LittleEndian.PutUint16(p[0x1c+8:], group)
	p[0x1c+10] = index
	p[0x1c+11] = k
	return p
}
func parityWire(group uint16, shards [][]byte) []byte {
	m := map[byte][]byte{}
	size := 0
	for i, p := range shards {
		m[byte(i)] = p[0x1c+16:]
		if len(m[byte(i)]) > size {
			size = len(m[byte(i)])
		}
	}
	data := interpolateShard(m, byte(len(shards)), size)
	p := makePkt(fragSpec{channel: 0x0c, paylen: uint16(len(data)), payload: data})
	return fecWire(p, group, byte(len(shards)), byte(len(shards)))
}
func TestFECValidationAndErasureRecovery(t *testing.T) {
	c, _ := timedClient()
	seq := uint16(0x4000)
	for group := uint16(0); group < 4; group++ {
		var shards [][]byte
		for i := 0; i < 3; i++ {
			shards = append(shards, testP(seq, uint32(seq), uint32(seq)*40))
			seq++
		}
		for i, p := range shards {
			if group == 3 && i == 1 {
				continue
			}
			c.parseDatagram(fecWire(p, group, byte(i), 3))
		}
		c.parseDatagram(parityWire(group, shards))
	}
	p := drainAll(c)
	if len(p) != 12 || c.fec.validated != 3 || c.v2.fecRecovered != 1 || c.v2.recovered != 1 {
		t.Fatalf("frames=%d FEC=%+v counters=%+v", len(p), c.fec, c.v2)
	}
	for i := 1; i < len(p); i++ {
		if p[i].Timestamp-p[i-1].Timestamp != 3600 {
			t.Fatal("recovered AU out of order")
		}
	}
}
func TestFECMismatchDisablesRecovery(t *testing.T) {
	c, _ := timedClient()
	shards := [][]byte{testP(0x4000, 1, 100), testP(0x4001, 2, 140), testP(0x4002, 3, 180)}
	for i, p := range shards {
		c.parseDatagram(fecWire(p, 0, byte(i), 3))
	}
	parity := parityWire(0, shards)
	parity[len(parity)-1] ^= 1
	c.parseDatagram(parity)
	if !c.fec.disabled || c.v2.fecInvalid != 1 || c.v2.fecRecovered != 0 {
		t.Fatal("unproven parity trusted")
	}
}
func TestFECObservedSequenceParity(t *testing.T) {
	// Header-only facts retained from live group 1: data seq4014..4027,
	// parity seq40e5. This checks the GF model against independent wire bytes.
	m := map[byte][]byte{}
	for i := 0; i < 20; i++ {
		m[byte(i)] = []byte{byte(0x14 + i), 0x40}
	}
	if got := interpolateShard(m, 20, 2); !bytes.Equal(got, []byte{0xe5, 0x40}) {
		t.Fatalf("live parity mismatch: %x", got)
	}
}
