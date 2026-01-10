package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"os"
	"path"
	"time"

	"golang.org/x/sys/unix"
)

const WORD_SIZE = 4
const HEADER_SIZE = 2 * WORD_SIZE

type objType uint8

const (
	objNone = iota
	objWLDisplay
	objWLRegistry
	objWLCallback
	objWLCompositor
	objWLShm
	objWLShmPool
	objWLSurface
	objWLBuffer
	objXDGWMBase
	objXDGSurface
	objXDGTopLevel
)

var objects = [1 << 8]objType{objNone, objWLDisplay, objWLRegistry}

const WLDisplayID = 1
const WLRegistryID = 2

var (
	// IDs
	WLCompositorID uint32
	WLShmID        uint32
	WLShmPoolID    uint32
	WLBufferID     uint32
	WLSurfaceID    uint32
	XDGWMBaseID    uint32
	XDGSurfaceID   uint32
	XDGTopLevelID  uint32

	// WLShmPool stuff
	WLShmPoolFile *os.File
	WLShmPoolBuf  []byte
)

func regObj(t objType) (id uint32) {
	id = 1
	n := uint32(len(objects))
	for id < n {
		if objects[id] == 0 {
			objects[id] = t
			break
		}
		id++
	}
	return id
}

func main() {
	socketPath := os.Getenv("WAYLAND_SOCKET")
	xdgRuntimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if socketPath == "" && xdgRuntimeDir == "" {
		panic(errors.New("wayland env vars not set, neither WAYLAND_SOCKET nor XDG_RUNTIME_DIR is set"))
	}
	if socketPath == "" {
		socketPath = path.Join(xdgRuntimeDir, os.Getenv("WAYLAND_DISPLAY"))
	}
	if socketPath == "" {
		socketPath = path.Join(xdgRuntimeDir, "wayland-0")
	}

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	err = conn.SetReadDeadline(time.Time{})
	if err != nil {
		panic(err)
	}

	mustGetReg(conn)
	callbackID := mustSync(conn)

loop:
	for {
		id, opcode, body, err := read(conn)
		if err != nil {
			panic(err)
		}
		switch id {
		case WLDisplayID:
			handleWLDisplayEvent(opcode, body)
		case WLRegistryID:
			switch opcode {
			case 0: // global
				name := binary.NativeEndian.Uint32(body)
				iface, off := parseStr(body[4:])
				ver := binary.NativeEndian.Uint32(body[4+off:])
				switch iface {
				case "wl_compositor":
					WLCompositorID = regObj(objWLCompositor)
					mustBind(conn, name, WLCompositorID, ver, iface)
				case "wl_shm":
					WLShmID = regObj(objWLShm)
					mustBind(conn, name, WLShmID, ver, iface)
				case "xdg_wm_base":
					XDGWMBaseID = regObj(objXDGWMBase)
					mustBind(conn, name, XDGWMBaseID, ver, iface)
				}
			}
		case callbackID: // wl_callback
			if opcode != 0 {
				slog.Error("wl_callback unknown opcode", "opcode", opcode)
				continue
			}
			slog.Info("wl_callback::done")
			break loop
		default:
			slog.Info("wl msg", "id", id, "opcode", opcode, "body", hex.EncodeToString(body))
		}
	}

	mustCreateSurface(conn)
	mustGetXDGSurface(conn)
	mustGetTopLevel(conn)
	mustCreatePool(conn)
	mustCreateBuffer(conn)
	callbackID = mustSync(conn)
loop2:
	for {
		id, opcode, body, err := read(conn)
		if err != nil {
			panic(err)
		}
		switch id {
		case WLDisplayID:
			handleWLDisplayEvent(opcode, body)
		case callbackID: // wl_callback
			if opcode != 0 {
				slog.Error("wl_callback unknown opcode", "opcode", opcode)
				continue
			}
			slog.Info("wl_callback::done")
			break loop2
		default:
			slog.Info("wl msg", "id", id, "opcode", opcode, "body", hex.EncodeToString(body))
		}
	}

	mustAttach(conn)
	mustDamage(conn)
	mustCommit(conn)
loop3:
	for {
		id, opcode, body, err := read(conn)
		if err != nil {
			panic(err)
		}
		switch id {
		case WLDisplayID:
			handleWLDisplayEvent(opcode, body)
		case XDGWMBaseID:
			if opcode != 0 {
				continue
			}
			serial := binary.NativeEndian.Uint32(body)
			mustPong(conn, serial)
		case XDGSurfaceID:
			if opcode != 0 {
				continue
			}
			serial := binary.NativeEndian.Uint32(body)
			mustAckConfigure(conn, serial)
			for i := range WLShmPoolBuf {
				WLShmPoolBuf[i] = ^WLShmPoolBuf[i]
			}
			mustAttach(conn)
			mustDamage(conn)
			mustCommit(conn)
		case XDGTopLevelID:
			if opcode != 1 {
				continue
			}
			slog.Info("xdg_top_level::close get")
			break loop3
		default:
			slog.Info("wl msg", "id", id, "opcode", opcode, "body", hex.EncodeToString(body))
		}
	}
}

