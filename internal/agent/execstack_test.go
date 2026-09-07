package agent

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildELFWithStackFlags writes a minimal but parseable ELF64 with one
// PT_GNU_STACK program header carrying the given flags.
func buildELFWithStackFlags(t *testing.T, path string, stackFlags uint32) {
	t.Helper()
	// ELF64 header is 64 bytes; one 56-byte phdr follows.
	ehdr := make([]byte, 64)
	copy(ehdr, "\x7fELF")
	ehdr[4] = 2                                      // ELFCLASS64
	ehdr[5] = 1                                      // little endian
	ehdr[6] = 1                                      // EV_CURRENT
	binary.LittleEndian.PutUint16(ehdr[0x10:], 2)    // ET_EXEC
	binary.LittleEndian.PutUint16(ehdr[0x12:], 0x3e) // EM_X86_64
	binary.LittleEndian.PutUint32(ehdr[0x14:], 1)    // version
	binary.LittleEndian.PutUint64(ehdr[0x20:], 64)   // e_phoff
	binary.LittleEndian.PutUint16(ehdr[0x36:], 56)   // e_phentsize
	binary.LittleEndian.PutUint16(ehdr[0x38:], 1)    // e_phnum
	binary.LittleEndian.PutUint16(ehdr[0x3a:], 64)   // e_ehsize
	binary.LittleEndian.PutUint16(ehdr[0x3c:], 56)   // e_phentsize again (shentsize slot unused)

	phdr := make([]byte, 56)
	binary.LittleEndian.PutUint32(phdr[0:], 0x6474e551) // PT_GNU_STACK
	binary.LittleEndian.PutUint32(phdr[4:], stackFlags)
	if err := os.WriteFile(path, append(ehdr, phdr...), 0o755); err != nil {
		t.Fatal(err)
	}
}

func stackFlagsOf(t *testing.T, path string) uint32 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	off, flags, err := gnuStackHeader(data)
	if err != nil {
		t.Fatalf("gnuStackHeader: %v", err)
	}
	if off != 64 {
		t.Fatalf("header offset = %d, want 64", off)
	}
	return flags
}

func TestClearExecStack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counterstrikesharp.so")
	buildELFWithStackFlags(t, path, 0x7) // PF_R|PF_W|PF_X
	if err := clearExecStack(path); err != nil {
		t.Fatalf("clearExecStack: %v", err)
	}
	if got := stackFlagsOf(t, path); got != 0x6 { // PF_R|PF_W
		t.Fatalf("flags = %#x, want RW (0x6)", got)
	}
	// The patch must be idempotent and leave clean binaries untouched.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := clearExecStack(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a clean binary must not be rewritten")
	}
}

// A file that is not ELF at all must produce an error, not a corrupt write.
func TestClearExecStackRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.so")
	if err := os.WriteFile(path, []byte("not an elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := clearExecStack(path); err == nil {
		t.Fatal("garbage accepted")
	}
	// and the file is left intact
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "not an elf" {
		t.Fatalf("garbage file modified: %q %v", b, err)
	}
}

// The real binary is ELF64; a 32-bit header table is also supported so the
// helper does not silently mis-parse other architectures.
func TestClearExecStackParsesELF32(t *testing.T) {
	// ELF32 header is 52 bytes, phdr 32 bytes; p_flags still follows p_type.
	ehdr := make([]byte, 52)
	copy(ehdr, "\x7fELF")
	ehdr[4] = 1 // ELFCLASS32
	ehdr[5] = 1
	ehdr[6] = 1
	binary.LittleEndian.PutUint16(ehdr[0x10:], 2)
	binary.LittleEndian.PutUint16(ehdr[0x12:], 3) // EM_386
	binary.LittleEndian.PutUint32(ehdr[0x14:], 1)
	binary.LittleEndian.PutUint32(ehdr[0x1c:], 52) // e_phoff
	binary.LittleEndian.PutUint16(ehdr[0x2a:], 32) // e_phentsize
	binary.LittleEndian.PutUint16(ehdr[0x2c:], 1)  // e_phnum
	phdr := make([]byte, 32)
	binary.LittleEndian.PutUint32(phdr[0:], 0x6474e551)
	binary.LittleEndian.PutUint32(phdr[4:], 0x7)
	path := filepath.Join(t.TempDir(), "x.so")
	if err := os.WriteFile(path, append(ehdr, phdr...), 0o755); err != nil {
		t.Fatal(err)
	}
	off, flags, err := gnuStackHeader(append(ehdr, phdr...))
	if err != nil {
		t.Fatal(err)
	}
	if off != 52 || flags != 7 {
		t.Fatalf("elf32 parse: off=%d flags=%#x", off, flags)
	}
	if err := clearExecStack(path); err != nil {
		t.Fatalf("clearExecStack elf32: %v", err)
	}
	data, _ := os.ReadFile(path)
	if _, flags, err := gnuStackHeader(data); err != nil || flags != 6 {
		t.Fatalf("elf32 flags after = %#x err=%v", flags, err)
	}
}

// elfWithExecStack builds a minimal ELF64 whose PT_GNU_STACK is RWE — the
// same shape as the cssharp release binary that Debian 13 refuses to load.
func elfWithExecStack(t *testing.T) []byte {
	t.Helper()
	ehdr := make([]byte, 64)
	copy(ehdr, "\x7fELF")
	ehdr[4] = 2                                      // ELFCLASS64
	ehdr[5] = 1                                      // little endian
	ehdr[6] = 1                                      // EV_CURRENT
	binary.LittleEndian.PutUint16(ehdr[0x10:], 2)    // ET_EXEC
	binary.LittleEndian.PutUint16(ehdr[0x12:], 0x3e) // EM_X86_64
	binary.LittleEndian.PutUint32(ehdr[0x14:], 1)    // version
	binary.LittleEndian.PutUint64(ehdr[0x20:], 64)   // e_phoff
	binary.LittleEndian.PutUint16(ehdr[0x36:], 56)   // e_phentsize
	binary.LittleEndian.PutUint16(ehdr[0x38:], 1)    // e_phnum
	binary.LittleEndian.PutUint16(ehdr[0x3a:], 64)   // e_ehsize
	phdr := make([]byte, 56)
	binary.LittleEndian.PutUint32(phdr[0:], 0x6474e551) // PT_GNU_STACK
	binary.LittleEndian.PutUint32(phdr[4:], 0x7)        // RWX
	return append(ehdr, phdr...)
}
