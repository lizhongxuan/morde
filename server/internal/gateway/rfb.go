package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
)

const (
	maxRFBMessageBytes      = 1 << 20
	maxWebSocketFrameBytes  = maxRFBMessageBytes + 14
	maxWebSocketBufferBytes = maxWebSocketFrameBytes + (32 << 10)
	rfbInputWriteTimeout    = 5 * time.Second
)

type rfbClientMessage struct {
	data  []byte
	input bool
}

const (
	rfbExtendedClipboardCaps = 1 << 24
	rfbFenceRequest          = 1 << 31
)

// rfbClientParser recognizes the fixed AIO image's RFB 3.8, None-security
// handshake and bounded client-to-server messages used by noVNC. Unknown
// protocol extensions fail closed.
type rfbClientParser struct {
	stage int
	buf   []byte
}

func (p *rfbClientParser) feed(data []byte) ([]rfbClientMessage, error) {
	if len(p.buf)+len(data) > maxRFBMessageBytes+24 {
		return nil, fmt.Errorf("RFB client message exceeds the configured limit")
	}
	p.buf = append(p.buf, data...)
	var messages []rfbClientMessage
	for {
		if p.stage < 3 {
			need := 1
			if p.stage == 0 {
				need = 12
			}
			if len(p.buf) < need {
				return messages, nil
			}
			message := append([]byte(nil), p.buf[:need]...)
			p.buf = p.buf[need:]
			switch p.stage {
			case 0:
				if string(message) != "RFB 003.008\n" {
					return nil, fmt.Errorf("unsupported RFB protocol version")
				}
			case 1:
				if message[0] != 1 {
					return nil, fmt.Errorf("unsupported RFB security type")
				}
			case 2:
				if message[0] > 1 {
					return nil, fmt.Errorf("invalid RFB client initialization")
				}
			}
			messages = append(messages, rfbClientMessage{data: message})
			p.stage++
			continue
		}
		length, input, ready, err := rfbClientMessageLength(p.buf)
		if err != nil {
			return nil, err
		}
		if !ready || len(p.buf) < length {
			return messages, nil
		}
		message := append([]byte(nil), p.buf[:length]...)
		p.buf = p.buf[length:]
		messages = append(messages, rfbClientMessage{data: message, input: input})
	}
}

