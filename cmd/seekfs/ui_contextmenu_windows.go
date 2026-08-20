//go:build windows && seekfs_ui

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// COM / shell constants (see the specs for the exact values).
const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4

	rpcEChangedMode = 0x80010106

	cmfNormal  = 0x0
	cmfExplore = 0x4

	tpmReturnCommand = 0x0100
	tpmRightButton   = 0x0002
	tpmLeftButton    = 0x0000

	cmicMaskPTInvoke = 0x20000000
	cmicMaskUnicode  = 0x4000
	swShowNormal     = 1
	wmNull           = 0x0000

	// HWND_MESSAGE is (HWND)-3, used as the parent for message-only windows.
	hwndMessage = ^uintptr(2)

	contextMenuIDFirst = 1
	contextMenuIDLast  = 0x7FFF

	// Vtable slot indices after IUnknown (0/1/2).
	iContextMenuQueryContextMenuSlot = 3
	iContextMenuInvokeCommandSlot    = 4
)

// parentKeyOfPidl returns a string key for the parent folder of an absolute
// ITEMIDLIST. A PIDL is a chain of SHITEMID records (each starting with a
// uint16 cb field, terminated by a cb==0 record); the parent folder is the
// prefix that ends just before the last real element. Two items share a
// parent iff these prefix bytes are identical.
func parentKeyOfPidl(pidl unsafe.Pointer) string {
	p := pidl
	lastElem := pidl
	for {
		cb := *(*uint16)(p)
		if cb == 0 {
			break
		}
		lastElem = p
		p = unsafe.Add(p, uintptr(cb))
	}
	// Copy the prefix up to (but not including) the last element.
	parent := unsafe.Slice((*byte)(unsafe.Add(pidl, 0)), int(uintptr(lastElem)-uintptr(pidl)))
	return string(parent)
}

// MAKEINTRESOURCEW: for a resource id that fits in the low word, the LPSTR
// encoding of the id is the id itself (upper word is zero).
func makeIntResourceW(id uint16) uintptr {
	return uintptr(id)
}

var (
	shell32DLLContextMenu = syscall.NewLazyDLL("shell32.dll")
	procSHParseDisplayName          = shell32DLLContextMenu.NewProc("SHParseDisplayName")
	procSHGetDesktopFolder           = shell32DLLContextMenu.NewProc("SHGetDesktopFolder")
	procSHBindToParent               = shell32DLLContextMenu.NewProc("SHBindToParent")
	procSHCreateDefaultContextMenu   = shell32DLLContextMenu.NewProc("SHCreateDefaultContextMenu")

	kernel32DLLContextMenu     = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleHandleW       = kernel32DLLContextMenu.NewProc("GetModuleHandleW")

	user32DLLContextMenu    = syscall.NewLazyDLL("user32.dll")
	procCreatePopupMenu     = user32DLLContextMenu.NewProc("CreatePopupMenu")
	procTrackPopupMenuEx    = user32DLLContextMenu.NewProc("TrackPopupMenuEx")
	procDestroyMenu         = user32DLLContextMenu.NewProc("DestroyMenu")
	procGetForegroundWindow = user32DLLContextMenu.NewProc("GetForegroundWindow")
	procGetCursorPos        = user32DLLContextMenu.NewProc("GetCursorPos")
	procGetWindowRect       = user32DLLContextMenu.NewProc("GetWindowRect")
	procSetForegroundWindow = user32DLLContextMenu.NewProc("SetForegroundWindow")
	procPostMessage         = user32DLLContextMenu.NewProc("PostMessageW")
	procWindowFromPoint     = user32DLLContextMenu.NewProc("WindowFromPoint")
	procCreateWindowExW     = user32DLLContextMenu.NewProc("CreateWindowExW")
	procRegisterClassExW    = user32DLLContextMenu.NewProc("RegisterClassExW")
	procDefWindowProcW      = user32DLLContextMenu.NewProc("DefWindowProcW")
	procDestroyWindow       = user32DLLContextMenu.NewProc("DestroyWindow")
)

