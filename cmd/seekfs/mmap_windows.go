package main

import (
	"os"
	"reflect"
	"unsafe"

	"golang.org/x/sys/windows"
)

type mappedIndexFile struct {
	file    *os.File
	mapping windows.Handle
	addr    uintptr
	data    []byte
}

func mapIndexFile(path string) (*mappedIndexFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.Size() <= 0 {
		_ = f.Close()
		return nil, os.ErrInvalid
	}
	mapping, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(mapping)
		_ = f.Close()
		return nil, err
	}
	return &mappedIndexFile{
		file:    f,
		mapping: mapping,
		addr:    addr,
		data:    mappedViewBytes(addr, int(info.Size())),
	}, nil
}

func mappedViewBytes(addr uintptr, size int) []byte {
	var data []byte
	header := (*reflect.SliceHeader)(unsafe.Pointer(&data))
	header.Data = addr
	header.Len = size
	header.Cap = size
	return data
}

func (m *mappedIndexFile) close() error {
	if m == nil {
		return nil
	}
	var err error
	if m.addr != 0 {
		err = windows.UnmapViewOfFile(m.addr)
		m.addr = 0
		m.data = nil
	}
	if m.mapping != 0 {
		if closeErr := windows.CloseHandle(m.mapping); err == nil {
			err = closeErr
		}
		m.mapping = 0
	}
	if m.file != nil {
		if closeErr := m.file.Close(); err == nil {
			err = closeErr
		}
		m.file = nil
	}
	return err
}
