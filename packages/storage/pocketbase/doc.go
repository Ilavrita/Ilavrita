// Package pocketbase will implement the storage interfaces on top of the
// Ilavrita PocketBase fork and SQLite.
//
// This is the only package permitted to import the PocketBase runtime. Keeping
// SQLite-specific SQL and PocketBase collection access confined here is what
// keeps the rest of the system portable (FR-025, NFR-002).
package pocketbase
