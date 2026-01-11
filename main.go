package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"os"
	"path"
	"runtime/trace"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	traceFile, err := os.Create("trace.out")
	if err != nil {
		panic(err)
	}
	defer traceFile.Close()

	trace.Start(traceFile)
	defer trace.Stop()

	ctx := context.Background()
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
			err = handleWLDisplayEvent(opcode, body)
			if err != nil {
				slog.ErrorContext(ctx, "wl_display handler err", "err", err)
				os.Exit(1)
			}
		case WLRegistryID:
			if opcode != 0 {
				// unhandled
				continue
			}
			name := binary.LittleEndian.Uint32(body)
			iface, off := parseStr(body[4:])
			ver := binary.LittleEndian.Uint32(body[4+off:])
			switch string(iface) {
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
		case callbackID: // wl_callback
			if opcode != 0 {
				slog.ErrorContext(ctx, "wl_callback unknown opcode", "opcode", opcode)
				continue
			}
			// 		slog.InfoContext(ctx, "wl_callback::done")
			break loop
		default:
			slog.InfoContext(ctx, "wl msg", "id", id, "opcode", opcode, "body", hex.EncodeToString(body))
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
				slog.ErrorContext(ctx, "wl_callback unknown opcode", "opcode", opcode)
				continue
			}
			// 		slog.InfoContext(ctx, "wl_callback::done")
			break loop2
		default:
			slog.InfoContext(ctx, "wl msg", "id", id, "opcode", opcode, "body", hex.EncodeToString(body))
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
			serial := binary.LittleEndian.Uint32(body)
			mustPong(conn, serial)
		case XDGSurfaceID:
			if opcode != 0 {
				continue
			}
			serial := binary.LittleEndian.Uint32(body)
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
			// 		slog.InfoContext(ctx, "xdg_top_level::close get")
			break loop3
		default:
			slog.InfoContext(ctx, "wl msg", "id", id, "opcode", opcode, "body", hex.EncodeToString(body))
		}
	}
}

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

const WORD_SIZE = 4
const HEADER_SIZE = 2 * WORD_SIZE

var headerBytes = make([]byte, HEADER_SIZE)

func read(conn net.Conn) (id, opcode uint32, body []byte, err error) {
	_, err = conn.Read(headerBytes)
	if err != nil {
		return
	}
	id = binary.LittleEndian.Uint32(headerBytes[0:])
	sizeNOpcode := binary.LittleEndian.Uint32(headerBytes[4:])
	size := sizeNOpcode >> 16
	opcode = sizeNOpcode & 0xffff
	body = make([]byte, size-HEADER_SIZE)
	_, err = conn.Read(body)
	return
}

type wlDisplayErr struct {
	id   uint32
	code uint32
	msg  []byte
}

func (err wlDisplayErr) Error() string {
	return "wl_display::error object " + strconv.FormatUint(uint64(err.id), 10) + " code " + strconv.FormatUint(uint64(err.code), 10) + ": " + string(err.msg)
}

func handleWLDisplayEvent(opcode uint32, body []byte) error {
	object := binary.LittleEndian.Uint32(body)
	switch opcode {
	case 0: // error
		code := binary.LittleEndian.Uint32(body[4:])
		msg, _ := parseStr(body[8:])
		return wlDisplayErr{id: object, code: code, msg: msg}
	case 1: // delete_id
		objects[object] = objNone
	}
	return nil
}

func mustGetReg(conn *net.UnixConn) {
	msgBytes := makeMsgBuf(WLDisplayID, 1, WORD_SIZE)
	msgBytes = binary.LittleEndian.AppendUint32(msgBytes, WLRegistryID)
	_, err := conn.Write(msgBytes)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_display::get_registry done", "msg", hex.EncodeToString(msgBytes))
}

func mustSync(conn *net.UnixConn) (id uint32) {
	id = regObj(objWLCallback)
	msgBytes := makeMsgBuf(WLDisplayID, 0, WORD_SIZE)
	msgBytes = binary.LittleEndian.AppendUint32(msgBytes, id)
	_, err := conn.Write(msgBytes)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_display::sync done", "id", id, "msg", hex.EncodeToString(msgBytes))
	return id
}

func mustBind(conn *net.UnixConn, name, id, ver uint32, iface []byte) {
	strLen := uint32(len(iface) + 1)
	padding := (4 - strLen%4) % 4
	msgBytes := makeMsgBuf(WLRegistryID, 0, WORD_SIZE*4+strLen+padding)
	msgBytes = binary.LittleEndian.AppendUint32(msgBytes, name)
	msgBytes = binary.LittleEndian.AppendUint32(msgBytes, uint32(len(iface)+1))
	msgBytes = append(msgBytes, iface...)
	msgBytes = append(msgBytes, 0)
	for range padding {
		msgBytes = append(msgBytes, 0)
	}
	msgBytes = binary.LittleEndian.AppendUint32(msgBytes, ver)
	msgBytes = binary.LittleEndian.AppendUint32(msgBytes, id)
	_, err := conn.Write(msgBytes)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_registry::bind done", "name", name, "id", id, "iface", iface, "ver", ver, "msg", hex.EncodeToString(msgBytes))
}

