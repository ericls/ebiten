// Copyright 2022 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !android
// +build !android

package gamepad

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const dirName = "/dev/input"

var reEvent = regexp.MustCompile(`^event[0-9]+$`)

func isBitSet(s []byte, bit int) bool {
	return s[bit/8]&(1<<(bit%8)) != 0
}

type nativeGamepads struct {
	inotify int
	watch   int
}

func (g *nativeGamepads) init(gamepads *gamepads) error {
	// Check the existence of the directory `dirName`.
	var stat unix.Stat_t
	if err := unix.Stat(dirName, &stat); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("gamepad: Stat failed: %w", err)
	}
	if stat.Mode&unix.S_IFDIR == 0 {
		return nil
	}

	inotify, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("gamepad: InotifyInit1 failed: %w", err)
	}
	g.inotify = inotify

	if g.inotify > 0 {
		// Register for IN_ATTRIB to get notified when udev is done.
		// This works well in practice but the true way is libudev.
		watch, err := unix.InotifyAddWatch(g.inotify, dirName, unix.IN_CREATE|unix.IN_ATTRIB|unix.IN_DELETE)
		if err != nil {
			return fmt.Errorf("gamepad: InotifyAddWatch failed: %w", err)
		}
		g.watch = watch
	}

	ents, err := ioutil.ReadDir(dirName)
	if err != nil {
		return fmt.Errorf("gamepad: ReadDir(%s) failed: %w", dirName, err)
	}
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		if !reEvent.MatchString(ent.Name()) {
			continue
		}
		if err := g.openGamepad(gamepads, filepath.Join(dirName, ent.Name())); err != nil {
			return err
		}
	}

	return nil
}

func (*nativeGamepads) openGamepad(gamepads *gamepads, path string) (err error) {
	if gamepads.find(func(gamepad *Gamepad) bool {
		return gamepad.path == path
	}) != nil {
		return nil
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		if err == unix.EACCES {
			return nil
		}
		// This happens just after a disconnection.
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("gamepad: Open failed: %w", err)
	}
	defer func() {
		if err != nil {
			unix.Close(fd)
		}
	}()

	evBits := make([]byte, (unix.EV_CNT+7)/8)
	keyBits := make([]byte, (_KEY_CNT+7)/8)
	absBits := make([]byte, (_ABS_CNT+7)/8)
	var id input_id
	if err := ioctl(fd, _EVIOCGBIT(0, uint(len(evBits))), unsafe.Pointer(&evBits[0])); err != nil {
		return fmt.Errorf("gamepad: ioctl for evBits failed: %w", err)
	}
	if err := ioctl(fd, _EVIOCGBIT(unix.EV_KEY, uint(len(keyBits))), unsafe.Pointer(&keyBits[0])); err != nil {
		return fmt.Errorf("gamepad: ioctl for keyBits failed: %w", err)
	}
	if err := ioctl(fd, _EVIOCGBIT(unix.EV_ABS, uint(len(absBits))), unsafe.Pointer(&absBits[0])); err != nil {
		return fmt.Errorf("gamepad: ioctl for absBits failed: %w", err)
	}
	if err := ioctl(fd, _EVIOCGID(), unsafe.Pointer(&id)); err != nil {
		return fmt.Errorf("gamepad: ioctl for an ID failed: %w", err)
	}

	if !isBitSet(evBits, unix.EV_KEY) {
		unix.Close(fd)
		return nil
	}
	if !isBitSet(evBits, unix.EV_ABS) {
		unix.Close(fd)
		return nil
	}

	cname := make([]byte, 256)
	name := "Unknown"
	// TODO: Is it OK to ignore the error here?
	if err := ioctl(fd, uint(_EVIOCGNAME(uint(len(name)))), unsafe.Pointer(&cname[0])); err == nil {
		name = unix.ByteSliceToString(cname)
	}

	var sdlID string
	if id.vendor != 0 && id.product != 0 && id.version != 0 {
		sdlID = fmt.Sprintf("%02x%02x0000%02x%02x0000%02x%02x0000%02x%02x0000",
			byte(id.bustype), byte(id.bustype>>8),
			byte(id.vendor), byte(id.vendor>>8),
			byte(id.product), byte(id.product>>8),
			byte(id.version), byte(id.version>>8))
	} else {
		bs := []byte(name)
		if len(bs) < 12 {
			bs = append(bs, make([]byte, 12-len(bs))...)
		}
		sdlID = fmt.Sprintf("%02x%02x0000%02x%02x%02x%02x%02x%02x%02x%02x%02x%02x%02x%02x",
			byte(id.bustype), byte(id.bustype>>8),
			bs[0], bs[1], bs[2], bs[3], bs[4], bs[5], bs[6], bs[7], bs[8], bs[9], bs[10], bs[11])
	}

	gp := gamepads.add(name, sdlID)
	gp.path = path
	gp.fd = fd
	runtime.SetFinalizer(gp, func(gp *Gamepad) {
		gp.close()
	})

	var axisCount int
	var buttonCount int
	var hatCount int
	for code := _BTN_MISC; code < _KEY_CNT; code++ {
		if !isBitSet(keyBits, code) {
			continue
		}
		gp.keyMap[code-_BTN_MISC] = buttonCount
		buttonCount++
	}
	for code := 0; code < _ABS_CNT; code++ {
		gp.absMap[code] = -1
		if !isBitSet(absBits, code) {
			continue
		}
		if code >= _ABS_HAT0X && code <= _ABS_HAT3Y {
			gp.absMap[code] = hatCount
			hatCount++
			// Skip Y.
			code++
			continue
		}
		if err := ioctl(gp.fd, uint(_EVIOCGABS(uint(code))), unsafe.Pointer(&gp.absInfo[code])); err != nil {
			return fmt.Errorf("gamepad: ioctl for an abs at openGamepad failed: %w", err)
		}
		gp.absMap[code] = axisCount
		axisCount++
	}

	gp.axisCount_ = axisCount
	gp.buttonCount_ = buttonCount
	gp.hatCount_ = hatCount

	if err := gp.pollAbsState(); err != nil {
		return err
	}

	return nil
}

