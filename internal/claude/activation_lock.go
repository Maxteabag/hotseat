package claude

import "time"

// ActivationLockError is a bounded, content-free refusal, not a credential error.
type ActivationLockError struct{ Code string }

func (e *ActivationLockError) Error() string { return "Account activation refused: " + e.Code }

const activationWait = 5 * time.Second