func mustCreatePool(conn *net.UnixConn) {
	buf := makeMsgBuf(WLShmID, 0, WORD_SIZE*2)
	WLShmPoolID = regObj(objWLShmPool)
	buf = binary.LittleEndian.AppendUint32(buf, WLShmPoolID)
	const size = 100 * 100 * 4
	buf = binary.LittleEndian.AppendUint32(buf, size)
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
	// slog.InfoContext(ctx, "wl_shm::create_pool done", "filename", WLShmPoolFile.Name(), "WLShmPoolID", WLShmPoolID, "msg", hex.EncodeToString(buf))
}

func mustCreateBuffer(conn *net.UnixConn) {
	buf := makeMsgBuf(WLShmPoolID, 0, WORD_SIZE*6)
	WLBufferID = regObj(objWLBuffer)
	buf = binary.LittleEndian.AppendUint32(buf, WLBufferID)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, 100)
	buf = binary.LittleEndian.AppendUint32(buf, 100)
	buf = binary.LittleEndian.AppendUint32(buf, 100*4)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_shm_pool::create_buffer done", "WLBufferID", WLBufferID, "msg", hex.EncodeToString(buf))
}
func mustCreateSurface(conn *net.UnixConn) {
	buf := makeMsgBuf(WLCompositorID, 0, WORD_SIZE)
	WLSurfaceID = regObj(objWLSurface)
	buf = binary.LittleEndian.AppendUint32(buf, WLSurfaceID)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_compositor::create_surface done", "WLSurfaceID", WLSurfaceID, "msg", hex.EncodeToString(buf))
}
func mustAttach(conn *net.UnixConn) {
	buf := makeMsgBuf(WLSurfaceID, 1, WORD_SIZE*3)
	buf = binary.LittleEndian.AppendUint32(buf, WLBufferID)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_surface::attach done", "msg", hex.EncodeToString(buf))
}
func mustDamage(conn *net.UnixConn) {
	buf := makeMsgBuf(WLSurfaceID, 9, WORD_SIZE*4)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, 100)
	buf = binary.LittleEndian.AppendUint32(buf, 100)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_surface::damage done", "msg", hex.EncodeToString(buf))
}
func mustCommit(conn *net.UnixConn) {
	buf := makeMsgBuf(WLSurfaceID, 6, 0)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "wl_surface::commit done", "msg", hex.EncodeToString(buf))
}
func mustGetXDGSurface(conn *net.UnixConn) {
	buf := makeMsgBuf(XDGWMBaseID, 2, WORD_SIZE*2)
	XDGSurfaceID = regObj(objXDGSurface)
	buf = binary.LittleEndian.AppendUint32(buf, XDGSurfaceID)
	buf = binary.LittleEndian.AppendUint32(buf, WLSurfaceID)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "xdg_wm_base::get_xdg_surface done", "XDGSurfaceID", XDGSurfaceID, "msg", hex.EncodeToString(buf))
}
func mustGetTopLevel(conn *net.UnixConn) {
	buf := makeMsgBuf(XDGSurfaceID, 1, WORD_SIZE)
	XDGTopLevelID = regObj(objXDGTopLevel)
	buf = binary.LittleEndian.AppendUint32(buf, XDGTopLevelID)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "xdg_surface::get_top_level done", "XDGTopLevelID", XDGTopLevelID, "msg", hex.EncodeToString(buf))
}
func mustAckConfigure(conn *net.UnixConn, serial uint32) {
	buf := makeMsgBuf(XDGSurfaceID, 4, WORD_SIZE)
	buf = binary.LittleEndian.AppendUint32(buf, serial)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "xdg_surface::ack_configure done", "serial", serial, "msg", hex.EncodeToString(buf))
}
func mustPong(conn *net.UnixConn, serial uint32) {
	buf := makeMsgBuf(XDGWMBaseID, 3, WORD_SIZE)
	buf = binary.LittleEndian.AppendUint32(buf, serial)
	_, err := conn.Write(buf)
	if err != nil {
		panic(err)
	}
	// slog.InfoContext(ctx, "xdg_wm_base::pong done", "serial", serial, "msg", hex.EncodeToString(buf))
}

func makeMsgBuf(id uint32, opcode uint16, dataLen uint32) []byte {
	msgLen := HEADER_SIZE + dataLen
	buf := make([]byte, HEADER_SIZE, msgLen)
	binary.LittleEndian.PutUint32(buf, id)
	binary.LittleEndian.PutUint32(buf[4:8], msgLen<<16+uint32(opcode))
	return buf
}

func parseStr(b []byte) ([]byte, uint32) {
	n := binary.LittleEndian.Uint32(b)
	end := 4 + n
	pad := (4 - n%4) % 4
	return b[4 : end-1], end + pad
}