var (
	// IID_IContextMenu {000214e4-0000-0000-c000-000000000046}
	iidContextMenu = windows.GUID{
		Data1: 0x000214e4,
		Data2: 0x0000,
		Data3: 0x0000,
		Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
	}
	// IID_IShellFolder {000214e6-0000-0000-c000-000000000046}
	iidShellFolder = windows.GUID{
		Data1: 0x000214e6,
		Data2: 0x0000,
		Data3: 0x0000,
		Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
	}
)

// IContextMenu vtable: IUnknown(0/1/2), QueryContextMenu(3),
// InvokeCommand(4), GetCommandString(5). All present, GetCommandString unused.
type iContextMenuVtbl struct {
	queryInterface   uintptr
	addRef           uintptr
	release          uintptr
	queryContextMenu uintptr
	invokeCommand    uintptr
	getCommandString uintptr
}

// POINT matches the Windows SDK POINT struct.
type point struct {
	X int32
	Y int32
}

// RECT matches the Windows SDK RECT struct.
type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

// CMINVOKECOMMANDINFOEX, matching the Windows SDK layout. dwNoWait is only
// present on Win7+; including it is safe and matches current Windows.
type cminvokeCommandInfoEx struct {
	cbSize        uint32
	fMask         uint32
	hwnd          uintptr
	lpVerb        uintptr
	lpParameters  uintptr
	lpDirectory   uintptr
	nShow         int32
	dwHotKey      uint32
	hIcon         uintptr
	lpTitle       uintptr
	lpVerbW       uintptr
	lpParametersW uintptr
	lpDirectoryW  uintptr
	lpTitleW      uintptr
	ptInvoke      point
	dwNoWait      uint32
}

// callVtbl invokes a COM vtable method: reads the fn pointer at slot index
// `slot` from the interface `iface` and calls it with `this` + args.
func callVtbl(iface unsafe.Pointer, slot uint32, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(iface)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(iface))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func releaseCom(iface unsafe.Pointer) {
	if iface == nil {
		return
	}
	vtbl := *(*unsafe.Pointer)(iface)
	release := *(*uintptr)(unsafe.Add(vtbl, 2*unsafe.Sizeof(uintptr(0))))
	syscall.SyscallN(release, uintptr(iface))
}

func comHResultErr(hr uintptr) error {
	return fmt.Errorf("COM call failed with HRESULT 0x%08X", uint32(hr))
}

// foregroundWindowHandle returns the HWND of the foreground window, or 0.
func foregroundWindowHandle() uintptr {
	r, _, _ := procGetForegroundWindow.Call()
	return r
}

// cursorPoint returns the current cursor position in screen coordinates.
func cursorPoint() (point, bool) {
	var pt point
	r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	return pt, r != 0
}

var menuOwnerClass = "seekfs_context_menu_owner"

// wndClassEx mirrors the WNDCLASSEXW layout (RegisterClassExW).
type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

// createMenuOwnerWindow registers a window class and creates a message-only
// owner window for the shell context menu's modal message loop. The window is
// created on the calling thread so the TrackPopupMenuEx modal loop and its
// owner share a message queue. Returns 0 on failure (callers fall back to the
// foreground window as owner).
func createMenuOwnerWindow() uintptr {
	wndProc := windows.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	})
	className, _ := windows.UTF16PtrFromString(menuOwnerClass)
	inst := instanceHandle()
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   wndProc,
		HInstance:     inst,
		LpszClassName: uintptr(unsafe.Pointer(className)),
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := procCreateWindowExW.Call(
		0, // dwExStyle
		uintptr(unsafe.Pointer(className)),
		0,   // window name
		0x0, // style: not visible, no decorations
		0, 0, 0, 0,
		hwndMessage, // HWND_MESSAGE parent -> message-only window
		0,           // hMenu
		inst,        // hInstance
		0,           // lpParam
	)
	return hwnd
}

func instanceHandle() uintptr {
	r, _, _ := procGetModuleHandleW.Call(0)
	return r
}

// shellMenuScreenPosition returns the screen coordinates where the shell
// context menu should appear. It prefers the current cursor position (the
// menu must appear at the user's pointer), falling back to the window origin
// plus the supplied client offsets when GetCursorPos fails.
func shellMenuScreenPosition(hwnd uintptr, clientX, clientY int) (int32, int32) {
	if pt, ok := cursorPoint(); ok {
		return pt.X, pt.Y
	}
	if hwnd != 0 {
		var rect rect
		if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); r != 0 {
			return rect.Left + int32(clientX), rect.Top + int32(clientY)
		}
	}
	return int32(clientX), int32(clientY)
}