func read(conn net.Conn) (id, opcode uint32, body []byte, err error) {
	headerBytes := make([]byte, HEADER_SIZE)
	_, err = conn.Read(headerBytes)
	if err != nil {
		return
	}
	id = binary.NativeEndian.Uint32(headerBytes[0:])
	sizeNOpcode := binary.NativeEndian.Uint32(headerBytes[4:])
	size := sizeNOpcode >> 16
	opcode = sizeNOpcode & 0xffff
	body = make([]byte, size-HEADER_SIZE)
	_, err = conn.Read(body)
	return
}

func handleWLDisplayEvent(opcode uint32, body []byte) {
	switch opcode {
	case 0: // error
		object := binary.NativeEndian.Uint32(body)
		code := binary.NativeEndian.Uint32(body[4:])
		msg, _ := parseStr(body[8:])
		slog.Error("wl_display::error", "object", object, "code", code, "msg", msg)
	case 1: // delete_id
		object := binary.NativeEndian.Uint32(body)
		objects[object] = objNone
	}
}

func mustGetReg(conn net.Conn) {
	msgBytes := makeMsgBuf(WLDisplayID, 1, WORD_SIZE)
	msgBytes = binary.NativeEndian.AppendUint32(msgBytes, WLRegistryID)
	_, err := conn.Write(msgBytes)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_display::get_registry done", "msg", hex.EncodeToString(msgBytes))
}

func mustSync(conn net.Conn) (id uint32) {
	id = regObj(objWLCallback)
	msgBytes := makeMsgBuf(WLDisplayID, 0, WORD_SIZE)
	msgBytes = binary.NativeEndian.AppendUint32(msgBytes, id)
	_, err := conn.Write(msgBytes)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_display::sync done", "id", id, "msg", hex.EncodeToString(msgBytes))
	return id
}

func mustBind(conn net.Conn, name, id, ver uint32, iface string) {
	strLen := uint32(len(iface) + 1)
	padding := (4 - strLen%4) % 4
	msgBytes := makeMsgBuf(WLRegistryID, 0, WORD_SIZE*4+strLen+padding)
	msgBytes = binary.NativeEndian.AppendUint32(msgBytes, name)
	msgBytes = binary.NativeEndian.AppendUint32(msgBytes, uint32(len(iface)+1))
	msgBytes = append(msgBytes, []byte(iface)...)
	msgBytes = append(msgBytes, 0)
	for range padding {
		msgBytes = append(msgBytes, 0)
	}
	msgBytes = binary.NativeEndian.AppendUint32(msgBytes, ver)
	msgBytes = binary.NativeEndian.AppendUint32(msgBytes, id)
	_, err := conn.Write(msgBytes)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_registry::bind done", "name", name, "id", id, "iface", iface, "ver", ver, "msg", hex.EncodeToString(msgBytes))
}

func mustCreatePool(conn *net.UnixConn) {
	buf := makeMsgBuf(WLShmID, 0, WORD_SIZE*2)
	WLShmPoolID = regObj(objWLShmPool)
	buf = binary.NativeEndian.AppendUint32(buf, WLShmPoolID)
	const size = 100 * 100 * 4
	buf = binary.NativeEndian.AppendUint32(buf, size)
	var err error
	WLShmPoolFile, err = os.CreateTemp("", "wl_shm_pool")
	if err != nil {
		panic(err)
	}
	err = WLShmPoolFile.Truncate(size)
	if err != nil {
		panic(err)
	}

	fd := int(WLShmPoolFile.Fd())
	WLShmPoolBuf, err = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		panic(err)
	}
	_, _, err = conn.WriteMsgUnix(buf, unix.UnixRights(fd), nil)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_shm::create_pool done", "filename", WLShmPoolFile.Name(), "WLShmPoolID", WLShmPoolID, "msg", hex.EncodeToString(buf))
}

