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
