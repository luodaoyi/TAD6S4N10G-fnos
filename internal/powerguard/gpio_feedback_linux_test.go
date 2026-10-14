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

// 提示音按编排逐声播放：两声之间要静默 gpioToneGap，且每声的记录时长取自编排。
func TestPlayGPIOChimeFollowsSpec(t *testing.T) {
	writer := &fakeFeedbackWriter{}
	var slept []time.Duration
	tones := []GPIOToneSpec{
		{FrequencyHz: 880, DurationMS: 800},
		{FrequencyHz: 1200, DurationMS: 150},
	}
	if err := playGPIOChime(writer, tones, func(duration time.Duration) { slept = append(slept, duration) }); err != nil {
		t.Fatal(err)
	}
	// 每声两次写（开声 / 关声），两声共 4 次。
	if len(writer.writes) != 4 {
		t.Fatalf("got %d input events, want 4", len(writer.writes))
	}
	if got := binary.LittleEndian.Uint32(writer.writes[0][20:24]); got != 880 {
		t.Fatalf("第一声频率 = %d，应为 880", got)
	}
	if got := binary.LittleEndian.Uint32(writer.writes[2][20:24]); got != 1200 {
		t.Fatalf("第二声频率 = %d，应为 1200", got)
	}
	if got := binary.LittleEndian.Uint32(writer.writes[1][20:24]); got != 0 {
		t.Fatalf("第一声应当被关掉，实际 %d", got)
	}
	wantSlept := []time.Duration{800 * time.Millisecond, gpioToneGap, 150 * time.Millisecond}
	if len(slept) != len(wantSlept) {
		t.Fatalf("静默次数 = %v，应为 %v", slept, wantSlept)
	}
	for i, want := range wantSlept {
		if slept[i] != want {
			t.Fatalf("第 %d 段静默 = %v，应为 %v", i, slept[i], want)
		}
	}
	if err := playGPIOChime(writer, nil, func(time.Duration) {}); err != nil {
		t.Fatalf("空编排应当直接返回: %v", err)
	}
}

// 非法参数回落到与按键反馈一致的默认值，别让一个坏参数把整段提示音弄哑。
func TestPlayGPIOChimeFallsBackToDefaults(t *testing.T) {
	writer := &fakeFeedbackWriter{}
	var slept []time.Duration
	if err := playGPIOChime(writer, []GPIOToneSpec{{}}, func(duration time.Duration) { slept = append(slept, duration) }); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(writer.writes[0][20:24]); got != gpioFeedbackToneHz {
		t.Fatalf("频率应回落到 %d，实际 %d", gpioFeedbackToneHz, got)
	}
	if len(slept) != 1 || slept[0] != gpioToneDuration {
		t.Fatalf("时长应回落到 %v，实际 %v", gpioToneDuration, slept)
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
