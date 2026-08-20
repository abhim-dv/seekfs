//go:build windows && seekfs_ui

package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Small (16x16) shell file-type icons rendered to PNG data URIs for the results
// list, mirroring Everything's per-row icons. The icon is resolved by extension
// with SHGetFileInfoW (USEFILEATTRIBUTES so no disk access is needed), drawn
// into a 32bpp top-down DIB with DrawIconEx, and cached per extension key.

const (
	shgfiIcon              = 0x000000100
	shgfiSmallIcon         = 0x000000001
	shgfiUseFileAttributes = 0x000000010

	fileAttributeNormal    = 0x80
	fileAttributeDirectory = 0x10

	iconSize = 16
	biRGB    = 0
	diNormal = 0x0003
)

type shFileInfo struct {
	hIcon         uintptr
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

var (
	shell32Icon            = syscall.NewLazyDLL("shell32.dll")
	procSHGetFileInfoW     = shell32Icon.NewProc("SHGetFileInfoW")
	user32Icon             = syscall.NewLazyDLL("user32.dll")
	procGetDC              = user32Icon.NewProc("GetDC")
	procReleaseDC          = user32Icon.NewProc("ReleaseDC")
	procDrawIconEx         = user32Icon.NewProc("DrawIconEx")
	procDestroyIcon        = user32Icon.NewProc("DestroyIcon")
	gdi32Icon              = syscall.NewLazyDLL("gdi32.dll")
	procCreateCompatibleDC = gdi32Icon.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32Icon.NewProc("CreateDIBSection")
	procSelectObject       = gdi32Icon.NewProc("SelectObject")
	procDeleteDC           = gdi32Icon.NewProc("DeleteDC")
	procDeleteObject       = gdi32Icon.NewProc("DeleteObject")
)

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	bmiHeader bitmapInfoHeader
	bmiColors [3]uint32
}

var iconCache sync.Map // extension key -> PNG data URI

// GetFileIcon returns a data URI with the 16x16 shell icon for the file at
// path (or the folder icon when isDir). Icons are keyed by extension so a
// whole result set costs at most a handful of shell calls.
func (a *UIApp) GetFileIcon(path string, isDir bool) string {
	var key string
	attrs := uintptr(fileAttributeNormal)
	if isDir {
		key = "dir"
		attrs = fileAttributeDirectory
	} else {
		ext := strings.ToLower(filepath.Ext(path))
		if ext == "" {
			key = "file"
		} else {
			key = ext
		}
	}
	if v, ok := iconCache.Load(key); ok {
		return v.(string)
	}
	uri := renderFileIconDataURI(path, attrs)
	iconCache.Store(key, uri)
	return uri
}

func renderFileIconDataURI(path string, attrs uintptr) string {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	var sfi shFileInfo
	r, _, _ := procSHGetFileInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		attrs,
		uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi),
		uintptr(shgfiIcon|shgfiSmallIcon|shgfiUseFileAttributes),
	)
	if r == 0 || sfi.hIcon == 0 {
		return ""
	}
	defer procDestroyIcon.Call(sfi.hIcon)

	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return ""
	}
	defer procReleaseDC.Call(0, hdc)
	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	if mem == 0 {
		return ""
	}
	defer procDeleteDC.Call(mem)

	bmi := bitmapInfo{
		bmiHeader: bitmapInfoHeader{
			biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			biWidth:       iconSize,
			biHeight:      -iconSize, // top-down DIB
			biPlanes:      1,
			biBitCount:    32,
			biCompression: biRGB,
		},
	}
	var bits unsafe.Pointer
	hbmp, _, _ := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 || bits == nil {
		return ""
	}
	defer procDeleteObject.Call(hbmp)

	oldBmp, _, _ := procSelectObject.Call(mem, hbmp)
	procDrawIconEx.Call(mem, 0, 0, sfi.hIcon, iconSize, iconSize, 0, 0, diNormal)
	procSelectObject.Call(mem, oldBmp)

	raw := unsafe.Slice((*byte)(bits), iconSize*iconSize*4)
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			o := (y*iconSize + x) * 4
			b := raw[o]
			g := raw[o+1]
			r := raw[o+2]
			a := raw[o+3]
			// DIB bits are premultiplied; NRGBA expects straight alpha.
			if a > 0 && a < 255 {
				b = byte(uint32(b) * 255 / uint32(a))
				g = byte(uint32(g) * 255 / uint32(a))
				r = byte(uint32(r) * 255 / uint32(a))
			}
			img.Pix[o] = r
			img.Pix[o+1] = g
			img.Pix[o+2] = b
			img.Pix[o+3] = a
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
