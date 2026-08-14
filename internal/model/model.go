// Package model holds the domain types shared across handlers and storage.
//
// Types here describe the domain, not the wire format: request and response
// shapes belong next to their handlers so that changing an API payload never
// forces a change to the domain model.
//
// It holds the domain types — User, Bet, PaymentMethod — plus the small
// amount of storage logic that would otherwise be duplicated across handlers.
package model
