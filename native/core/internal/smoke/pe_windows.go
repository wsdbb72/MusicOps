//go:build windows

package smoke

import (
	"debug/pe"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// peVersion is the version resource embedded in a packaged Windows
// executable. The numbers come from VS_FIXEDFILEINFO and the strings
// from the translation goversioninfo writes (LANGID 2052, charset 1200).
type peVersion struct {
	FileMajor, FileMinor, FilePatch, FileBuild                          uint16
	ProductMajor, ProductMinor, ProductPatch, ProductBuild              uint16
	Subsystem                                                           uint16
	Company, Description, FileVersion, ProductVersion, OriginalFilename string
	LangID, Charset                                                     uint16
}

func readPEVersion(path string) (peVersion, error) {
	var out peVersion
	file, err := pe.Open(path)
	if err != nil {
		return out, err
	}
	defer file.Close()
	switch header := file.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		out.Subsystem = header.Subsystem
	case *pe.OptionalHeader32:
		out.Subsystem = header.Subsystem
	default:
		return out, fmt.Errorf("unexpected PE optional header")
	}

	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return out, err
	}
	block := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&block[0])); err != nil {
		return out, err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var fixedLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&block[0]), `\`, unsafe.Pointer(&fixed), &fixedLen); err != nil {
		return out, err
	}
	if fixed == nil || fixedLen < uint32(unsafe.Sizeof(*fixed)) || fixed.Signature != 0xFEEF04BD {
		return out, fmt.Errorf("version resource has no fixed info")
	}
	out.FileMajor = uint16(fixed.FileVersionMS >> 16)
	out.FileMinor = uint16(fixed.FileVersionMS)
	out.FilePatch = uint16(fixed.FileVersionLS >> 16)
	out.FileBuild = uint16(fixed.FileVersionLS)
	out.ProductMajor = uint16(fixed.ProductVersionMS >> 16)
	out.ProductMinor = uint16(fixed.ProductVersionMS)
	out.ProductPatch = uint16(fixed.ProductVersionLS >> 16)
	out.ProductBuild = uint16(fixed.ProductVersionLS)

	var trans *uint16
	var transLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&block[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&trans), &transLen); err != nil {
		return out, err
	}
	if trans == nil || transLen < 4 {
		return out, fmt.Errorf("version resource has no translation")
	}
	out.LangID = *trans
	out.Charset = *(*uint16)(unsafe.Add(unsafe.Pointer(trans), 2))
	prefix := fmt.Sprintf(`\StringFileInfo\%04x%04x\`, out.LangID, out.Charset)
	for _, item := range []struct {
		name string
		dest *string
	}{
		{"CompanyName", &out.Company},
		{"FileDescription", &out.Description},
		{"FileVersion", &out.FileVersion},
		{"ProductVersion", &out.ProductVersion},
		{"OriginalFilename", &out.OriginalFilename},
	} {
		value, err := versionString(block, prefix+item.name)
		if err != nil {
			return out, fmt.Errorf("%s: %w", item.name, err)
		}
		*item.dest = value
	}
	return out, nil
}

func versionString(block []byte, query string) (string, error) {
	var ptr *uint16
	var size uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&block[0]), query, unsafe.Pointer(&ptr), &size); err != nil {
		return "", err
	}
	if ptr == nil || size == 0 {
		return "", fmt.Errorf("empty")
	}
	chars := unsafe.Slice(ptr, size)
	end := len(chars)
	if end > 0 && chars[end-1] == 0 {
		end--
	}
	return windows.UTF16ToString(chars[:end]), nil
}
