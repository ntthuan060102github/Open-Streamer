package domain

import "time"

// ErrorEntry is a single recorded error with the time it occurred.
// Used by manager (per input) and transcoder (per profile) to expose a
// short rolling history of errors via their RuntimeStatus APIs.
type ErrorEntry struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}