// contextMenuService owns the STA apartment, the message-only owner window,
// and the built-menu cache that make the shell context menu appear instantly
// on right-click. QueryContextMenu is slow (~300ms) because it invokes every
// registered shell-extension DLL, so we prebuild the menu while the pointer
// hovers over a row and cache the built HMENU/IContextMenu. A right-click that
// hits the cache shows immediately; a miss builds on demand. Because the
// shell COM objects are apartment-bound, all building, showing and cache
// access happen on this one dedicated thread.
type contextMenuService struct {
	req   chan ctxMenuRequest
	cache map[string]*builtShellMenu
}

type ctxMenuRequest struct {
	cmd    ctxMenuCmd
	paths  []string
	screenX int32
	screenY int32
	owner  uintptr
	result chan ctxMenuResult
}

type ctxMenuCmd int

const (
	ctxMenuCmdShow ctxMenuCmd = iota
	ctxMenuCmdPrebuild
	ctxMenuCmdWarmup
)

type ctxMenuResult struct {
	shown bool
	err   error
}

var ctxMenuSvc *contextMenuService

// startContextMenuService launches the persistent context-menu thread. It is
// called once at startup; a fresh OS thread has no COM apartment, so the STA
// initialization always succeeds here and stays valid for the process life.
func startContextMenuService() {
	if ctxMenuSvc != nil {
		return
	}
	svc := &contextMenuService{
		req:   make(chan ctxMenuRequest, 16),
		cache: make(map[string]*builtShellMenu),
	}
	ctxMenuSvc = svc
	go svc.run()
}

func (s *contextMenuService) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, coinitApartmentThreaded|coinitDisableOLE1DDE); err != nil {
		if err.(syscall.Errno) != rpcEChangedMode {
			return
		}
	}
	defer windows.CoUninitialize()

	// One message-only owner window for the lifetime of the service, so every
	// menu shares a stable owner on this thread's message queue.
	owner := createMenuOwnerWindow()
	if owner == 0 {
		owner = foregroundWindowHandle()
	}
	uiDebugLogf("contextmenu service thread owner=%x", owner)

	for req := range s.req {
		switch req.cmd {
		case ctxMenuCmdPrebuild:
			s.prebuild(req.paths)
		case ctxMenuCmdWarmup:
			s.warmup(req.paths)
		case ctxMenuCmdShow:
			shown, err := s.show(owner, req)
			if req.result != nil {
				req.result <- ctxMenuResult{shown: shown, err: err}
			}
		}
	}
}

func pathsCacheKey(paths []string) string {
	return strings.Join(paths, "\x00")
}

// prebuild builds the context menu for the given paths and stores it in the
// cache so a later right-click on the same selection is instant. The previous
// entry for the same key is destroyed to bound cache memory.
func (s *contextMenuService) prebuild(paths []string) {
	key := pathsCacheKey(paths)
	if old, ok := s.cache[key]; ok {
		s.destroyBuilt(old)
		delete(s.cache, key)
	}
	built, err := buildShellContextMenu(paths)
	if err != nil {
		uiDebugLogf("contextmenu prebuild err=%v", err)
		return
	}
	if built == nil {
		return
	}
	s.cache[key] = built
	uiDebugLogf("contextmenu prebuild cached key=%d", len(key))
}

// warmup builds and immediately discards menus for a few representative file
// types so the shell-extension DLLs are loaded before the first right-click.
func (s *contextMenuService) warmup(paths []string) {
	for _, path := range paths {
		built, err := buildShellContextMenu([]string{path})
		if err != nil {
			uiDebugLogf("contextmenu warmup %q err=%v", path, err)
			continue
		}
		if built == nil {
			continue
		}
		s.destroyBuilt(built)
	}
	uiDebugLogf("contextmenu warmup done paths=%d", len(paths))
}

func (s *contextMenuService) destroyBuilt(built *builtShellMenu) {
	if built == nil {
		return
	}
	if built.hMenu != 0 {
		procDestroyMenu.Call(built.hMenu)
	}
	if built.ctxMenu != nil {
		releaseCom(built.ctxMenu)
	}
}