func (g *nativeGamepads) update(gamepads *gamepads) error {
	if g.inotify <= 0 {
		return nil
	}

	buf := make([]byte, 16384)
	n, err := unix.Read(g.inotify, buf[:])
	if err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		return fmt.Errorf("gamepad: Read failed: %w", err)
	}
	buf = buf[:n]

	for len(buf) > 0 {
		e := inotify_event{
			wd:     int32(buf[0]) | int32(buf[1])<<8 | int32(buf[2])<<16 | int32(buf[3])<<24,
			mask:   uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16 | uint32(buf[7])<<24,
			cookie: uint32(buf[8]) | uint32(buf[9])<<8 | uint32(buf[10])<<16 | uint32(buf[11])<<24,
			len:    uint32(buf[12]) | uint32(buf[13])<<8 | uint32(buf[14])<<16 | uint32(buf[15])<<24,
		}
		e.name = unix.ByteSliceToString(buf[16 : 16+e.len-1]) // len includes the null termiinate.
		buf = buf[16+e.len:]
		if !reEvent.MatchString(e.name) {
			continue
		}

		path := filepath.Join(dirName, e.name)
		if e.mask&(unix.IN_CREATE|unix.IN_ATTRIB) != 0 {
			if err := g.openGamepad(gamepads, path); err != nil {
				return err
			}
			continue
		}
		if e.mask&unix.IN_DELETE != 0 {
			if gp := gamepads.find(func(gamepad *Gamepad) bool {
				return gamepad.path == path
			}); gp != nil {
				gp.close()
				gamepads.remove(func(gamepad *Gamepad) bool {
					return gamepad == gp
				})
			}
			continue
		}
	}

	return nil
}

type nativeGamepad struct {
	fd      int
	path    string
	keyMap  [_KEY_CNT - _BTN_MISC]int
	absMap  [_ABS_CNT]int
	absInfo [_ABS_CNT]input_absinfo
	dropped bool

	axes    [_ABS_CNT]float64
	buttons [_KEY_CNT - _BTN_MISC]bool
	hats    [4]int

	axisCount_   int
	buttonCount_ int
	hatCount_    int
}

func (g *nativeGamepad) close() {
	if g.fd != 0 {
		unix.Close(g.fd)
	}
	g.fd = 0
}