func mustCreateBuffer(conn *net.UnixConn) {
	buf := makeMsgBuf(WLShmPoolID, 0, WORD_SIZE*6)
	WLBufferID = regObj(objWLBuffer)
	buf = binary.NativeEndian.AppendUint32(buf, WLBufferID)
	buf = binary.NativeEndian.AppendUint32(buf, 0)
	buf = binary.NativeEndian.AppendUint32(buf, 100)
	buf = binary.NativeEndian.AppendUint32(buf, 100)
	buf = binary.NativeEndian.AppendUint32(buf, 100*4)
	buf = binary.NativeEndian.AppendUint32(buf, 1)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_shm_pool::create_buffer done", "WLBufferID", WLBufferID, "msg", hex.EncodeToString(buf))
}
func mustCreateSurface(conn *net.UnixConn) {
	buf := makeMsgBuf(WLCompositorID, 0, WORD_SIZE)
	WLSurfaceID = regObj(objWLSurface)
	buf = binary.NativeEndian.AppendUint32(buf, WLSurfaceID)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_compositor::create_surface done", "WLSurfaceID", WLSurfaceID, "msg", hex.EncodeToString(buf))
}
func mustAttach(conn *net.UnixConn) {
	buf := makeMsgBuf(WLSurfaceID, 1, WORD_SIZE*3)
	buf = binary.NativeEndian.AppendUint32(buf, WLBufferID)
	buf = binary.NativeEndian.AppendUint32(buf, 0)
	buf = binary.NativeEndian.AppendUint32(buf, 0)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_surface::attach done", "msg", hex.EncodeToString(buf))
}
func mustDamage(conn *net.UnixConn) {
	buf := makeMsgBuf(WLSurfaceID, 9, WORD_SIZE*4)
	buf = binary.NativeEndian.AppendUint32(buf, 0)
	buf = binary.NativeEndian.AppendUint32(buf, 0)
	buf = binary.NativeEndian.AppendUint32(buf, 100)
	buf = binary.NativeEndian.AppendUint32(buf, 100)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_surface::damage done", "msg", hex.EncodeToString(buf))
}
func mustCommit(conn *net.UnixConn) {
	buf := makeMsgBuf(WLSurfaceID, 6, 0)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("wl_surface::commit done", "msg", hex.EncodeToString(buf))
}
func mustGetXDGSurface(conn *net.UnixConn) {
	buf := makeMsgBuf(XDGWMBaseID, 2, WORD_SIZE*2)
	XDGSurfaceID = regObj(objXDGSurface)
	buf = binary.NativeEndian.AppendUint32(buf, XDGSurfaceID)
	buf = binary.NativeEndian.AppendUint32(buf, WLSurfaceID)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("xdg_wm_base::get_xdg_surface done", "XDGSurfaceID", XDGSurfaceID, "msg", hex.EncodeToString(buf))
}
func mustGetTopLevel(conn *net.UnixConn) {
	buf := makeMsgBuf(XDGSurfaceID, 1, WORD_SIZE)
	XDGTopLevelID = regObj(objXDGTopLevel)
	buf = binary.NativeEndian.AppendUint32(buf, XDGTopLevelID)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("xdg_surface::get_top_level done", "XDGTopLevelID", XDGTopLevelID, "msg", hex.EncodeToString(buf))
}
func mustAckConfigure(conn *net.UnixConn, serial uint32) {
	buf := makeMsgBuf(XDGSurfaceID, 4, WORD_SIZE)
	buf = binary.NativeEndian.AppendUint32(buf, serial)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("xdg_surface::ack_configure done", "serial", serial, "msg", hex.EncodeToString(buf))
}
func mustPong(conn *net.UnixConn, serial uint32) {
	buf := makeMsgBuf(XDGWMBaseID, 3, WORD_SIZE)
	buf = binary.NativeEndian.AppendUint32(buf, serial)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	slog.Info("xdg_wm_base::pong done", "serial", serial, "msg", hex.EncodeToString(buf))
}

func makeMsgBuf(id uint32, opcode uint16, dataLen uint32) []byte {
	msgLen := HEADER_SIZE + dataLen
	buf := make([]byte, 0, msgLen)
	buf = binary.NativeEndian.AppendUint32(buf, id)
	buf = binary.NativeEndian.AppendUint32(buf, msgLen<<16+uint32(opcode))
	return buf
}

func parseStr(b []byte) (s string, n uint32) {
	n = binary.NativeEndian.Uint32(b)
	end := 4 + n
	s = string(b[4 : end-1])
	endpadded := end + (4-end%4)%4
	return s, endpadded
}
