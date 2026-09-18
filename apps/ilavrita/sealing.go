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
