package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// ErrBotNameTaken reports a name the Project already registers, surfaced as a
// typed error rather than as a driver-shaped constraint violation.
var ErrBotNameTaken = errors.New("pocketbase: the project already registers this bot name")

// botColumns is what every read selects, in the order the scan reads them.
const botColumns = "id, name, state, version"

const (
	createBot = "INSERT INTO bots (project_id, id, name, state, created_at, updated_at, revoked_at, version)" +
		" VALUES (?, ?, ?, ?, ?, ?, NULL, 1)" +
		" ON CONFLICT (project_id, name) DO NOTHING" +
		" RETURNING version"

	readBot = "SELECT " + botColumns + " FROM bots WHERE project_id = ? AND id = ?"

	updateBotState = "UPDATE bots SET state = ?, updated_at = ?," +
		" revoked_at = CASE WHEN ? = 'revoked' THEN ? ELSE revoked_at END," +
		" version = version + 1" +
		" WHERE project_id = ? AND id = ? AND version = ?" +
		" RETURNING version"
)

// BotVersion is the optimistic-concurrency counter bots.version carries.
type BotVersion int64

// BotStore persists bots. It has no credential method, because a bot holds no
// secret and no table would accept one.
type BotStore struct {
	db *sql.DB
}

// NewBotStore binds a store to an open database. The caller owns the pool and is
// responsible for opening it with foreign keys enforced.
func NewBotStore(db *sql.DB) *BotStore {
	return &BotStore{db: db}
}

// Create registers a bot at version 1. A name already taken inside the Project is
// ErrBotNameTaken and nothing is written.
func (s *BotStore) Create(ctx context.Context, bot project.Bot) (BotVersion, error) {
	if bot.State() != project.ServiceActive {
		return 0, fmt.Errorf("%w: %s is %s", ErrRegistrationNotActive, bot.ID(), bot.State())
	}

	stamp := time.Now().UTC().UnixMilli()

	var version int64

	err := conn(ctx, s.db).QueryRowContext(ctx, createBot,
		string(bot.Project()), string(bot.ID()), bot.Name(), string(bot.State()), stamp, stamp,
	).Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %q in %s", ErrBotNameTaken, bot.Name(), bot.Project())
	case err != nil:
		return 0, fmt.Errorf("pocketbase: create bot: %w", err)
	}

	return BotVersion(version), nil
}

// ByID reads one bot inside one Project. An absent row is a clean miss, so a
// caller cannot carry a zero value onward as though it named one.
func (s *BotStore) ByID(
	ctx context.Context, proj project.ID, id project.BotID,
) (project.Bot, BotVersion, bool, error) {
	if err := project.ValidateID(proj); err != nil {
		return project.Bot{}, 0, false, err
	}

	if err := project.ValidateBotID(id); err != nil {
		return project.Bot{}, 0, false, err
	}

	var (
		scannedID, name, state string
		version                int64
	)

	err := conn(ctx, s.db).QueryRowContext(ctx, readBot, string(proj), string(id)).Scan(
		&scannedID, &name, &state, &version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return project.Bot{}, 0, false, nil
	case err != nil:
		return project.Bot{}, 0, false, fmt.Errorf("pocketbase: read bot: %w", err)
	}

	bot, err := project.NewBot(proj, project.BotConfig{
		ID: project.BotID(scannedID), Name: name, State: project.ServiceState(state),
	})
	if err != nil {
		return project.Bot{}, 0, false, fmt.Errorf("pocketbase: rebuild bot %s in %s: %w", scannedID, proj, err)
	}

	return bot, BotVersion(version), true, nil
}

// UpdateState moves a bot under the version the caller last read. It reads first
// so the lifecycle runs against the persisted state, and carries every
// precondition into the statement so the decision cannot go stale.
func (s *BotStore) UpdateState(
	ctx context.Context,
	proj project.ID,
	id project.BotID,
	next project.ServiceState,
	expect BotVersion,
) (BotVersion, error) {
	bot, version, found, err := s.ByID(ctx, proj, id)

	switch {
	case err != nil:
		return 0, err
	case !found:
		return 0, fmt.Errorf("%w: bot %s", storage.ErrNotFound, id)
	case version != expect:
		return 0, fmt.Errorf("%w: bot %s stands at version %d", storage.ErrVersionConflict, id, version)
	}

	if _, err := bot.TransitionTo(next); err != nil {
		return 0, err
	}

	stamp := time.Now().UTC().UnixMilli()

	var written int64

	err = conn(ctx, s.db).QueryRowContext(ctx, updateBotState,
		string(next), stamp, string(next), stamp, string(proj), string(id), int64(expect),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: bot %s moved since it was read", storage.ErrVersionConflict, id)
	case err != nil:
		return 0, fmt.Errorf("pocketbase: update bot state: %w", err)
	}

	return BotVersion(written), nil
}
