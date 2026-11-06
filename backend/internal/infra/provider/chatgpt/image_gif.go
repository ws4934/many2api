package chatgpt

import (
	"bytes"
	"encoding/binary"
	"image/gif"
)

// validateGIFOutput bounds frame allocation before DecodeAll, then validates
// every frame. image.Decode alone would accept a GIF truncated after frame one.
// The original bytes are archived, preserving delays, disposal, loop and palette.
func validateGIFOutput(raw []byte) error {
	bad := func() error {
		return invalidImageOutput("OpenAI GIF output is corrupt or exceeds 16 frames / 64 million decoded pixels")
	}
	if len(raw) < 13 || (string(raw[:6]) != "GIF87a" && string(raw[:6]) != "GIF89a") {
		return bad()
	}
	width, height := int64(binary.LittleEndian.Uint16(raw[6:8])), int64(binary.LittleEndian.Uint16(raw[8:10]))
	pos := 13
	if raw[10]&0x80 != 0 {
		pos += 3 << ((raw[10] & 7) + 1)
	}
	skipBlocks := func() bool {
		for pos < len(raw) {
			n := int(raw[pos])
			pos++
			if n == 0 {
				return true
			}
			pos += n
		}
		return false
	}
	frames := 0
	for pos < len(raw) {
		block := raw[pos]
		pos++
		switch block {
		case 0x21: // Extension label followed by sub-blocks.
			if pos >= len(raw) {
				return bad()
			}
			pos++
			if !skipBlocks() {
				return bad()
			}
		case 0x2c: // Image descriptor and optional local palette.
			if pos+9 > len(raw) {
				return bad()
			}
			frames++
			if frames > 16 || width*height*int64(frames) > 64_000_000 {
				return bad()
			}
			packed := raw[pos+8]
			pos += 9
			if packed&0x80 != 0 {
				pos += 3 << ((packed & 7) + 1)
			}
			if pos >= len(raw) {
				return bad()
			}
			pos++ // LZW minimum code size.
			if !skipBlocks() {
				return bad()
			}
		case 0x3b:
			if frames < 2 {
				return invalidImageOutput("OpenAI returned a still GIF instead of an animation")
			}
			if _, err := gif.DecodeAll(bytes.NewReader(raw)); err != nil {
				return bad()
			}
			return nil
		default:
			return bad()
		}
	}
	return bad()
}
