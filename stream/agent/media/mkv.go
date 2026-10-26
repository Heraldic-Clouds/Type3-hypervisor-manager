package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Minimal live Matroska demuxer for the ffmpeg encoder path. ffmpeg writes one
// cluster per packet (-cluster_size_limit 1), so every SimpleBlock arrives as
// soon as it is encoded and carries its own length: no frame of delay is
// added waiting for the next start code, unlike splitting raw Annex B.

const (
	idEBML        = 0x1A45DFA3
	idSegment     = 0x18538067
	idCluster     = 0x1F43B675
	idTimecode    = 0xE7
	idSimpleBlock = 0xA3
	idBlockGroup  = 0xA0
	idBlock       = 0xA1
	idTracks      = 0x1654AE6B
	idTrackEntry  = 0xAE
	idTrackNumber = 0xD7
	idCodecID     = 0x86
	idCodecPriv   = 0x63A2
	idInfo        = 0x1549A966
	idTimecodeSc  = 0x2AD7B1
)

var errUnknownSize = errors.New("unknown size")

type mkvTrack struct {
	number  uint64
	codecID string
	priv    []byte
}

// MKVReader yields encoded frames as Annex B (H.264/H.265) or low-overhead
// OBUs (AV1), with parameter sets / sequence headers prepended to keyframes.
type MKVReader struct {
	r        *bufio.Reader
	video    *mkvTrack
	audio    *mkvTrack
	scale    uint64 // ns per timecode tick
	cluster  int64
	nalLen   int
	params   []byte // Annex B SPS/PPS(/VPS) or AV1 sequence header OBUs
	codec    string
	gotFirst bool
}

func NewMKVReader(r io.Reader) *MKVReader {
	return &MKVReader{r: bufio.NewReaderSize(r, 256<<10), scale: 1_000_000}
}

// Codec returns the video codec once the Tracks element was read.
func (m *MKVReader) Codec() string { return m.codec }

func readVint(r *bufio.Reader, keepMarker bool) (uint64, int, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	n := 1
	for mask := byte(0x80); n <= 8 && b&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 {
		return 0, 0, errors.New("mkv: bad vint")
	}
	v := uint64(b)
	if !keepMarker {
		v &= uint64(0xFF >> n)
	}
	allOnes := v == uint64(0xFF>>n)
	for i := 1; i < n; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		v = v<<8 | uint64(c)
		if c != 0xFF {
			allOnes = false
		}
	}
	if !keepMarker && allOnes {
		return v, n, errUnknownSize
	}
	return v, n, nil
}

func vintBytes(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	n := 1
	for mask := byte(0x80); n <= 8 && b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > len(b) {
		return 0, 0
	}
	v := uint64(b[0]) & uint64(0xFF>>n)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n
}

// Packet is one demuxed frame.
type Packet struct {
	Video bool
	Key   bool
	PTS   time.Duration
	Data  []byte
}

