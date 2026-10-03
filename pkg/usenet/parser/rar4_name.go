package parser

import (
	"bytes"
	"unicode/utf16"
)

// RAR4 Unicode name encoding: each flag byte carries four 2-bit opcodes.
const (
	rar4NameOpLow      = 0 // one byte: the low byte, high byte zero
	rar4NameOpHigh     = 1 // one byte: the low byte, high byte from the header
	rar4NameOpWide     = 2 // two bytes: a little-endian UTF-16 unit
	rar4NameOpCopy     = 3 // a run copied from the OEM name, optionally shifted
	rar4NameOpBits     = 2
	rar4NameFlagBits   = 8
	rar4NameWideLen    = 2 // bytes in an rar4NameOpWide unit
	rar4NameHeaderLen  = 2 // the high byte and the first flag byte
	rar4NameRunMask    = 0x7f
	rar4NameRunShifted = 0x80
	rar4NameRunMin     = 2
)

// decodeRAR4Name decodes the name of a RAR4 header with the Unicode flag. The
// field is either UTF-8, or an OEM name, a NUL, and RAR's compact UTF-16
// encoding of the real name (unrar's EncodeFileName). A truncated encoding
// yields what decoded before the cut; an empty one, the OEM name.
func decodeRAR4Name(field []byte) string {
	oem, enc, found := bytes.Cut(field, []byte{0})
	if !found {
		return string(field)
	}
	if len(enc) < rar4NameHeaderLen {
		return string(oem)
	}
	d := rar4NameDecoder{oem: oem, enc: enc[1:], high: uint16(enc[0]) << rar4NameFlagBits}
	for len(d.units) < len(oem) {
		if !d.step() {
			break
		}
	}
	return string(utf16.Decode(d.units))
}

// rar4NameDecoder holds the state of one decodeRAR4Name run.
type rar4NameDecoder struct {
	oem      []byte
	enc      []byte
	high     uint16
	units    []uint16
	flags    byte
	flagBits int
}

// next consumes one encoded byte; ok is false when none is left.
func (d *rar4NameDecoder) next() (byte, bool) {
	if len(d.enc) == 0 {
		return 0, false
	}
	b := d.enc[0]
	d.enc = d.enc[1:]
	return b, true
}

// step decodes one opcode and reports whether decoding can go on.
func (d *rar4NameDecoder) step() bool {
	if d.flagBits == 0 {
		flags, ok := d.next()
		if !ok {
			return false
		}
		d.flags, d.flagBits = flags, rar4NameFlagBits
	}
	op := d.flags >> (rar4NameFlagBits - rar4NameOpBits)
	d.flags <<= rar4NameOpBits
	d.flagBits -= rar4NameOpBits

	switch op {
	case rar4NameOpLow, rar4NameOpHigh:
		low, ok := d.next()
		if !ok {
			return false
		}
		unit := uint16(low)
		if op == rar4NameOpHigh {
			unit |= d.high
		}
		d.units = append(d.units, unit)
	case rar4NameOpWide:
		if len(d.enc) < rar4NameWideLen {
			return false
		}
		d.units = append(d.units, uint16(d.enc[0])|uint16(d.enc[1])<<rar4NameFlagBits)
		d.enc = d.enc[rar4NameWideLen:]
	case rar4NameOpCopy:
		return d.copyRun()
	}
	return true
}

// copyRun copies a run of OEM name bytes, shifted into the header's high
// byte when the run asks for it.
func (d *rar4NameDecoder) copyRun() bool {
	run, ok := d.next()
	if !ok {
		return false
	}
	n := min(int(run&rar4NameRunMask)+rar4NameRunMin, len(d.oem)-len(d.units))
	src := d.oem[len(d.units) : len(d.units)+n]
	if run&rar4NameRunShifted == 0 {
		for _, c := range src {
			d.units = append(d.units, uint16(c))
		}
		return true
	}
	shift, ok := d.next()
	if !ok {
		return false
	}
	for _, c := range src {
		d.units = append(d.units, uint16(c+shift)|d.high)
	}
	return true
}
