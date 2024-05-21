package cyw43439

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/soypat/cyw43439/whd"
)

var (
	errUnalignedBuffer        = errors.New("cyw: unaligned buffer")
	errHCIPacketTooLarge      = errors.New("cyw: hci packet too large")
	errBTWakeTimeout          = errors.New("cyw: bt wake timeout")
	errBTReadyTimeout         = errors.New("cyw: bt ready timeout")
	errTimeout                = errors.New("cyw: timeout")
	errZeroBTAddr             = errors.New("cyw: btaddr=0")
	errBTInvalidVersionLength = errors.New("invalid bt version length")
	errBTWatermark            = errors.New("bt watermark set failed")
)

type deviceHCI struct {
	dev *Device
}

func (d *deviceHCI) Buffered() int {
	return d.dev.BufferedHCI()
}

func (d *deviceHCI) Read(b []byte) (int, error) {
	return d.dev.ReadHCI(b)
}

func (d *deviceHCI) Write(b []byte) (int, error) {
	return d.dev.WriteHCI(b)
}

// HCIReaderWriter returns a io.ReadWriter interface which wraps the BufferedHCI, WriteHCI and ReadHCI methods.
func (d *Device) HCIReaderWriter() (io.ReadWriter, error) {
	if !d.bt_mode_enabled() {
		return nil, errors.New("need to enable bluetooth in Init to use HCI interface")
	}
	return &deviceHCI{
		dev: d,
	}, nil
}

// BufferedHCI returns amounts of HCI bytes stored inside CYW43439 internal ring buffer.
func (d *Device) BufferedHCI() int {
	err := d.acquire(modeBluetooth)
	defer d.release()
	if err != nil {
		return 0
	}
	n32, _ := d.hci_buffered()
	return int(n32)
}