// Next returns the next video or audio packet.
func (m *MKVReader) Next() (Packet, error) {
	for {
		id, _, err := readVint(m.r, true)
		if err != nil {
			return Packet{}, err
		}
		size, _, err := readVint(m.r, false)
		unknown := errors.Is(err, errUnknownSize)
		if err != nil && !unknown {
			return Packet{}, err
		}
		switch id {
		case idSegment, idCluster, idBlockGroup:
			continue // descend into master elements
		}
		if unknown {
			return Packet{}, fmt.Errorf("mkv: unknown size for element %x", id)
		}
		if size > 64<<20 {
			return Packet{}, errors.New("mkv: element too large")
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(m.r, buf); err != nil {
			return Packet{}, err
		}
		switch id {
		case idInfo:
			m.parseInfo(buf)
		case idTracks:
			if err := m.parseTracks(buf); err != nil {
				return Packet{}, err
			}
		case idTimecode:
			m.cluster = int64(uintBE(buf))
		case idSimpleBlock, idBlock:
			p, ok, err := m.block(buf, id == idSimpleBlock)
			if err != nil {
				return Packet{}, err
			}
			if ok {
				return p, nil
			}
		}
	}
}

func uintBE(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

type element struct {
	id   uint64
	data []byte
}

func children(b []byte) []element {
	var out []element
	r := bufio.NewReader(bytes.NewReader(b))
	for {
		id, _, err := readVint(r, true)
		if err != nil {
			return out
		}
		size, _, err := readVint(r, false)
		if err != nil || size > uint64(len(b)) {
			return out
		}
		d := make([]byte, size)
		if _, err := io.ReadFull(r, d); err != nil {
			return out
		}
		out = append(out, element{id, d})
	}
}

func (m *MKVReader) parseInfo(b []byte) {
	for _, e := range children(b) {
		if e.id == idTimecodeSc {
			m.scale = uintBE(e.data)
		}
	}
}

func (m *MKVReader) parseTracks(b []byte) error {
	for _, te := range children(b) {
		if te.id != idTrackEntry {
			continue
		}
		t := &mkvTrack{}
		for _, e := range children(te.data) {
			switch e.id {
			case idTrackNumber:
				t.number = uintBE(e.data)
			case idCodecID:
				t.codecID = string(bytes.TrimRight(e.data, "\x00"))
			case idCodecPriv:
				t.priv = e.data
			}
		}
		switch t.codecID {
		case "V_MPEG4/ISO/AVC":
			m.video, m.codec = t, "h264"
			return m.avcC(t.priv)
		case "V_MPEGH/ISO/HEVC":
			m.video, m.codec = t, "h265"
			return m.hvcC(t.priv)
		case "V_AV1":
			m.video, m.codec = t, "av1"
			if len(t.priv) > 4 {
				m.params = append([]byte(nil), t.priv[4:]...)
			}
		case "A_OPUS":
			m.audio = t
		}
	}
	return nil
}

var startCode = []byte{0, 0, 0, 1}

func (m *MKVReader) avcC(p []byte) error {
	if len(p) < 7 {
		return errors.New("mkv: short avcC")
	}
	m.nalLen = int(p[4]&3) + 1
	i, out := 6, []byte{}
	nsps := int(p[5] & 0x1f)
	for k := 0; k < nsps && i+2 <= len(p); k++ {
		l := int(binary.BigEndian.Uint16(p[i:]))
		i += 2
		if i+l > len(p) {
			return errors.New("mkv: bad avcC")
		}
		out = append(append(out, startCode...), p[i:i+l]...)
		i += l
	}
	if i < len(p) {
		npps := int(p[i])
		i++
		for k := 0; k < npps && i+2 <= len(p); k++ {
			l := int(binary.BigEndian.Uint16(p[i:]))
			i += 2
			if i+l > len(p) {
				return errors.New("mkv: bad avcC")
			}
			out = append(append(out, startCode...), p[i:i+l]...)
			i += l
		}
	}
	m.params = out
	return nil
}

func (m *MKVReader) hvcC(p []byte) error {
	if len(p) < 23 {
		return errors.New("mkv: short hvcC")
	}
	m.nalLen = int(p[21]&3) + 1
	n, i, out := int(p[22]), 23, []byte{}
	for a := 0; a < n && i+3 <= len(p); a++ {
		cnt := int(binary.BigEndian.Uint16(p[i+1:]))
		i += 3
		for k := 0; k < cnt && i+2 <= len(p); k++ {
			l := int(binary.BigEndian.Uint16(p[i:]))
			i += 2
			if i+l > len(p) {
				return errors.New("mkv: bad hvcC")
			}
			out = append(append(out, startCode...), p[i:i+l]...)
			i += l
		}
	}
	m.params = out
	return nil
}

func (m *MKVReader) block(b []byte, simple bool) (Packet, bool, error) {
	track, n := vintBytes(b)
	if n == 0 || len(b) < n+3 {
		return Packet{}, false, errors.New("mkv: short block")
	}
	tc := int16(binary.BigEndian.Uint16(b[n:]))
	flags := b[n+2]
	if flags&0x06 != 0 {
		return Packet{}, false, errors.New("mkv: laced blocks unsupported")
	}
	data := b[n+3:]
	pts := time.Duration((m.cluster+int64(tc))*int64(m.scale)) * time.Nanosecond
	key := simple && flags&0x80 != 0
	switch {
	case m.video != nil && track == m.video.number:
		if !m.gotFirst {
			key = true // the first frame of an ffmpeg stream is always an IDR
			m.gotFirst = true
		}
		out, err := m.toBitstream(data, key)
		return Packet{Video: true, Key: key, PTS: pts, Data: out}, err == nil, err
	case m.audio != nil && track == m.audio.number:
		return Packet{PTS: pts, Data: data, Key: true}, true, nil
	}
	return Packet{}, false, nil
}

func (m *MKVReader) toBitstream(d []byte, key bool) ([]byte, error) {
	if m.codec == "av1" {
		if key && len(m.params) > 0 && (len(d) == 0 || (d[0]>>3)&0xF != 1) {
			return append(append([]byte(nil), m.params...), d...), nil
		}
		return d, nil
	}
	out := make([]byte, 0, len(d)+len(m.params)+16)
	if key {
		out = append(out, m.params...)
	}
	for i := 0; i < len(d); {
		if i+m.nalLen > len(d) {
			return nil, errors.New("mkv: truncated NAL length")
		}
		var l int
		for k := 0; k < m.nalLen; k++ {
			l = l<<8 | int(d[i+k])
		}
		i += m.nalLen
		if l < 0 || i+l > len(d) {
			return nil, errors.New("mkv: truncated NAL")
		}
		out = append(append(out, startCode...), d[i:i+l]...)
		i += l
	}
	return out, nil
}
