package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// This server has no PocketBase superuser, and cannot be made to have one.
//
// A superuser is not one of this server's principals. It holds no Project, no
// Membership and no Grant, so nothing it does is narrowed by a compartment, a
// filter or a projection, and nothing it does reaches the audit trail. The
// console it unlocks can take a backup of the whole data directory, and that
// archive carries every Project's clinical record out with it.
//
// Being an administrator of this install is Super Admin, which is a standing
// inside the model — decided per request, recorded like any other. There is no
// second way in beside it.
//
// Two things hold that. The command that mints a superuser is never registered,
// which is why main calls Execute rather than Start. And any row that reached
// the table some other way — an older build, a second PocketBase binary run
// against this directory, a restored archive — is removed before this server
// answers anything.
const superuserTable = core.CollectionNameSuperusers

// refuseSuperusers removes every PocketBase superuser this install holds.
//
// It runs on every start rather than once, because the table is reachable by
// things that are not this process: the guarantee is "none while this server is
// running", and only re-checking makes that true. An install that never had one
// is the ordinary case and records nothing.
func refuseSuperusers(ctx context.Context, runtime dbx.Builder, records *sql.DB) error {
	_, err := sqlite.Perform(ctx, records, time.Now(), sqlite.SuperJob{
		Name:    "enforce.superusers.none",
		Kind:    sqlite.JobMigration,
		Subject: superuserTable,
	}, func(context.Context) (sqlite.Done, error) {
		// Deleted in one statement rather than through the record API, which
		// refuses to remove the last superuser — a rule that protects an
		// ordinary PocketBase install and would leave exactly one here.
		result, err := runtime.NewQuery("DELETE FROM {{" + superuserTable + "}}").Execute()
		if err != nil {
			return sqlite.Done{}, fmt.Errorf("ilavrita: remove the superusers: %w", err)
		}

		removed, err := result.RowsAffected()
		if err != nil {
			return sqlite.Done{}, fmt.Errorf("ilavrita: count the superusers removed: %w", err)
		}

		if removed == 0 {
			return sqlite.Done{}, nil
		}

		// Loud, because a credential existed on this install and somebody made
		// it. Whoever did should know it is gone rather than wonder later why
		// it stopped working.
		log.Printf("WARNING: removed %d PocketBase superuser(s); this server has no such account", removed)

		return sqlite.Done{
			Changed: true,
			Detail:  fmt.Sprintf("removed %d superuser(s)", removed),
		}, nil
	})

	return err
}