func rfbClientMessageLength(buf []byte) (length int, input, ready bool, err error) {
	if len(buf) == 0 {
		return 0, false, false, nil
	}
	switch buf[0] {
	case 0: // SetPixelFormat
		return 20, false, true, nil
	case 2: // SetEncodings
		if len(buf) < 4 {
			return 0, false, false, nil
		}
		return 4 + int(binary.BigEndian.Uint16(buf[2:4]))*4, false, true, nil
	case 3: // FramebufferUpdateRequest
		return 10, false, true, nil
	case 4: // KeyEvent
		return 8, true, true, nil
	case 5: // PointerEvent
		return 6, true, true, nil
	case 6: // ClientCutText
		if len(buf) < 8 {
			return 0, false, false, nil
		}
		textLength := int32(binary.BigEndian.Uint32(buf[4:8]))
		if textLength >= 0 {
			if textLength > maxRFBMessageBytes-8 {
				return 0, false, false, fmt.Errorf("RFB clipboard message exceeds the configured limit")
			}
			return 8 + int(textLength), true, true, nil
		}
		extendedLength := -int64(textLength)
		if extendedLength < 4 || extendedLength > maxRFBMessageBytes-8 {
			return 0, false, false, fmt.Errorf("RFB clipboard message exceeds the configured limit")
		}
		if len(buf) < 12 {
			return 0, false, false, nil
		}
		flags := binary.BigEndian.Uint32(buf[8:12])
		if flags&0x00ff0000 != 0 {
			return 0, false, false, fmt.Errorf("RFB extended clipboard message has reserved flags set")
		}
		if flags&rfbExtendedClipboardCaps != 0 {
			formatCount := bits.OnesCount16(uint16(flags))
			if extendedLength != int64(4+formatCount*4) {
				return 0, false, false, fmt.Errorf("invalid RFB extended clipboard capabilities message")
			}
			return 8 + int(extendedLength), false, true, nil
		}
		return 8 + int(extendedLength), true, true, nil
	case 255: // QEMU ExtendedKeyEvent.
		if len(buf) < 2 {
			return 0, false, false, nil
		}
		if buf[1] != 0 {
			return 0, false, false, fmt.Errorf("unsupported RFB QEMU client message subtype %d", buf[1])
		}
		if len(buf) < 4 {
			return 0, false, false, nil
		}
		if down := binary.BigEndian.Uint16(buf[2:4]); down > 1 {
			return 0, false, false, fmt.Errorf("invalid RFB extended key event state")
		}
		return 12, true, true, nil
	case 150: // EnableContinuousUpdates
		return 10, false, true, nil
	case 248: // Fence response; requests are input and remain blocked in AI mode.
		if len(buf) < 9 {
			return 0, false, false, nil
		}
		length := int(buf[8])
		if length > 64 {
			return 0, false, false, fmt.Errorf("RFB fence payload exceeds the configured limit")
		}
		flags := binary.BigEndian.Uint32(buf[4:8])
		return 9 + length, flags&rfbFenceRequest != 0, true, nil
	case 251: // SetDesktopSize
		if len(buf) < 8 {
			return 0, false, false, nil
		}
		return 8 + int(buf[6])*16, true, true, nil
	default:
		return 0, false, false, fmt.Errorf("unsupported RFB client message type %d", buf[0])
	}
}

type rfbFilteringBody struct {
	upstream   io.ReadWriteCloser
	ctx        context.Context
	ref        host.Ref
	permits    app.HostInputPermitStore
	allowInput bool
	onError    func(error)
	frames     []byte
	rfb        rfbClientParser
	fragmented bool
}

func newRFBFilteringBody(upstream io.ReadWriteCloser, ctx context.Context, ref host.Ref, permits app.HostInputPermitStore, allowInput bool, onError func(error)) *rfbFilteringBody {
	return &rfbFilteringBody{upstream: upstream, ctx: ctx, ref: ref, permits: permits, allowInput: allowInput, onError: onError}
}

func (b *rfbFilteringBody) Read(p []byte) (int, error) { return b.upstream.Read(p) }

func (b *rfbFilteringBody) Write(p []byte) (written int, err error) {
	defer func() {
		if err != nil && b.onError != nil {
			b.onError(err)
		}
	}()
	if len(b.frames)+len(p) > maxWebSocketBufferBytes {
		return 0, fmt.Errorf("WebSocket input buffer exceeds the configured limit")
	}
	b.frames = append(b.frames, p...)
	for len(b.frames) > 0 {
		frameLength, payloadStart, payloadLength, fin, opcode, err := clientWebSocketFrameLength(b.frames)
		if err != nil {
			return 0, err
		}
		if frameLength == 0 {
			return len(p), nil
		}
		frame := b.frames[:frameLength]
		payload := make([]byte, payloadLength)
		mask := frame[payloadStart-4 : payloadStart]
		for index := range payload {
			payload[index] = frame[payloadStart+index] ^ mask[index&3]
		}
		if opcode >= 8 {
			if _, err := b.upstream.Write(frame); err != nil {
				return 0, err
			}
		} else {
			if err := b.acceptDataFrame(fin, opcode, payload); err != nil {
				return 0, err
			}
		}
		b.frames = b.frames[frameLength:]
	}
	return len(p), nil
}

