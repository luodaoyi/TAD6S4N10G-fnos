package powerguard

import "errors"

type gpioToneOutput interface {
	PlayTones(count int) error
}

type gpioLEDOutput interface {
	Flash(count int) error
}

// GPIOToneSpec 描述一声蜂鸣：频率（Hz）与持续时长（ms）。按键确认反馈是固定的
// 「滴滴滴 3 声」（440 Hz / 200 ms），存储设备插入提示音则要靠不同的节奏区分
// 事件类型，所以单独给一套可自由编排的接口。
type GPIOToneSpec struct {
	FrequencyHz int
	DurationMS  int
}

// gpioChimeOutput 播放一段自定义提示音。与按键确认反馈的 gpioToneOutput 分开，
// 免得为了存储提示去改动既有「滴滴滴 3 声」的时序与参数。
type gpioChimeOutput interface {
	PlayChime(tones []GPIOToneSpec) error
}

// runGPIOFeedback deliberately attempts both outputs. Confirmation feedback is
// best effort, so callers must not use its error to suppress a button action.
func runGPIOFeedback(pattern GPIOFeedbackPattern, tone gpioToneOutput, led gpioLEDOutput) error {
	var errs []error
	if tone == nil {
		errs = append(errs, ErrGPIOFeedbackUnavailable)
	} else if err := tone.PlayTones(pattern.Tones); err != nil {
		errs = append(errs, err)
	}
	if led == nil {
		errs = append(errs, ErrGPIOFeedbackUnavailable)
	} else if err := led.Flash(pattern.Flashes); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