// show displays the context menu for the request's paths. A cache hit shows
// the prebuilt menu immediately; a miss builds it first (cold, ~300ms).
func (s *contextMenuService) show(owner uintptr, req ctxMenuRequest) (bool, error) {
	key := pathsCacheKey(req.paths)
	built, ok := s.cache[key]
	if ok {
		delete(s.cache, key)
	}
	if !ok {
		var err error
		built, err = buildShellContextMenu(req.paths)
		if err != nil {
			return false, err
		}
		if built == nil {
			return false, nil // no verbs: frontend falls back to HTML menu
		}
	}
	defer s.destroyBuilt(built)

	if owner == 0 {
		owner = req.owner
	}
	menuStart := time.Now()
	uiDebugLogf("contextmenu show cached=%v count=%d t=%dms", ok, built.count, time.Since(menuStart).Milliseconds())

	// Give the owner window foreground focus before the modal loop.
	// Without this, TrackPopupMenuEx can dismiss the menu immediately.
	if owner != 0 {
		procSetForegroundWindow.Call(owner)
		// A menu can be dismissed by a stray WM_LBUTTONDOWN/WM_RBUTTONDOWN
		// that was already queued; the WM_NULL forces a wait for the message
		// queue to catch up. This is the standard workaround used by the
		// SDK's TrackPopupMenu demos.
		procPostMessage.Call(owner, wmNull, 0, 0)
	}

	// TrackPopupMenuEx(hMenu, TPM_RETURNCMD|TPM_RIGHTBUTTON|TPM_LEFTBUTTON,
	// screenX, screenY, owner, nil). Returns the selected command offset, or 0
	// when dismissed.
	cmd, _, _ := procTrackPopupMenuEx.Call(
		built.hMenu,
		uintptr(tpmReturnCommand|tpmRightButton|tpmLeftButton),
		uintptr(uint32(req.screenX)),
		uintptr(uint32(req.screenY)),
		owner,
		0,
	)
	uiDebugLogf("contextmenu TrackPopupMenuEx cmd=%d owner=%x t=%dms", cmd, owner, time.Since(menuStart).Milliseconds())
	if cmd == 0 {
		return true, nil // dismissed without choosing an item
	}
	verb := uint16(cmd - contextMenuIDFirst)

	var info cminvokeCommandInfoEx
	info.cbSize = uint32(unsafe.Sizeof(info))
	info.fMask = cmicMaskUnicode | cmicMaskPTInvoke
	info.hwnd = owner
	info.lpVerb = makeIntResourceW(verb)
	info.lpVerbW = makeIntResourceW(verb)
	info.ptInvoke.X = int32(req.screenX)
	info.ptInvoke.Y = int32(req.screenY)
	info.nShow = swShowNormal

	// InvokeCommand(&info).
	r := callVtbl(built.ctxMenu, iContextMenuInvokeCommandSlot,
		uintptr(unsafe.Pointer(&info)),
	)
	if r != 0 {
		return true, fmt.Errorf("InvokeCommand: %w", comHResultErr(r))
	}
	return true, nil
}

// showShellContextMenu shows the native Explorer shell context menu for the
// given paths at the given screen coordinates. Returns shown=true once the
// menu has been displayed (even if the user dismissed it without choosing an
// item). Returns shown=false with an error when the menu could not be shown.
//
// The menu is built and shown on the persistent context-menu service thread,
// which holds the STA apartment and the owner window. A prebuilt cache entry
// for the same selection makes this path instant.
func showShellContextMenu(ownerHwnd uintptr, paths []string, screenX, screenY int32) (bool, error) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			clean = append(clean, trimmed)
		}
	}
	if len(clean) == 0 {
		return false, nil
	}
	if ownerHwnd == 0 {
		ownerHwnd = foregroundWindowHandle()
	}
	startContextMenuService()
	req := ctxMenuRequest{
		cmd:     ctxMenuCmdShow,
		paths:   clean,
		screenX: screenX,
		screenY: screenY,
		owner:   ownerHwnd,
		result:  make(chan ctxMenuResult, 1),
	}
	ctxMenuSvc.req <- req
	res := <-req.result
	return res.shown, res.err
}

