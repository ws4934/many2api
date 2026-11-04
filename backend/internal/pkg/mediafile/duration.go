package mediafile

import (
	"encoding/binary"
	"math"
)

// VideoDuration reads container duration metadata without decoding video.
// Unknown, truncated and non-finite durations are rejected, not treated as zero.
func VideoDuration(data []byte) (float64, bool) {
	var seconds float64
	switch VideoType(data) {
	case "video/mp4", "video/quicktime":
		seconds = mp4Duration(data)
	case "video/webm":
		seconds = webmDuration(data)
	}
	return seconds, seconds > 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0)
}

func mp4Box(data []byte, want string) []byte {
	for len(data) >= 8 {
		size, header := uint64(binary.BigEndian.Uint32(data)), 8
		switch size {
		case 0:
			size = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return nil
			}
			size, header = binary.BigEndian.Uint64(data[8:]), 16
		}
		if size < uint64(header) || size > uint64(len(data)) {
			return nil
		}
		if string(data[4:8]) == want {
			return data[header:int(size)]
		}
		data = data[int(size):]
	}
	return nil
}

func mp4Duration(data []byte) float64 {
	mvhd := mp4Box(mp4Box(data, "moov"), "mvhd")
	if len(mvhd) < 20 {
		return 0
	}
	var scale uint32
	var duration uint64
	switch mvhd[0] {
	case 0:
		scale, duration = binary.BigEndian.Uint32(mvhd[12:]), uint64(binary.BigEndian.Uint32(mvhd[16:]))
		if duration == math.MaxUint32 {
			return 0 // ISO BMFF unknown duration
		}
	case 1:
		if len(mvhd) < 32 {
			return 0
		}
		scale, duration = binary.BigEndian.Uint32(mvhd[20:]), binary.BigEndian.Uint64(mvhd[24:])
		if duration == math.MaxUint64 {
			return 0
		}
	default:
		return 0
	}
	if scale == 0 {
		return 0
	}
	return float64(duration) / float64(scale)
}

// ebmlInt returns an ID (marker retained) or size (marker removed). All-one
// sizes are marked unknown; only the outer Segment may use an unknown size.
func ebmlInt(data []byte, id bool) (value uint64, width int, unknown bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false
	}
	width = 1
	for data[0]&(0x80>>uint(width-1)) == 0 {
		width++
	}
	if width > len(data) || (id && width > 4) {
		return 0, 0, false
	}
	for _, b := range data[:width] {
		value = value<<8 | uint64(b)
	}
	if !id {
		mask := uint64(1)<<(7*width) - 1
		value &= mask
		unknown = value == mask
	}
	return
}

func ebmlElement(data []byte) (tag uint64, body, rest []byte, ok bool) {
	tag, n, _ := ebmlInt(data, true)
	if n == 0 {
		return 0, nil, nil, false
	}
	size, m, unknown := ebmlInt(data[n:], false)
	if m == 0 {
		return 0, nil, nil, false
	}
	data = data[n+m:]
	if unknown {
		if tag != 0x18538067 { // Segment
			return 0, nil, nil, false
		}
		size = uint64(len(data))
	}
	if size > uint64(len(data)) {
		return 0, nil, nil, false
	}
	return tag, data[:int(size)], data[int(size):], true
}

func ebmlChild(data []byte, want uint64) []byte {
	for len(data) > 0 {
		tag, body, rest, ok := ebmlElement(data)
		if !ok {
			return nil
		}
		if tag == want {
			return body
		}
		data = rest
	}
	return nil
}

func webmDuration(data []byte) float64 {
	info := ebmlChild(ebmlChild(data, 0x18538067), 0x1549a966)
	scale, duration := uint64(1000000), float64(0)
	for len(info) > 0 {
		tag, body, rest, ok := ebmlElement(info)
		if !ok {
			return 0
		}
		switch tag {
		case 0x2ad7b1: // TimestampScale, nanoseconds
			if len(body) == 0 || len(body) > 8 {
				return 0
			}
			scale = 0
			for _, b := range body {
				scale = scale<<8 | uint64(b)
			}
		case 0x4489: // Duration, in TimestampScale units
			switch len(body) {
			case 4:
				duration = float64(math.Float32frombits(binary.BigEndian.Uint32(body)))
			case 8:
				duration = math.Float64frombits(binary.BigEndian.Uint64(body))
			default:
				return 0
			}
		}
		info = rest
	}
	return duration * float64(scale) / 1e9
}
