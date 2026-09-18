package main

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// sealingKeyVariable names the key this deployment seals stored secrets with.
const sealingKeyVariable = "ILAVRITA_SEALING_KEY"

// sealingKey reads the configured key once.
//
// A deployment that configured none gets the zero key, which seals nothing and
// opens nothing: second factors cannot be enrolled, and a login requires none.
// That is refused rather than defaulted, because a key this server chose for
// itself would be one an attacker can choose too — and storing the secrets in
// the clear instead would make the database a list of everyone's second factor.
var sealingKey = sync.OnceValue(func() project.SealingKey {
	configured := strings.TrimSpace(os.Getenv(sealingKeyVariable))
	if configured == "" {
		log.Printf("WARNING: %s is unset, so second factors cannot be enrolled", sealingKeyVariable)

		return project.SealingKey{}
	}

	key, err := project.ParseSealingKey(configured)
	if err != nil {
		log.Printf("WARNING: %s is not a usable key (%v), so second factors cannot be enrolled",
			sealingKeyVariable, err)

		return project.SealingKey{}
	}

	return key
})

// attemptKeys derives what the login throttle names attempts under, from the
// same configured secret rather than from a second one for an operator to lose.
//
// A deployment that configured none names attempts under a key of zeroes. The
// throttle still works; what stops working is the claim that its table hides
// anything, because an email address and an IPv4 address are both small enough
// to walk a digest back to.
var attemptKeys = sync.OnceValue(func() project.AttemptKeys {
	names := project.NewAttemptKeys(sealingKey())
	if !names.Keyed() {
		log.Printf("WARNING: %s is unset, so failed logins name their addresses recoverably",
			sealingKeyVariable)
	}

	return names
})
