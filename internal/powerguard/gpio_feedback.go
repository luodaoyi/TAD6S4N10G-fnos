package powerguard

import "errors"

type gpioToneOutput interface {
	PlayTones(count int) error
}

type gpioLEDOutput interface {
	Flash(count int) error
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
