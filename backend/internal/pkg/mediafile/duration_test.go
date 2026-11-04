package mediafile

import (
	"encoding/binary"
	"math"
	"testing"
)

func durationBox(kind string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:], kind)
	copy(box[8:], payload)
	return box
}

func durationMP4(version byte, scale uint32, ticks uint64, brand string) []byte {
	header := durationBox("ftyp", append([]byte(brand), make([]byte, 4)...))
	mvhd := make([]byte, 32)
	mvhd[0] = version
	if version == 1 {
		binary.BigEndian.PutUint32(mvhd[20:], scale)
		binary.BigEndian.PutUint64(mvhd[24:], ticks)
	} else {
		binary.BigEndian.PutUint32(mvhd[12:], scale)
		binary.BigEndian.PutUint32(mvhd[16:], uint32(ticks))
	}
	return append(header, durationBox("moov", durationBox("mvhd", mvhd))...)
}

func durationEBML(tag []byte, payload []byte) []byte {
	return append(append(append([]byte{}, tag...), byte(0x80|len(payload))), payload...)
}

func durationWebM(seconds float64, scale uint32, unknown bool, float32Value bool) []byte {
	s := binary.BigEndian.AppendUint32(nil, scale)
	d := binary.BigEndian.AppendUint64(nil, math.Float64bits(seconds*1e9/float64(scale)))
	if float32Value {
		d = binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(seconds*1e9/float64(scale))))
	}
	info := durationEBML([]byte{0x2a, 0xd7, 0xb1}, s)
	info = append(info, durationEBML([]byte{0x44, 0x89}, d)...)
	info = durationEBML([]byte{0x15, 0x49, 0xa9, 0x66}, info)
	segment := durationEBML([]byte{0x18, 0x53, 0x80, 0x67}, info)
	if unknown {
		segment = append([]byte{0x18, 0x53, 0x80, 0x67, 0xff}, info...)
	}
	return append(durationEBML([]byte{0x1a, 0x45, 0xdf, 0xa3}, nil), segment...)
}

func TestVideoDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want float64
	}{
		{"mp4-v0", durationMP4(0, 1000, 120000, "isom"), 120},
		{"mp4-v1", durationMP4(1, 1000, 120001, "mp42"), 120.001},
		{"quicktime", durationMP4(0, 24, 300, "qt  "), 12.5},
		{"webm", durationWebM(120, 1000000, false, false), 120},
		{"webm-unknown-segment", durationWebM(12.5, 100000, true, false), 12.5},
		{"webm-float32", durationWebM(60, 1000000, false, true), 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := VideoDuration(tc.data)
			if !ok || math.Abs(got-tc.want) > 0.000001 {
				t.Fatalf("duration=%v,%v want %v", got, ok, tc.want)
			}
		})
	}
}

func TestVideoDurationRejectsUnknownAndMalformed(t *testing.T) {
	valid := durationMP4(0, 1000, 6000, "isom")
	badBox := append([]byte{}, valid...)
	binary.BigEndian.PutUint32(badBox[16:], math.MaxUint32)
	for _, data := range [][]byte{
		nil, []byte("not a video"), valid[:len(valid)-1], badBox,
		durationMP4(0, 0, 10, "isom"), durationMP4(0, 1000, 0, "isom"),
		durationMP4(2, 1000, 6000, "isom"), durationMP4(0, 1000, math.MaxUint32, "isom"), durationMP4(1, 1000, math.MaxUint64, "isom"),
		durationWebM(math.NaN(), 1000000, false, false), durationWebM(math.Inf(1), 1000000, false, false),
		durationWebM(-1, 1000000, false, false), durationWebM(0, 1000000, false, false),
		{0x1a, 0x45, 0xdf, 0xa3, 0xff},
	} {
		if value, ok := VideoDuration(data); ok {
			t.Fatalf("invalid metadata accepted: duration=%v", value)
		}
	}
	webm := durationWebM(5, 1000000, false, false)
	for i := 0; i < len(webm); i++ {
		if _, ok := VideoDuration(webm[:i]); ok {
			t.Fatalf("truncated WebM accepted at %d", i)
		}
	}
}

func FuzzVideoDuration(f *testing.F) {
	f.Add(durationMP4(0, 1000, 6000, "isom"))
	f.Add(durationWebM(6, 1000000, true, false))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, ok := VideoDuration(data)
		if ok && (d <= 0 || math.IsInf(d, 0) || math.IsNaN(d)) {
			t.Fatalf("invalid duration %v", d)
		}
	})
}
