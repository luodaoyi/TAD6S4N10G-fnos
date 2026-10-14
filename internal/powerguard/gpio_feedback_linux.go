//go:build linux && amd64

package powerguard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

var gpioFeedbackMu sync.Mutex

const (
	gpioSpeakerEventPath = "/dev/input/by-path/platform-pcspkr-event-spkr"
	gpioLEDPort          = int64(0xA00)
	gpioLEDBit           = uint(0)
	// 反馈参数与 RR 官方插件 tad6s4n10g 对齐：
	//   btnBeep 3 -> beep -r 3（440 Hz，每声 200 ms，间隔 100 ms）
	//   btnLed  3 -> 亮 0.5 s / 灭 0.5 s，重复 3 次
	gpioFeedbackToneHz = 440
	gpioToneDuration   = 200 * time.Millisecond
	gpioToneGap        = 100 * time.Millisecond
	gpioFlashDuration  = 500 * time.Millisecond
)

type gpioPortReadWriter interface {
	io.ReaderAt
	io.WriterAt
}

type linuxGPIOToneOutput struct {
	path string
}

func (output linuxGPIOToneOutput) PlayTones(count int) error {
	file, err := os.OpenFile(output.path, os.O_WRONLY, 0)
	if err != nil {
		return gpioFeedbackUnavailable("open PC speaker", output.path, err)
	}
	defer func() { _ = file.Close() }()
	return playGPIOTones(file, count, time.Sleep)
}

// PlayChime 播放一段自定义提示音（存储设备插入提示用）；每声之间固定静默
// gpioToneGap，与按键反馈的手感保持一致。
func (output linuxGPIOToneOutput) PlayChime(tones []GPIOToneSpec) error {
	if len(tones) == 0 {
		return nil
	}
	file, err := os.OpenFile(output.path, os.O_WRONLY, 0)
	if err != nil {
		return gpioFeedbackUnavailable("open PC speaker", output.path, err)
	}
	defer func() { _ = file.Close() }()
	return playGPIOChime(file, tones, time.Sleep)
}

type linuxGPIOLEDOutput struct {
	path string
}

func (output linuxGPIOLEDOutput) Flash(count int) error {
	file, err := os.OpenFile(output.path, os.O_RDWR, 0)
	if err != nil {
		return gpioFeedbackUnavailable("open LED port", output.path, err)
	}
	defer func() { _ = file.Close() }()
	return flashGPIOLED(file, count, time.Sleep)
}

// PlayGPIOFeedback emits best-effort PC-speaker and LED confirmation for a
// threshold event. It is intentionally synchronous; main should call it in a
// goroutine and always execute the release action independently.
func (m *Manager) PlayGPIOFeedback(event GPIOEvent) error {
	if !event.IsFeedback() {
		return nil
	}
	gpioFeedbackMu.Lock()
	defer gpioFeedbackMu.Unlock()
	return runGPIOFeedback(
		*event.Feedback,
		linuxGPIOToneOutput{path: m.rooted(gpioSpeakerEventPath)},
		linuxGPIOLEDOutput{path: m.GPIOPortPath()},
	)
}

func gpioFeedbackUnavailable(operation, path string, err error) error {
	return fmt.Errorf("%w: %s %s: %v", ErrGPIOFeedbackUnavailable, operation, path, err)
}

// PlayStorageChime 播放一次存储设备插入提示音。与按键确认反馈共用同一把锁，
// 免得两段声音叠在一起；提示音是尽力而为的，调用方只记日志。
func (m *Manager) PlayStorageChime(tones []GPIOToneSpec) error {
	if len(tones) == 0 {
		return nil
	}
	gpioFeedbackMu.Lock()
	defer gpioFeedbackMu.Unlock()
	return linuxGPIOToneOutput{path: m.rooted(gpioSpeakerEventPath)}.PlayChime(tones)
}

func playGPIOTones(writer io.Writer, count int, sleep func(time.Duration)) (err error) {
	if count <= 0 {
		return nil
	}
	toneActive := false
	defer func() {
		if toneActive {
			err = errors.Join(err, writePCSpeakerTone(writer, 0))
		}
	}()
	for i := 0; i < count; i++ {
		if err = writePCSpeakerTone(writer, gpioFeedbackToneHz); err != nil {
			return err
		}
		toneActive = true
		sleep(gpioToneDuration)
		if err = writePCSpeakerTone(writer, 0); err != nil {
			return err
		}
		toneActive = false
		if i+1 < count {
			sleep(gpioToneGap)
		}
	}
	return nil
}

// playGPIOChime 按给定的编排逐声播放：每一场都是「开声 → 静默 → 关声」，
// 声与声之间留 gpioToneGap。频率或时长非法的条目回落到与按键反馈一致的
// 440 Hz / 200 ms，保证提示音不会因为一个坏参数整段哑掉。
func playGPIOChime(writer io.Writer, tones []GPIOToneSpec, sleep func(time.Duration)) (err error) {
	if len(tones) == 0 {
		return nil
	}
	toneActive := false
	defer func() {
		if toneActive {
			err = errors.Join(err, writePCSpeakerTone(writer, 0))
		}
	}()
	for index, tone := range tones {
		frequency := tone.FrequencyHz
		if frequency <= 0 {
			frequency = gpioFeedbackToneHz
		}
		duration := time.Duration(tone.DurationMS) * time.Millisecond
		if tone.DurationMS <= 0 {
			duration = gpioToneDuration
		}
		if err = writePCSpeakerTone(writer, int32(frequency)); err != nil {
			return err
		}
		toneActive = true
		sleep(duration)
		if err = writePCSpeakerTone(writer, 0); err != nil {
			return err
		}
		toneActive = false
		if index+1 < len(tones) {
			sleep(gpioToneGap)
		}
	}
	return nil
}

// writePCSpeakerTone writes one Linux input_event (EV_SND/SND_TONE). On the
// supported linux/amd64 target input_event is 24 bytes: timeval followed by
// type, code and value.
func writePCSpeakerTone(writer io.Writer, frequency int32) error {
	var event [24]byte
	binary.LittleEndian.PutUint16(event[16:18], 0x12) // EV_SND
	binary.LittleEndian.PutUint16(event[18:20], 0x02) // SND_TONE
	binary.LittleEndian.PutUint32(event[20:24], uint32(frequency))
	written, err := writer.Write(event[:])
	if err != nil {
		return err
	}
	if written != len(event) {
		return io.ErrShortWrite
	}
	return nil
}

func flashGPIOLED(port gpioPortReadWriter, count int, sleep func(time.Duration)) (err error) {
	if count <= 0 {
		return nil
	}
	var original [1]byte
	if _, err = port.ReadAt(original[:], gpioLEDPort); err != nil {
		return err
	}
	defer func() {
		restoreErr := writeGPIOPortByte(port, gpioLEDPort, original[0])
		err = errors.Join(err, restoreErr)
	}()
	for i := 0; i < count; i++ {
		if err = writeGPIOPortByte(port, gpioLEDPort, original[0]|(1<<gpioLEDBit)); err != nil {
			return err
		}
		sleep(gpioFlashDuration)
		if err = writeGPIOPortByte(port, gpioLEDPort, original[0]&^(1<<gpioLEDBit)); err != nil {
			return err
		}
		if i+1 < count {
			sleep(gpioFlashDuration)
		}
	}
	return nil
}

func writeGPIOPortByte(port io.WriterAt, offset int64, value byte) error {
	written, err := port.WriteAt([]byte{value}, offset)
	if err != nil {
		return err
	}
	if written != 1 {
		return io.ErrShortWrite
	}
	return nil
}
