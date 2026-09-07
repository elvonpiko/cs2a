package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// clearExecStack rewrites the ELF PT_GNU_STACK program header of path from
// RWE to RW.
//
// CounterStrikeSharp's release binary is linked with an executable stack
// (GNU_STACK flags RWE). Modern hardening in Debian 13 / Ubuntu 25.04+ makes
// the loader refuse such objects outright — "cannot enable executable stack
// as shared object requires: Invalid argument" — so the panel installed
// cssharp "successfully" and the server then failed to boot with the reason
// buried in the journal. patchelf --clear-execstack fixes it by hand; doing
// it in the installer means the operator never meets the problem.
//
// The patch is one field: PT_GNU_STACK's p_flags loses PF_X. Everything else
// is left byte-for-byte alone, and the write is atomic + ownership-preserving
// like every other file cs2a touches in the game tree.
func clearExecStack(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("execstack: read: %w", err)
	}
	// Find the PT_GNU_STACK header and check its flags first; a binary that
	// does not need the patch must not get a pointless rewrite.
	off, flags, err := gnuStackHeader(data)
	if err != nil {
		return fmt.Errorf("execstack: %s: %w", path, err)
	}
	const pfX = 0x1 // PF_X
	if flags&pfX == 0 {
		return nil // already clean
	}
	// p_flags sits at phdr + 4 (p_type, p_flags, p_offset, ...).
	flagOff := off + 4
	binary.LittleEndian.PutUint32(data[flagOff:], flags & ^uint32(pfX))
	// Keep the mode and owner the game user already has on this file.
	if err := atomicWrite(path, data, 0o755); err != nil {
		return fmt.Errorf("execstack: write: %w", err)
	}
	return nil
}

// gnuStackHeader locates the PT_GNU_STACK program header and returns its file
// offset and flags. The walk is over raw bytes rather than debug/elf because
// the real cssharp binary must be patched even when its section table is
// unusual, and debug/elf validates more than this patch needs.
func gnuStackHeader(data []byte) (off int64, flags uint32, err error) {
	if len(data) < 0x34 || !bytes.HasPrefix(data, []byte("\x7fELF")) {
		return 0, 0, fmt.Errorf("not an ELF file")
	}
	var (
		phoff     uint64
		phentsize uint16
		phnum     uint16
	)
	switch data[4] { // EI_CLASS
	case 2: // ELFCLASS64
		phoff = binary.LittleEndian.Uint64(data[0x20:])
		phentsize = binary.LittleEndian.Uint16(data[0x36:])
		phnum = binary.LittleEndian.Uint16(data[0x38:])
	case 1: // ELFCLASS32
		phoff = uint64(binary.LittleEndian.Uint32(data[0x1c:]))
		phentsize = binary.LittleEndian.Uint16(data[0x2a:])
		phnum = binary.LittleEndian.Uint16(data[0x2c:])
	default:
		return 0, 0, fmt.Errorf("unknown ELF class %d", data[4])
	}
	if phnum == 0 || phentsize == 0 {
		return 0, 0, fmt.Errorf("no program headers")
	}
	for i := uint16(0); i < phnum; i++ {
		p := int64(phoff) + int64(i)*int64(phentsize)
		if int(p)+8 > len(data) {
			return 0, 0, fmt.Errorf("program header %d out of range", i)
		}
		ptype := binary.LittleEndian.Uint32(data[p:])
		const ptGNUStack = 0x6474e551
		if ptype != ptGNUStack {
			continue
		}
		// p_flags follows p_type in both classes.
		return p, binary.LittleEndian.Uint32(data[p+4:]), nil
	}
	return 0, 0, fmt.Errorf("no PT_GNU_STACK header")
}
