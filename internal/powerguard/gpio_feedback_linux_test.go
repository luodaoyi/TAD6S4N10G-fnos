//go:build linux && amd64

package powerguard

import (
	"encoding/binary"
	"io"
	"testing"
	"time"
)

type fakeFeedbackWriter struct {
	writes [][]byte
}

func (writer *fakeFeedbackWriter) Write(data []byte) (int, error) {
	copy := append([]byte(nil), data...)
	writer.writes = append(writer.writes, copy)
	return len(data), nil
}

type fakeFeedbackPort struct {
	value  byte
	writes []byte
}

func (port *fakeFeedbackPort) ReadAt(data []byte, offset int64) (int, error) {
	if offset != gpioLEDPort {
		return 0, io.EOF
	}
	data[0] = port.value
	return 1, nil
}

func (port *fakeFeedbackPort) WriteAt(data []byte, offset int64) (int, error) {
	if offset != gpioLEDPort {
		return 0, io.EOF
	}
	port.value = data[0]
	port.writes = append(port.writes, data[0])
	return 1, nil
}

func TestPlayGPIOTonesWritesEVSNDToneWithoutSleeping(t *testing.T) {
	writer := &fakeFeedbackWriter{}
	if err := playGPIOTones(writer, 2, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if len(writer.writes) != 4 {
		t.Fatalf("got %d input events, want 4", len(writer.writes))
	}
	for i, event := range writer.writes {
		if len(event) != 24 || binary.LittleEndian.Uint16(event[16:18]) != 0x12 || binary.LittleEndian.Uint16(event[18:20]) != 0x02 {
			t.Fatalf("event %d is not EV_SND/SND_TONE: %x", i, event)
		}
	}
	if got := binary.LittleEndian.Uint32(writer.writes[0][20:24]); got != gpioFeedbackToneHz {
		t.Fatalf("first event tone = %d, want %d", got, gpioFeedbackToneHz)
	}
	if got := binary.LittleEndian.Uint32(writer.writes[1][20:24]); got != 0 {
		t.Fatalf("second event tone = %d, want off", got)
	}
}

func TestFlashGPIOLEDPreservesOtherBitsAndRestoresOriginalValue(t *testing.T) {
	port := &fakeFeedbackPort{value: 0b1010_0110}
	if err := flashGPIOLED(port, 2, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	wantWrites := []byte{
		0b1010_0111,
		0b1010_0110,
		0b1010_0111,
		0b1010_0110,
		0b1010_0110,
	}
	if len(port.writes) != len(wantWrites) {
		t.Fatalf("writes = %08b, want %08b", port.writes, wantWrites)
	}
	for i, want := range wantWrites {
		if port.writes[i] != want {
			t.Fatalf("write %d = %08b, want %08b", i, port.writes[i], want)
		}
	}
	if port.value != 0b1010_0110 {
		t.Fatalf("final value = %08b, want original", port.value)
	}
}
