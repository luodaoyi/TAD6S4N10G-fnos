//go:build !linux || !amd64

package powerguard

import "fmt"

// PlayGPIOFeedback is unavailable outside the module's supported linux/amd64
// runtime. It remains best effort and must not prevent the business action.
func (m *Manager) PlayGPIOFeedback(event GPIOEvent) error {
	if !event.IsFeedback() {
		return nil
	}
	return fmt.Errorf("%w: PC speaker and /dev/port feedback require linux/amd64", ErrGPIOFeedbackUnavailable)
}

// PlayStorageChime is unavailable outside linux/amd64 for the same reason as
// PlayGPIOFeedback: it drives the PC speaker through /dev/input.
func (m *Manager) PlayStorageChime(tones []GPIOToneSpec) error {
	if len(tones) == 0 {
		return nil
	}
	return fmt.Errorf("%w: PC speaker feedback requires linux/amd64", ErrGPIOFeedbackUnavailable)
}