func clientWebSocketFrameLength(buf []byte) (frameLength, payloadStart, payloadLength int, fin bool, opcode byte, err error) {
	if len(buf) < 2 {
		return 0, 0, 0, false, 0, nil
	}
	first, second := buf[0], buf[1]
	fin = first&0x80 != 0
	opcode = first & 0x0f
	if first&0x70 != 0 || second&0x80 == 0 {
		return 0, 0, 0, false, 0, fmt.Errorf("invalid or unmasked client WebSocket frame")
	}
	length := uint64(second & 0x7f)
	headerLength := 2
	switch length {
	case 126:
		if len(buf) < 4 {
			return 0, 0, 0, false, 0, nil
		}
		length = uint64(binary.BigEndian.Uint16(buf[2:4]))
		headerLength += 2
		if length < 126 {
			return 0, 0, 0, false, 0, fmt.Errorf("non-canonical WebSocket frame length")
		}
	case 127:
		if len(buf) < 10 {
			return 0, 0, 0, false, 0, nil
		}
		length = binary.BigEndian.Uint64(buf[2:10])
		headerLength += 8
		if length <= 65535 || length>>63 != 0 {
			return 0, 0, 0, false, 0, fmt.Errorf("invalid WebSocket frame length")
		}
	}
	control := opcode >= 8
	if opcode != 0 && opcode != 2 && opcode != 8 && opcode != 9 && opcode != 10 {
		return 0, 0, 0, false, 0, fmt.Errorf("unsupported WebSocket opcode %d", opcode)
	}
	if control && (!fin || length > 125) {
		return 0, 0, 0, false, 0, fmt.Errorf("invalid WebSocket control frame")
	}
	if length > maxRFBMessageBytes {
		return 0, 0, 0, false, 0, fmt.Errorf("WebSocket frame exceeds the configured limit")
	}
	frameLength = headerLength + 4 + int(length)
	if len(buf) < frameLength {
		return 0, 0, 0, false, 0, nil
	}
	return frameLength, headerLength + 4, int(length), fin, opcode, nil
}

func (b *rfbFilteringBody) acceptDataFrame(fin bool, opcode byte, payload []byte) error {
	switch opcode {
	case 2:
		if b.fragmented {
			return fmt.Errorf("interleaved WebSocket data frames are not supported")
		}
		b.fragmented = !fin
	case 0:
		if !b.fragmented {
			return fmt.Errorf("unexpected WebSocket continuation frame")
		}
		b.fragmented = !fin
	}
	messages, err := b.rfb.feed(payload)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}
	containsInput := false
	var data []byte
	for _, message := range messages {
		if message.input && !b.allowInput {
			continue
		}
		containsInput = containsInput || message.input
		data = append(data, message.data...)
	}
	if len(data) == 0 {
		return nil
	}
	if containsInput {
		if b.ref.ControlState != host.ControlUser {
			return fmt.Errorf("RFB input is not permitted in the current Host control mode")
		}
		ctx, cancel := context.WithTimeout(b.ctx, rfbInputWriteTimeout)
		defer cancel()
		timer := time.AfterFunc(rfbInputWriteTimeout, func() { _ = b.Close() })
		defer timer.Stop()
		return b.permits.WithHostInputPermit(ctx, b.ref, func() error { return b.writeBinary(data) })
	}
	return b.writeBinary(data)
}

func (b *rfbFilteringBody) writeBinary(payload []byte) error {
	mask := [4]byte{}
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("generate WebSocket mask: %w", err)
	}
	length := len(payload)
	header := make([]byte, 0, 14)
	header = append(header, 0x82)
	switch {
	case length < 126:
		header = append(header, 0x80|byte(length))
	case length <= 65535:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		header = append(header, extended[:]...)
	}
	header = append(header, mask[:]...)
	frame := make([]byte, len(header)+len(payload))
	copy(frame, header)
	masked := make([]byte, len(payload))
	for index := range payload {
		masked[index] = payload[index] ^ mask[index&3]
	}
	copy(frame[len(header):], masked)
	for len(frame) > 0 {
		written, err := b.upstream.Write(frame)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		frame = frame[written:]
	}
	return nil
}

func (b *rfbFilteringBody) Close() error { return b.upstream.Close() }
