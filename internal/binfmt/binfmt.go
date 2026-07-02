// Package binfmt identifies the platform an executable targets by inspecting
// its object format (ELF or Mach-O). No toolchain is required, so both the
// CLI and the host agent can validate binaries before installing them.
package binfmt

import (
	"bytes"
	"debug/elf"
	"debug/macho"
)

// Detect reports the GOOS/GOARCH an executable targets. ok is false when the
// bytes are not a recognizable single-architecture ELF or Mach-O executable
// for a platform ship supports.
func Detect(data []byte) (goos, goarch string, ok bool) {
	if f, err := elf.NewFile(bytes.NewReader(data)); err == nil {
		defer f.Close()
		switch f.Machine {
		case elf.EM_X86_64:
			return "linux", "amd64", true
		case elf.EM_AARCH64:
			return "linux", "arm64", true
		}
		return "", "", false
	}
	if f, err := macho.NewFile(bytes.NewReader(data)); err == nil {
		defer f.Close()
		switch f.Cpu {
		case macho.CpuAmd64:
			return "darwin", "amd64", true
		case macho.CpuArm64:
			return "darwin", "arm64", true
		}
		return "", "", false
	}
	return "", "", false
}

// HasDarwinSlice reports whether a Mach-O universal binary contains a slice
// for the given architecture.
func HasDarwinSlice(data []byte, goarch string) bool {
	f, err := macho.NewFatFile(bytes.NewReader(data))
	if err != nil {
		return false
	}
	defer f.Close()
	for _, arch := range f.Arches {
		if (arch.Cpu == macho.CpuAmd64 && goarch == "amd64") ||
			(arch.Cpu == macho.CpuArm64 && goarch == "arm64") {
			return true
		}
	}
	return false
}
