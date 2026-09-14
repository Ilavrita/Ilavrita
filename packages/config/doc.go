// Package config resolves runtime settings from the environment and an optional
// configuration file.
//
// Settings live at the edge of the system so the rest of the code receives
// values rather than reading the environment itself. Secrets are never written
// to source-controlled defaults (FR-040).
package config
