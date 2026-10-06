package photo

import "encoding/binary"

// exifSignature opens the APP1 payload that carries EXIF metadata.
const exifSignature = "Exif\x00\x00"

// exifOrientationTag is the TIFF tag that names how the pixels are rotated
// relative to the scene. The value is one SHORT.
const exifOrientationTag = 0x0112

// orientationFromJPEG reads the EXIF orientation out of the JPEG bytes in
// b. It walks the marker segments ahead of the scan, and reads the tag from
// the first APP1 segment whose payload opens with the Exif signature.
//
// Every walk is bounds checked. A byte order it cannot name, a bad offset,
// a truncated segment, or a tag value outside 1 to 8 all read as 1. The
// pixels are what they are when their metadata cannot be trusted.
//
// Only JPEG carries an orientation this package reads. A PNG or WebP input
// is trusted to be upright.
func orientationFromJPEG(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	off := 2
	for off+4 <= len(b) {
		if b[off] != 0xFF {
			return 1
		}
		marker := b[off+1]
		if marker == 0xFF {
			// A fill byte ahead of a marker.
			off++
			continue
		}
		// TEM and the RST markers, together with SOI and EOI, stand
		// alone and carry no length. Reaching one means the scan or
		// the end of the stream arrived with no Exif segment found.
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9) {
			return 1
		}
		// The scan starts here. EXIF lives ahead of it.
		if marker == 0xDA {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(b[off+2 : off+4]))
		if segLen < 2 || off+2+segLen > len(b) {
			return 1
		}
		seg := b[off+4 : off+2+segLen]
		if marker == 0xE1 {
			if t := exifPayload(seg); t != nil {
				return tiffOrientation(t)
			}
		}
		off += 2 + segLen
	}
	return 1
}

// exifPayload returns the TIFF block of an APP1 segment payload when the
// payload opens with the Exif signature.
func exifPayload(seg []byte) []byte {
	if len(seg) >= len(exifSignature)+8 && string(seg[:len(exifSignature)]) == exifSignature {
		return seg[len(exifSignature):]
	}
	return nil
}

// tiffOrientation reads the orientation tag from the first image file
// directory of the TIFF block t. The block is the Exif payload after its
// signature. Byte order comes from the header, and every read is bounds
// checked against the block.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var order binary.ByteOrder = binary.LittleEndian
	switch {
	case t[0] == 'I' && t[1] == 'I':
	case t[0] == 'M' && t[1] == 'M':
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(t[2:4]) != 42 {
		return 1
	}
	ifd := int64(order.Uint32(t[4:8]))
	if ifd < 8 || ifd+2 > int64(len(t)) {
		return 1
	}
	entries := int64(order.Uint16(t[ifd : ifd+2]))
	if ifd+2+12*entries > int64(len(t)) {
		return 1
	}
	for i := int64(0); i < entries; i++ {
		e := t[ifd+2+12*i : ifd+2+12*i+12]
		if order.Uint16(e[0:2]) != exifOrientationTag {
			continue
		}
		// The orientation is one SHORT, and a SHORT value of count
		// one sits inline in the first two bytes of the value field.
		if order.Uint16(e[2:4]) != 3 || order.Uint32(e[4:8]) != 1 {
			return 1
		}
		if v := int(order.Uint16(e[8:10])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}
