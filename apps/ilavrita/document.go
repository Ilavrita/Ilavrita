package main

import (
	"encoding/json"
	"errors"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// documentType is the type that points at clinical documents. It is named here
// by name rather than by a general rule, the same way Binary is, because nothing
// else this build serves carries an attachment.
const documentType storage.ResourceType = "DocumentReference"

// errInlineAttachment reports a DocumentReference submitted with bytes inside
// it. It names Binary because that is where they go.
var errInlineAttachment = errors.New(
	"ilavrita: a document's bytes are stored as a Binary and referenced, never inlined")

// refuseInlineAttachment refuses a DocumentReference carrying its own bytes.
//
// A document belongs outside the row for the same reason a Binary's payload
// does: megabytes in the row make every read of the metadata pay for them and
// every backup of the database carry them. The difference is where they go. A
// Binary is a payload with a resource describing it, so this server splits the
// two itself. A DocumentReference is a description of a document that already
// exists somewhere — R4 gives its attachment a url for exactly that — so the
// bytes are posted as a Binary and the reference names it.
//
// Accepting the data member and quietly storing it in the row would store what
// the rest of this build refuses to, and accepting it and splitting it out would
// be a second payload mechanism answering the question the first one answers.
func refuseInlineAttachment(key storage.ResourceKey, content json.RawMessage) error {
	if key.Type != documentType {
		return nil
	}

	var held struct {
		Content []struct {
			Attachment struct {
				Data json.RawMessage `json:"data"`
			} `json:"attachment"`
		} `json:"content"`
	}

	// A body this cannot read is one the write path refuses for itself; what is
	// asked here is only whether bytes were inlined.
	if err := json.Unmarshal(content, &held); err != nil {
		return nil
	}

	for _, entry := range held.Content {
		if len(entry.Attachment.Data) > 0 && string(entry.Attachment.Data) != "null" {
			return errInlineAttachment
		}
	}

	return nil
}