// WriteHCI sends a HCI packet over the CYW43439's interface. Used for bluetooth.
func (d *Device) WriteHCI(b []byte) (int, error) {
	err := d.acquire(modeBluetooth)
	defer d.release()
	if err != nil {
		return 0, err
	}
	err = d.hci_write(b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// WriteHCI reads from HCI ring buffer internal to the CYW43439. Used for bluetooth.
func (d *Device) ReadHCI(b []byte) (int, error) {
	err := d.acquire(modeBluetooth)
	defer d.release()
	if err != nil {
		return 0, err
	}
	return d.hci_read(b)
}

func (d *Device) bt_mode_enabled() bool {
	return d.mode&modeBluetooth != 0
}

func (d *Device) bt_init(firmware string) error {
	d.trace("bt_init")
	err := d.bp_write32(whd.CYW_BT_BASE_ADDRESS+whd.BT2WLAN_PWRUP_ADDR, whd.BT2WLAN_PWRUP_WAKE)
	if err != nil {
		return err
	}
	time.Sleep(2 * time.Millisecond)
	err = d.bt_upload_firmware(firmware)
	if err != nil {
		return err
	}
	d.trace("bt:firmware-upload-finished")
	err = d.bt_wait_ready()
	if err != nil {
		return err
	}
	err = d.bt_init_buffers()
	if err != nil {
		return err
	}
	err = d.bt_wait_awake()
	if err != nil {
		return err
	}
	err = d.bt_set_host_ready()
	if err != nil {
		return err
	}
	d.bt_toggle_intr()
	if err != nil {
		return err
	}
	return nil
}

func (d *Device) bt_upload_firmware(firmware string) error {
	versionlength := firmware[0]
	_version := firmware[1:versionlength]
	d.trace("bt_init:start", slog.String("fwversion", _version), slog.Int("versionlen", int(versionlength)))
	// Skip version + length byte + 1 extra byte as per cybt_shared_bus_driver.c
	firmware = firmware[versionlength+2:]
	// buffers
	rawbuffer := u32AsU8(d._sendIoctlBuf[:])
	alignedDataBuffer := rawbuffer[:256]
	btfwCB := firmware
	hfd := hexFileData{
		addrmode: whd.BTFW_ADDR_MODE_EXTENDED,
	}
	var memoryValueBytes [4]byte
	for {
		var numFwBytes uint32
		numFwBytes, btfwCB = bt_read_firmware_patch_line(btfwCB, &hfd)
		if numFwBytes == 0 {
			break
		}
		d.trace("BTpatch", slog.Int("addrmode", int(hfd.addrmode)), slog.Uint64("len", uint64(numFwBytes)))
		fwBytes := hfd.ds[:numFwBytes]
		dstStartAddr := hfd.dstAddr + whd.CYW_BT_BASE_ADDRESS
		var alignedDataBufferIdx uint32
		if !isaligned(dstStartAddr, 4) {
			// Pad with bytes already in memory.
			numPadBytes := dstStartAddr % 4
			paddedDstStartAddr := aligndown(dstStartAddr, 4)
			memoryValue, _ := d.bp_read32(paddedDstStartAddr)

			_busOrder.PutUint32(memoryValueBytes[:], memoryValue)
			for i := 0; i < int(numPadBytes); i++ {
				alignedDataBuffer[alignedDataBufferIdx] = memoryValueBytes[i]
				alignedDataBufferIdx++
			}
			// Copy firmware bytes after the padding bytes.
			for i := 0; i < int(numFwBytes); i++ {
				alignedDataBuffer[alignedDataBufferIdx] = fwBytes[i]
				alignedDataBufferIdx++
			}

			dstStartAddr = paddedDstStartAddr
		} else {
			// Directly copy fw_bytes into aligned_data_buffer if no start padding is required
			for i := 0; i < int(numFwBytes); i++ {
				alignedDataBuffer[alignedDataBufferIdx] = fwBytes[i]
				alignedDataBufferIdx++
			}
		}

		// pad end.
		dstEndAddr := dstStartAddr + alignedDataBufferIdx
		if !isaligned(dstEndAddr, 4) {
			offset := dstEndAddr % 4
			numPadBytesEnd := 4 - offset
			paddedDstEndAddr := aligndown(dstEndAddr, 4)
			memoryValue, _ := d.bp_read32(paddedDstEndAddr)
			_busOrder.PutUint32(memoryValueBytes[:], memoryValue)
			for i := offset; i < 4; i++ {
				alignedDataBuffer[alignedDataBufferIdx] = memoryValueBytes[i]
				alignedDataBufferIdx++
			}
			dstEndAddr += numPadBytesEnd
		}

		bufferToWrite := alignedDataBuffer[0:alignedDataBufferIdx]
		if dstStartAddr%4 != 0 || dstEndAddr%4 != 0 || alignedDataBufferIdx%4 != 0 {
			return errors.New("unaligned BT firmware bug")
		}

		const chunksize = 64 // Is writing in 64 byte chunks needed?
		numChunks := alignedDataBufferIdx/64 + b2u32(alignedDataBufferIdx%64 != 0)
		for i := uint32(0); i < numChunks; i++ {
			offset := i * chunksize
			end := (i + 1) * chunksize
			if end > alignedDataBufferIdx {
				end = alignedDataBufferIdx
			}
			chunk := bufferToWrite[offset:end]
			// d.trace("chunk-write", slog.Uint64("idx", uint64(i)), slog.Uint64("offset", uint64(offset)), slog.Uint64("end", uint64(end)), slog.Uint64("len", uint64(end-offset)))
			err := d.bp_write(dstStartAddr+offset, chunk)
			if err != nil {
				return err
			}
			time.Sleep(time.Millisecond) // TODO: is this sleep needed?
		}
	}
	return nil
}

func (d *Device) hci_buffered() (uint32, error) {
	d.trace("hci_buffered")
	// Check if buffer contains data.
	newPtr, err := d.bp_read32(d.btaddr + whd.BTSDIO_OFFSET_BT2HOST_IN)
	if err != nil {
		return 0, err
	}
	available := (d.b2hReadPtr - newPtr) % whd.BTSDIO_FWBUF_SIZE
	if available < 4 {
		return 0, nil
	}
	// Read the HCI packet without advancing buffer.
	// This is done since ring buffer sometimes returns a spurious large number.
	buf := u32AsU8(d._rxBuf[:])
	err = d.hci_raw_read_ringbuf(buf[:4])
	if err != nil {
		return 0, err
	}
	buffered := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16
	buffered += 4 // Add HCI header.
	d.debug("hci_buffered", slog.Uint64("buffered", uint64(buffered)))
	return buffered, nil
}

func (d *Device) hci_read(b []byte) (int, error) {
	d.trace("hci_read", slog.Int("inputlen", len(b)))
	// Calculate length of HCI packet.
	err := d.hci_read_ringbuf(b[:4], true)
	if err != nil {
		return 0, err
	}
	length := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
	roundedLen := int(alignup(length, 4)) + 4
	if roundedLen > len(b) {
		return 0, io.ErrShortBuffer
	}
	err = d.hci_read_ringbuf(b[4:roundedLen], true)
	if err != nil {
		return 0, err
	}
	// Release bus.
	err = d.bt_toggle_intr()
	if err != nil {
		return roundedLen, err
	}
	err = d.bt_bus_release()
	if err != nil {
		return roundedLen, err
	}
	return roundedLen, nil
}

// hci_wait_read_buffered blocks until there are at least n bytes ready to read.
func (d *Device) hci_wait_read_buffered(n int) error {
	d.trace("hci_wait_read_buffered", slog.Int("n", n))
	for {
		// Block on no data available.
		available, err := d.hci_buffered()
		if int(available) >= n {
			break
		} else if err != nil {
			return err
		}
		time.Sleep(time.Second)
	}
	return nil
}

// hci_read_ringbuf fills the entire contents of buf with the first contents of
// the ring buffer. It advances the pointer of the ring buffer len(buf) on successful read.
func (d *Device) hci_read_ringbuf(buf []byte, advancePtr bool) error {
	d.trace("hci_read_ringbuf", slog.Int("len", len(buf)), slog.Bool("advptr", advancePtr))
	if len(buf)%4 != 0 || len(buf) > math.MaxInt32 {
		return errUnalignedBuffer
	}
	err := d.hci_wait_read_buffered(len(buf))
	if err != nil {
		return err
	}
	err = d.hci_raw_read_ringbuf(buf)
	if err != nil {
		return err
	}
	if advancePtr {
		return d.hci_advance_read_ringbuf(uint32(len(buf)))
	}
	return nil
}

// hci_raw_read_ringbuf reads the next len(buf) bytes into the buffer without checking for validity of bytes.
// Does not advance ringbuffer pointer.
func (d *Device) hci_raw_read_ringbuf(buf []byte) (err error) {
	d.trace("hci_raw_read_ringbuf")
	addr := d.btaddr + whd.BTSDIO_OFFSET_HOST_READ_BUF + d.b2hReadPtr
	if d.b2hReadPtr+uint32(len(buf)) > whd.BTSDIO_FWBUF_SIZE {
		// Special case: Wrap around of ring-buffer.
		n := whd.BTSDIO_FWBUF_SIZE - d.b2hReadPtr
		err = d.bp_read(addr, buf[:n])
		if err == nil {
			addr = d.btaddr + whd.BTSDIO_OFFSET_HOST_READ_BUF
			err = d.bp_read(addr, buf[n:])
		}
	} else {
		err = d.bp_read(addr, buf[:])
	}
	return err
}

// hci_advance_read_ringbuf advances the CYW43439's internal ring buffer read pointer, a.k.a offset.
func (d *Device) hci_advance_read_ringbuf(n uint32) error {
	newPtr := (d.b2hReadPtr + n) % whd.BTSDIO_FWBUF_SIZE
	err := d.bp_write32(d.btaddr+whd.BTSDIO_OFFSET_BT2HOST_OUT, newPtr)
	d.trace("hci_advance_read_ringbuf",
		slog.Uint64("newptr", uint64(newPtr)),
		slog.Uint64("oldptr", uint64(d.b2hReadPtr)),
		slog.Uint64("n", uint64(n)),
		slog.Bool("err", err != nil),
	)
	if err == nil {
		d.b2hReadPtr = newPtr
	}
	return err
}

func (d *Device) hci_write(b []byte) error {
	d.trace("hci_write:start")
	buflen := len(b)
	alignBuflen := alignup(uint32(buflen), 4)
	if buflen != int(alignBuflen) {
		return errUnalignedBuffer
	}
	cmdlen := buflen + 3 - 4 // Add 3 bytes for SDIO header (revise)

	bufWithCmd := u32AsU8(d._sendIoctlBuf[:256/4])
	if buflen > len(bufWithCmd)-3 {
		return errHCIPacketTooLarge
	}
	bufWithCmd[0] = byte(cmdlen)
	bufWithCmd[1] = byte(cmdlen >> 8)
	bufWithCmd[2] = 0
	copy(bufWithCmd[3:], b)

	paddedBufWithCmd := bufWithCmd[0:alignBuflen]
	err := d.bt_bus_request()
	if err != nil {
		return err
	}
	addr := d.btaddr + whd.BTSDIO_OFFSET_HOST_WRITE_BUF + d.h2bWritePtr
	err = d.bp_write(addr, paddedBufWithCmd)
	if err != nil {
		return err
	}
	d.h2bWritePtr += uint32(len(paddedBufWithCmd))
	err = d.bp_write32(d.btaddr+whd.BTSDIO_OFFSET_HOST2BT_IN, d.h2bWritePtr)
	if err != nil {
		return err
	}
	err = d.bt_toggle_intr()
	if err != nil {
		return err
	}
	return d.bt_bus_release()
}

func (d *Device) bt_wait_ready() error {
	if err := d.bt_wait_ctrl_bits(whd.BTSDIO_REG_FW_RDY_BITMASK, 300); err != nil {
		return errBTReadyTimeout
	}
	return nil
}

func (d *Device) bt_wait_awake() error {
	if err := d.bt_wait_ctrl_bits(whd.BTSDIO_REG_BT_AWAKE_BITMASK, 300); err != nil {
		return errBTWakeTimeout
	}
	return nil
}

func (d *Device) bt_wait_ctrl_bits(bits uint32, timeout_ms int) (err error) {
	d.trace("bt_wait_ctrl_bits:start", slog.Uint64("bits", uint64(bits)))
	var val uint32
	for i := 0; i < timeout_ms/4+3; i++ {
		val, err = d.bp_read32(whd.BT_CTRL_REG_ADDR)
		if err != nil {
			return err
		}
		if val&bits != 0 {
			return nil
		}
		time.Sleep(4 * time.Millisecond)
	}
	d.logerr("bt:ctrl-timeout", slog.Uint64("got", uint64(val)), slog.Uint64("want", uint64(bits)))
	return errTimeout
}

func (d *Device) bt_set_host_ready() error {
	d.trace("bt_set_host_ready:start")
	oldval, err := d.bp_read32(whd.HOST_CTRL_REG_ADDR)
	if err != nil {
		return err
	}
	newval := oldval | whd.BTSDIO_REG_SW_RDY_BITMASK
	return d.bp_write32(whd.HOST_CTRL_REG_ADDR, newval)
}

func (d *Device) bt_set_awake(awake bool) error {
	d.trace("bt_set_awake:start")
	oldval, err := d.bp_read32(whd.HOST_CTRL_REG_ADDR)
	if err != nil {
		return err
	}
	// Swap endianness on this read?
	var newval uint32
	if awake {
		newval = oldval | whd.BTSDIO_REG_WAKE_BT_BITMASK
	} else {
		newval = oldval &^ whd.BTSDIO_REG_WAKE_BT_BITMASK
	}
	return d.bp_write32(whd.HOST_CTRL_REG_ADDR, newval)
}

func (d *Device) bt_toggle_intr() error {
	d.trace("bt_toggle_intr:start")
	oldval, err := d.bp_read32(whd.HOST_CTRL_REG_ADDR)
	if err != nil {
		return err
	}
	// TODO(soypat): Swap endianness on this read?
	newval := oldval ^ whd.BTSDIO_REG_DATA_VALID_BITMASK
	return d.bp_write32(whd.HOST_CTRL_REG_ADDR, newval)
}

func (d *Device) bt_set_intr() error {
	d.trace("bt_set_intr:start")
	oldval, err := d.bp_read32(whd.HOST_CTRL_REG_ADDR)
	if err != nil {
		return err
	}
	newval := oldval | whd.BTSDIO_REG_DATA_VALID_BITMASK
	return d.bp_write32(whd.HOST_CTRL_REG_ADDR, newval)
}

func (d *Device) bt_init_buffers() error {
	d.trace("bt_init_buffers:start")
	btaddr, err := d.bp_read32(whd.WLAN_RAM_BASE_REG_ADDR)
	if err != nil {
		return err
	} else if btaddr == 0 {
		return errZeroBTAddr
	}
	d.btaddr = btaddr
	d.bp_write32(btaddr+whd.BTSDIO_OFFSET_HOST2BT_IN, 0)
	d.bp_write32(btaddr+whd.BTSDIO_OFFSET_HOST2BT_OUT, 0)
	d.bp_write32(btaddr+whd.BTSDIO_OFFSET_BT2HOST_IN, 0)
	return d.bp_write32(btaddr+whd.BTSDIO_OFFSET_BT2HOST_OUT, 0)
}

func (d *Device) bt_bus_request() error {
	err := d.bt_set_awake(true)
	if err != nil {
		return err
	}
	return d.bt_wait_awake()
}

func (d *Device) bt_bus_release() error {
	return nil
}

func (d *Device) bt_has_work() bool {
	d.trace("bt_has_work:start")
	intstat, _ := d.bp_read32(whd.SDIO_BASE_ADDRESS)
	if intstat&whd.I_HMB_FC_CHANGE != 0 {
		d.bp_write32(whd.SDIO_BASE_ADDRESS+whd.SDIO_INT_STATUS, intstat&whd.I_HMB_FC_CHANGE)
		return true
	}
	return false
}

type hexFileData struct {
	addrmode int32
	hiaddr   uint16
	dstAddr  uint32
	ds       [256]byte
}

// bt_read_firmware_patch_line reads firmware addressing scheme into hfd and returns the patch line length stored into hfd.
func bt_read_firmware_patch_line(cbFirmware string, hfd *hexFileData) (uint32, string) {
	var absBaseAddr32 uint32
	nxtLineStart := cbFirmware
	for {
		numBytes := nxtLineStart[0]
		nxtLineStart = nxtLineStart[1:]

		addr := uint16(nxtLineStart[0])<<8 | uint16(nxtLineStart[1])
		nxtLineStart = nxtLineStart[2:]

		lineType := nxtLineStart[0]
		nxtLineStart = nxtLineStart[1:]
		if numBytes == 0 {
			break
		}
		copy(hfd.ds[:numBytes], nxtLineStart[:numBytes])
		nxtLineStart = nxtLineStart[numBytes:]
		switch lineType {
		case whd.BTFW_HEX_LINE_TYPE_EXTENDED_ADDRESS:
			hfd.hiaddr = uint16(hfd.ds[0])<<8 | uint16(hfd.ds[1])
			hfd.addrmode = whd.BTFW_ADDR_MODE_EXTENDED

		case whd.BTFW_HEX_LINE_TYPE_EXTENDED_SEGMENT_ADDRESS:
			hfd.hiaddr = uint16(hfd.ds[0])<<8 | uint16(hfd.ds[1])
			hfd.addrmode = whd.BTFW_ADDR_MODE_SEGMENT

		case whd.BTFW_HEX_LINE_TYPE_ABSOLUTE_32BIT_ADDRESS:
			absBaseAddr32 = uint32(hfd.ds[0])<<24 | uint32(hfd.ds[1])<<16 |
				uint32(hfd.ds[2])<<8 | uint32(hfd.ds[3])
			hfd.addrmode = whd.BTFW_ADDR_MODE_LINEAR32

		case whd.BTFW_HEX_LINE_TYPE_DATA:
			hfd.dstAddr = uint32(addr)
			switch hfd.addrmode {
			case whd.BTFW_ADDR_MODE_EXTENDED:
				hfd.dstAddr += uint32(hfd.hiaddr) << 16
			case whd.BTFW_ADDR_MODE_SEGMENT:
				hfd.dstAddr += uint32(hfd.hiaddr) << 4
			case whd.BTFW_ADDR_MODE_LINEAR32:
				hfd.dstAddr += absBaseAddr32
			}
			return uint32(numBytes), nxtLineStart
		default:
			// println("skip line type", lineType)
		}
	}
	return 0, nxtLineStart
}