func (g *nativeGamepad) update(gamepad *gamepads) error {
	if g.fd == 0 {
		return nil
	}

	for {
		buf := make([]byte, unsafe.Sizeof(input_event{}))
		// TODO: Should the returned byte count be cared?
		if _, err := unix.Read(g.fd, buf); err != nil {
			if err == unix.EAGAIN {
				break
			}
			// Disconnected
			if err == unix.ENODEV {
				g.close()
				return nil
			}
			return fmt.Errorf("gamepad: Read failed: %w", err)
		}

		// time is not used.
		e := input_event{
			typ:   uint16(buf[16]) | uint16(buf[17])<<8,
			code:  uint16(buf[18]) | uint16(buf[19])<<8,
			value: int32(buf[20]) | int32(buf[21])<<8 | int32(buf[22])<<16 | int32(buf[23])<<24,
		}

		if e.typ == unix.EV_SYN {
			switch e.code {
			case _SYN_DROPPED:
				g.dropped = true
			case _SYN_REPORT:
				g.dropped = false
				g.pollAbsState()
			}
		}
		if g.dropped {
			continue
		}

		switch e.typ {
		case unix.EV_KEY:
			idx := g.keyMap[e.code-_BTN_MISC]
			g.buttons[idx] = e.value != 0
		case unix.EV_ABS:
			g.handleAbsEvent(int(e.code), e.value)
		}
	}
	return nil
}

func (g *nativeGamepad) pollAbsState() error {
	for code := 0; code < _ABS_CNT; code++ {
		if g.absMap[code] < 0 {
			continue
		}
		if err := ioctl(g.fd, uint(_EVIOCGABS(uint(code))), unsafe.Pointer(&g.absInfo[code])); err != nil {
			return fmt.Errorf("gamepad: ioctl for an abs at pollAbsState failed: %w", err)
		}
		g.handleAbsEvent(code, g.absInfo[code].value)
	}
	return nil
}

func (g *nativeGamepad) handleAbsEvent(code int, value int32) {
	index := g.absMap[code]

	if code >= _ABS_HAT0X && code <= _ABS_HAT3Y {
		axis := (code - _ABS_HAT0X) % 2

		switch axis {
		case 0:
			switch {
			case value < 0:
				g.hats[index] |= hatLeft
				g.hats[index] &^= hatRight
			case value > 0:
				g.hats[index] &^= hatLeft
				g.hats[index] |= hatRight
			default:
				g.hats[index] &^= hatLeft | hatRight
			}
		case 1:
			switch {
			case value < 0:
				g.hats[index] |= hatUp
				g.hats[index] &^= hatDown
			case value > 0:
				g.hats[index] &^= hatUp
				g.hats[index] |= hatDown
			default:
				g.hats[index] &^= hatUp | hatDown
			}
		}
		return
	}

	info := g.absInfo[code]
	v := float64(value)
	if r := float64(info.maximum) - float64(info.minimum); r != 0 {
		v = (v - float64(info.minimum)) / r
		v = v*2 - 1
	}
	g.axes[index] = v
}

func (*nativeGamepad) hasOwnStandardLayoutMapping() bool {
	return false
}

func (g *nativeGamepad) axisCount() int {
	return g.axisCount_
}

func (g *nativeGamepad) buttonCount() int {
	return g.buttonCount_
}

func (g *nativeGamepad) hatCount() int {
	return g.hatCount_
}

func (g *nativeGamepad) axisValue(axis int) float64 {
	if axis < 0 || axis >= g.axisCount_ {
		return 0
	}
	return g.axes[axis]
}

func (g *nativeGamepad) isButtonPressed(button int) bool {
	if button < 0 || button >= g.buttonCount_ {
		return false
	}
	return g.buttons[button]
}

func (*nativeGamepad) buttonValue(button int) float64 {
	panic("gamepad: buttonValue is not implemented")
}

func (g *nativeGamepad) hatState(hat int) int {
	if hat < 0 || hat >= g.hatCount_ {
		return hatCentered
	}
	return g.hats[hat]
}

func (g *nativeGamepad) vibrate(duration time.Duration, strongMagnitude float64, weakMagnitude float64) {
	// TODO: Implement this (#1452)
}