// prebuildShellContextMenu builds the context menu for the given paths in the
// background (on the persistent service thread) and caches it, so a subsequent
// right-click on the same selection shows instantly. Non-blocking: if the
// service is busy (e.g. a menu is already open) the prebuild is skipped.
func prebuildShellContextMenu(paths []string) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			clean = append(clean, trimmed)
		}
	}
	if len(clean) == 0 {
		return
	}
	startContextMenuService()
	select {
	case ctxMenuSvc.req <- ctxMenuRequest{cmd: ctxMenuCmdPrebuild, paths: clean}:
	default:
	}
}

// warmUpShellContextMenus pre-loads the shell context-menu handlers for a few
// representative file types so the first user right-click is warm. The built
// menus are discarded immediately. Runs on the service thread at startup.
func warmUpShellContextMenus() {
	startContextMenuService()
	go func() {
		var representative []string
		if exe, err := os.Executable(); err == nil && fileExists(exe) {
			representative = append(representative, exe)
		}
		dir := os.TempDir()
		for _, ext := range []string{".txt", ".png", ".zip"} {
			f, err := os.CreateTemp(dir, "seekfs-warm-*"+ext)
			if err != nil {
				continue
			}
			path := f.Name()
			_ = f.Close()
			representative = append(representative, path)
			defer os.Remove(path)
		}
		ctxMenuSvc.req <- ctxMenuRequest{cmd: ctxMenuCmdWarmup, paths: representative}
	}()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// builtShellMenu holds an IContextMenu plus its populated HMENU, produced by
// buildShellContextMenu. Callers either show it via TrackPopupMenuEx (the
// normal path) or destroy it immediately (the warm-up path).
type builtShellMenu struct {
	ctxMenu unsafe.Pointer
	hMenu   uintptr
	count   uint32
}

// buildShellContextMenu parses the given paths and builds the Explorer
// context menu for them: PIDLs, parent grouping, SHCreateDefaultContextMenu,
// CreatePopupMenu and QueryContextMenu. The expensive part is
// QueryContextMenu, which loads every registered shell-extension DLL, so it is
// shared by the interactive path and the startup warm-up.
//
// If all selected items live in the same parent folder we use
// SHCreateDefaultContextMenu — the same API Explorer uses — which gives every
// modern verb and shell extension: Open with, Send to, Copy as path, Rename,
// Share, Compress, Edit with, third-party handlers, etc. SHCreateDefault
// ContextMenu requires child-relative PIDLs from a single parent folder, so a
// cross-folder selection (common in search results) falls back to the desktop-
// folder merge, which yields the compact common verb set Explorer itself shows
// for multi-folder selections. Items are grouped by their parent folder
// (compared by the parent PIDL bytes, not COM pointer identity) so that
// same-folder selections are detected reliably even when the shell returns
// fresh IShellFolder instances for the same directory.
func buildShellContextMenu(clean []string) (*builtShellMenu, error) {
	// Parse each path into an ITEMIDLIST.
	pidls := make([]unsafe.Pointer, 0, len(clean))
	defer func() {
		for _, pidl := range pidls {
			windows.CoTaskMemFree(pidl)
		}
	}()
	for _, path := range clean {
		ptr, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		var pidl unsafe.Pointer
		r, _, _ := procSHParseDisplayName.Call(
			uintptr(unsafe.Pointer(ptr)),   // pszName
			0,                              // pbc
			uintptr(unsafe.Pointer(&pidl)), // ppidl
			0,                              // sfgaoIn
			0,                              // psfgaoOut
		)
		if r != 0 {
			return nil, fmt.Errorf("SHParseDisplayName(%q): %w", path, comHResultErr(r))
		}
		if pidl == nil {
			return nil, fmt.Errorf("SHParseDisplayName(%q) returned a nil pidl", path)
		}
		pidls = append(pidls, pidl)
	}

	type parentGroup struct {
		parent   unsafe.Pointer // IShellFolder (kept alive until done)
		children []unsafe.Pointer
	}
	groupsByKey := make(map[string]*parentGroup)
	groups := make([]*parentGroup, 0, len(pidls))
	keptParents := make([]unsafe.Pointer, 0, len(pidls))
	for _, pidl := range pidls {
		// SHBindToParent(pidl, &IID_IShellFolder, &parent, &child)
		var parent unsafe.Pointer
		var child unsafe.Pointer
		r, _, _ := procSHBindToParent.Call(
			uintptr(pidl),
			uintptr(unsafe.Pointer(&iidShellFolder)),
			uintptr(unsafe.Pointer(&parent)),
			uintptr(unsafe.Pointer(&child)),
		)
		if r != 0 {
			return nil, fmt.Errorf("SHBindToParent(%q): %w", "", comHResultErr(r))
		}
		key := parentKeyOfPidl(pidl)
		g := groupsByKey[key]
		if g == nil {
			g = &parentGroup{parent: parent}
			groupsByKey[key] = g
			groups = append(groups, g)
			keptParents = append(keptParents, parent)
		} else {
			// Same parent folder, but a fresh IShellFolder instance: release
			// the duplicate, keep the first one for the whole selection.
			releaseCom(parent)
		}
		g.children = append(g.children, child)
	}
	defer func() {
		for _, p := range keptParents {
			releaseCom(p)
		}
	}()

	var ctxMenu unsafe.Pointer
	type defContextMenu struct {
		hwnd       uintptr
		pcmcb      uintptr
		pidlFolder uintptr
		psf        unsafe.Pointer
		cidl       uint32
		apidl      *unsafe.Pointer
		psfFilter  unsafe.Pointer
		pidlItem   unsafe.Pointer
	}
	var dcm defContextMenu
	if len(groups) == 1 && len(groups[0].children) > 0 {
		// Single parent folder: build the full default context menu.
		// DEFCONTEXTMENU struct — field order must match the SDK layout:
		//   hwnd, pcmcb, pidlFolder, psf, cidl, apidl, psfFilter, pidlItem
		g := groups[0]
		dcm = defContextMenu{
			psf:   g.parent,
			cidl:  uint32(len(g.children)),
			apidl: &g.children[0],
		}
	} else if len(pidls) > 0 {
		// Cross-folder selection: SHCreateDefaultContextMenu accepts the
		// desktop as the parent folder with the absolute PIDLs as children,
		// producing the same compact common-verb menu Explorer shows for
		// multi-folder selections (Open/Cut/Copy/Delete/Properties/...).
		var desktop unsafe.Pointer
		r, _, _ := procSHGetDesktopFolder.Call(uintptr(unsafe.Pointer(&desktop)))
		if r != 0 {
			return nil, fmt.Errorf("SHGetDesktopFolder: %w", comHResultErr(r))
		}
		defer releaseCom(desktop)
		dcm = defContextMenu{
			psf:   desktop,
			cidl:  uint32(len(pidls)),
			apidl: &pidls[0],
		}
	}
	if dcm.psf == nil || dcm.cidl == 0 {
		return nil, fmt.Errorf("no items to build a context menu for")
	}
	r, _, _ := procSHCreateDefaultContextMenu.Call(
		uintptr(unsafe.Pointer(&dcm)),
		uintptr(unsafe.Pointer(&iidContextMenu)),
		uintptr(unsafe.Pointer(&ctxMenu)),
	)
	if r != 0 {
		return nil, fmt.Errorf("SHCreateDefaultContextMenu: %w", comHResultErr(r))
	}
	if ctxMenu == nil {
		return nil, fmt.Errorf("SHCreateDefaultContextMenu returned a nil IContextMenu")
	}

	// Create the popup menu.
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		releaseCom(ctxMenu)
		return nil, fmt.Errorf("CreatePopupMenu failed")
	}

	// QueryContextMenu(hMenu, 0, 1, 0x7FFF, CMF_NORMAL|CMF_EXPLORE).
	r = callVtbl(ctxMenu, iContextMenuQueryContextMenuSlot,
		hMenu,
		0,
		contextMenuIDFirst,
		contextMenuIDLast,
		uintptr(cmfNormal|cmfExplore),
	)
	if r == 0xFFFFFFFF {
		procDestroyMenu.Call(hMenu)
		releaseCom(ctxMenu)
		return nil, fmt.Errorf("QueryContextMenu failed")
	}
	if r == 0 {
		// No verbs were added (e.g. the selection has no shell handlers).
		procDestroyMenu.Call(hMenu)
		releaseCom(ctxMenu)
		return nil, nil
	}
	return &builtShellMenu{ctxMenu: ctxMenu, hMenu: hMenu, count: uint32(r & 0xFFFF)}, nil
}
